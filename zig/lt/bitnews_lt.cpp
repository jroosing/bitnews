#include "bitnews_lt.h"

#include <libtorrent/add_torrent_params.hpp>
#include <libtorrent/address.hpp>
#include <libtorrent/alert.hpp>
#include <libtorrent/alert_types.hpp>
#include <libtorrent/bencode.hpp>
#include <libtorrent/create_torrent.hpp>
#include <libtorrent/error_code.hpp>
#include <libtorrent/load_torrent.hpp>
#include <libtorrent/magnet_uri.hpp>
#include <libtorrent/session.hpp>
#include <libtorrent/settings_pack.hpp>
#include <libtorrent/sha1_hash.hpp>
#include <libtorrent/socket.hpp>
#include <libtorrent/torrent_handle.hpp>
#include <libtorrent/torrent_status.hpp>
#include <libtorrent/version.hpp>
#if LIBTORRENT_VERSION_MAJOR > 2 || (LIBTORRENT_VERSION_MAJOR == 2 && LIBTORRENT_VERSION_MINOR >= 1)
#include <libtorrent/write_resume_data.hpp>
#include <memory>
#endif

#include <cerrno>
#include <chrono>
#include <cstdint>
#include <cstdlib>
#include <cstring>
#include <deque>
#include <exception>
#include <iterator>
#include <string>
#include <vector>

namespace lt = libtorrent;

struct bnlt_session {
    lt::session ses;
    std::deque<std::string> alerts; // messages popped from libtorrent, not yet read

    explicit bnlt_session(lt::settings_pack p) : ses(std::move(p)) {}
};

namespace {

// Same ceiling as load_torrent_limits::max_buffer_size. Larger buffers are
// rejected before they reach the parser.
constexpr std::size_t k_max_torrent_bytes = 10000000;

void fill_status(lt::torrent_status const &st, bnlt_status *out) {
    std::memset(out, 0, sizeof *out);
    out->has_metadata = st.has_metadata ? 1 : 0;
    out->state = static_cast<int32_t>(st.state);
    out->num_peers = st.num_peers;
    out->num_seeds = st.num_seeds;
    out->total_done = st.total_done;
    out->total_wanted = st.total_wanted;
    out->download_rate = st.download_payload_rate;
    out->upload_rate = st.upload_payload_rate;
    std::strncpy(out->name, st.name.c_str(), sizeof out->name - 1);
}

lt::torrent_handle find_v1(bnlt_session *s, uint8_t const info_hash[20]) {
    // sha1_hash is the find_torrent argument both 2.0.11 and 2.1 expose.
    // Construct from the raw 20 bytes; a C string would stop at an embedded NUL.
    lt::sha1_hash const hash(reinterpret_cast<char const *>(info_hash));
    return s->ses.find_torrent(hash);
}

// "ip:port" or "[ipv6]:port". Returns 0 and fills ep, or 1 when addr is unusable.
int parse_peer(char const *addr, lt::tcp::endpoint &ep) {
    if (addr == nullptr) return 1;
    std::size_t const n = std::strlen(addr);
    if (n == 0 || n > 128) return 1;

    std::string host;
    char const *port_s = nullptr;
    if (addr[0] == '[') {
        char const *br = std::strchr(addr, ']');
        if (br == nullptr || br == addr + 1 || br[1] != ':') return 1;
        host.assign(addr + 1, br);
        port_s = br + 2;
    } else {
        char const *colon = std::strrchr(addr, ':');
        if (colon == nullptr || colon == addr || colon[1] == '\0') return 1;
        if (std::strchr(addr, ':') != colon) return 1;
        host.assign(addr, colon);
        port_s = colon + 1;
    }
    if (host.empty() || port_s[0] < '0' || port_s[0] > '9') return 1;

    errno = 0;
    char *end = nullptr;
    long const port = std::strtol(port_s, &end, 10);
    if (end == port_s || *end != '\0' || errno != 0 || port < 0 || port > 65535) return 1;

    lt::error_code ec;
    lt::address const ip = lt::make_address(host, ec);
    if (ec) return 1;
    ep = lt::tcp::endpoint(ip, static_cast<std::uint16_t>(port));
    return 0;
}

// 0 = parsed, 1 = bad torrent bytes. Unexpected failures propagate.
int load_torrent_bytes(void const *bytes, std::size_t len, lt::add_torrent_params &params) {
    lt::span<char const> const buf(static_cast<char const *>(bytes), static_cast<std::ptrdiff_t>(len));
#if LIBTORRENT_VERSION_MAJOR > 2 || (LIBTORRENT_VERSION_MAJOR == 2 && LIBTORRENT_VERSION_MINOR >= 1)
    // 2.1 reports parse failures through error_code. 2.0.11 only has the throwing overload.
    lt::error_code ec;
    params = lt::load_torrent_buffer(buf, ec, lt::load_torrent_limits{});
    if (ec) return 1;
#else
    try {
        params = lt::load_torrent_buffer(buf);
    } catch (const lt::system_error &) {
        return 1;
    }
#endif
    if (!params.ti || !params.ti->info_hashes().has_v1()) return 1;
    return 0;
}

} // namespace

