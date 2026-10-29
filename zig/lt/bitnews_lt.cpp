#include "bitnews_lt.h"

#include <libtorrent/add_torrent_params.hpp>
#include <libtorrent/alert.hpp>
#include <libtorrent/alert_types.hpp>
#include <libtorrent/magnet_uri.hpp>
#include <libtorrent/session.hpp>
#include <libtorrent/settings_pack.hpp>
#include <libtorrent/torrent_handle.hpp>
#include <libtorrent/torrent_status.hpp>
#include <libtorrent/version.hpp>

#include <cstring>
#include <deque>
#include <exception>
#include <string>
#include <vector>

namespace lt = libtorrent;

struct bnlt_session {
    lt::session ses;
    std::deque<std::string> alerts; // messages popped from libtorrent, not yet read

    explicit bnlt_session(lt::settings_pack p) : ses(std::move(p)) {}
};

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

int bnlt_status_first(bnlt_session *s, bnlt_status *out) {
    std::vector<lt::torrent_handle> all = s->ses.get_torrents();
    if (all.empty()) return 1;
    lt::torrent_status st = all[0].status();
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
    return 0;
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