extern "C" {

const char *bnlt_version(void) { return lt::version(); }

bnlt_session *bnlt_new(const char *listen_interfaces, int enable_dht) {
    try {
        lt::settings_pack p;
        p.set_str(lt::settings_pack::listen_interfaces, listen_interfaces);
        p.set_bool(lt::settings_pack::enable_dht, enable_dht != 0);
        p.set_bool(lt::settings_pack::enable_lsd, false);
        p.set_int(lt::settings_pack::alert_mask,
                  lt::alert_category::error | lt::alert_category::status | lt::alert_category::peer |
                      lt::alert_category::storage);
        return new bnlt_session(std::move(p));
    } catch (const std::exception &) {
        return nullptr;
    }
}

void bnlt_free(bnlt_session *s) { delete s; }

int bnlt_add_magnet(bnlt_session *s, const char *uri, const char *save_path) {
    try {
        lt::error_code ec;
        lt::add_torrent_params params = lt::parse_magnet_uri(uri, ec);
        if (ec) return 1;
        params.save_path = save_path;
        s->ses.add_torrent(std::move(params));
        return 0;
    } catch (const std::exception &) {
        return 2;
    }
}

int bnlt_add_torrent(bnlt_session *s, const void *bytes, size_t len, const char *save_path,
                     uint8_t info_hash_out[20]) {
    if (bytes == nullptr || save_path == nullptr || save_path[0] == '\0') return 1;
    if (len == 0 || len > k_max_torrent_bytes) return 1;
    try {
        lt::add_torrent_params params;
        int const loaded = load_torrent_bytes(bytes, len, params);
        if (loaded != 0) return loaded;
        lt::sha1_hash const v1 = params.ti->info_hashes().v1;
        params.save_path = save_path;
        s->ses.add_torrent(std::move(params));
        if (info_hash_out != nullptr) std::memcpy(info_hash_out, v1.data(), 20);
        return 0;
    } catch (const std::exception &) {
        return 2;
    }
}

int bnlt_listen_port(bnlt_session *s) {
    try {
        return static_cast<int>(s->ses.listen_port());
    } catch (const std::exception &) {
        return 0;
    }
}

int bnlt_status_first(bnlt_session *s, bnlt_status *out) {
    std::vector<lt::torrent_handle> all = s->ses.get_torrents();
    if (all.empty()) return 1;
    fill_status(all[0].status(), out);
    return 0;
}

int bnlt_status_hash(bnlt_session *s, const uint8_t info_hash[20], bnlt_status *out) {
    if (info_hash == nullptr || out == nullptr) return 1;
    try {
        lt::torrent_handle h = find_v1(s, info_hash);
        if (!h.is_valid()) return 1;
        fill_status(h.status(), out);
        return 0;
    } catch (const std::exception &) {
        return 2;
    }
}

int bnlt_pause(bnlt_session *s, const uint8_t info_hash[20]) {
    if (info_hash == nullptr) return 1;
    try {
        lt::torrent_handle h = find_v1(s, info_hash);
        if (!h.is_valid()) return 1;
        h.pause();
        return 0;
    } catch (const std::exception &) {
        return 2;
    }
}

int bnlt_resume(bnlt_session *s, const uint8_t info_hash[20]) {
    if (info_hash == nullptr) return 1;
    try {
        lt::torrent_handle h = find_v1(s, info_hash);
        if (!h.is_valid()) return 1;
        h.resume();
        return 0;
    } catch (const std::exception &) {
        return 2;
    }
}

int bnlt_remove(bnlt_session *s, const uint8_t info_hash[20], int delete_files) {
    if (info_hash == nullptr) return 1;
    try {
        lt::torrent_handle h = find_v1(s, info_hash);
        if (!h.is_valid()) return 1;
        // delete_partfile is the shared name (2.0.11 and 2.1). No delete_part symbol.
        auto const flags =
            delete_files != 0 ? lt::session::delete_files : lt::session::delete_partfile;
        s->ses.remove_torrent(h, flags);
        return 0;
    } catch (const std::exception &) {
        return 2;
    }
}

int bnlt_move_storage(bnlt_session *s, const uint8_t info_hash[20], const char *path) {
    if (s == nullptr || info_hash == nullptr || path == nullptr || path[0] == '\0') return 1;
    try {
        lt::torrent_handle h = find_v1(s, info_hash);
        if (!h.is_valid()) return 1;
        h.move_storage(path);
        auto const deadline = std::chrono::steady_clock::now() + std::chrono::seconds(30);
        while (std::chrono::steady_clock::now() < deadline) {
            s->ses.wait_for_alert(std::chrono::milliseconds(200));
            std::vector<lt::alert *> alerts;
            s->ses.pop_alerts(&alerts);
            for (lt::alert *a : alerts) {
                if (auto *ok = lt::alert_cast<lt::storage_moved_alert>(a)) {
                    if (ok->handle == h) return 0;
                }
                if (auto *bad = lt::alert_cast<lt::storage_moved_failed_alert>(a)) {
                    if (bad->handle == h) return 2;
                }
            }
        }
        return 2;
    } catch (const std::exception &) {
        return 2;
    }
}

int bnlt_read_torrent(bnlt_session *s, const uint8_t info_hash[20], void *buf, size_t cap, size_t *out_len) {
    if (s == nullptr || info_hash == nullptr) return 1;
    try {
        lt::torrent_handle h = find_v1(s, info_hash);
        if (!h.is_valid()) return 1;
        std::shared_ptr<const lt::torrent_info> ti = h.torrent_file();
        if (!ti) return 1;
#if LIBTORRENT_VERSION_MAJOR > 2 || (LIBTORRENT_VERSION_MAJOR == 2 && LIBTORRENT_VERSION_MINOR >= 1)
        // 2.1 deprecates create_torrent(torrent_info). write_torrent_file needs
        // the info dict on add_torrent_params.
        lt::add_torrent_params atp;
        atp.ti = std::make_shared<lt::torrent_info>(*ti);
        std::vector<char> raw = lt::write_torrent_file_buf(atp, {});
#else
        lt::create_torrent ct(*ti);
        std::vector<char> raw;
        lt::bencode(std::back_inserter(raw), ct.generate());
#endif
        if (out_len != nullptr) *out_len = raw.size();
        if (buf == nullptr || raw.size() > cap) return 3;
        if (!raw.empty()) std::memcpy(buf, raw.data(), raw.size());
        return 0;
    } catch (const std::exception &) {
        return 2;
    }
}

int bnlt_add_peer(bnlt_session *s, const uint8_t info_hash[20], const char *addr) {
    if (info_hash == nullptr) return 1;
    lt::tcp::endpoint ep;
    if (parse_peer(addr, ep) != 0) return 1;
    try {
        lt::torrent_handle h = find_v1(s, info_hash);
        if (!h.is_valid()) return 1;
        h.connect_peer(ep);
        return 0;
    } catch (const std::exception &) {
        return 2;
    }
}

int bnlt_pop_alert(bnlt_session *s, char *buf, size_t len) {
    if (s->alerts.empty()) {
        std::vector<lt::alert *> batch;
        s->ses.pop_alerts(&batch);
        for (lt::alert *a : batch) s->alerts.push_back(a->message());
    }
    if (s->alerts.empty()) return 0;
    std::strncpy(buf, s->alerts.front().c_str(), len - 1);
    buf[len - 1] = '\0';
    s->alerts.pop_front();
    return 1;
}

} // extern "C"
