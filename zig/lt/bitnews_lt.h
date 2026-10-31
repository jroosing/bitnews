/* libbitnews_lt: the C face of libtorrent-rasterbar for bitnews (M15, ADR-0014).
 * Built with the system C++ compiler against the system libtorrent; Zig only
 * ever sees these C declarations. The Z0 spike uses the first few calls; the
 * real wrapper (Z7) grows to about 15 functions. */
#ifndef BITNEWS_LT_H
#define BITNEWS_LT_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

typedef struct bnlt_session bnlt_session;

typedef struct {
    int32_t has_metadata; /* 1 once the torrent's metadata is known */
    int32_t state;        /* libtorrent's torrent_status::state_t */
    int32_t num_peers;
    int32_t num_seeds;
    int64_t total_done;
    int64_t total_wanted;
    int32_t download_rate;
    int32_t upload_rate;
    char name[256];
} bnlt_status;

/* The libtorrent version string, e.g. "2.0.11.0". */
const char *bnlt_version(void);

/* listen_interfaces is libtorrent's setting, e.g. "127.0.0.1:0" or "0.0.0.0:6881".
 * Returns NULL on failure. */
bnlt_session *bnlt_new(const char *listen_interfaces, int enable_dht);
void bnlt_free(bnlt_session *s);

/* Adds a magnet link, downloading into save_path. Returns 0 on success. */
int bnlt_add_magnet(bnlt_session *s, const char *uri, const char *save_path);

/* Adds a .torrent file. info_hash_out may be NULL; otherwise it receives the
 * 20-byte v1 SHA-1 info-hash. Returns 0 on success. */
int bnlt_add_torrent(bnlt_session *s, const void *bytes, size_t len, const char *save_path,
                     uint8_t info_hash_out[20]);

/* UDP/TCP listen port the session bound, or 0 when it is not known yet. */
int bnlt_listen_port(bnlt_session *s);

/* Status of the first torrent; returns 0 on success, 1 when there is none. */
int bnlt_status_first(bnlt_session *s, bnlt_status *out);

/* Status of the torrent with this 20-byte v1 info-hash.
 * Returns 0 on success, 1 when it is not in the session.
 * Named apart from the bnlt_status struct: C has one namespace for both. */
int bnlt_status_hash(bnlt_session *s, const uint8_t info_hash[20], bnlt_status *out);

/* Pause, resume, remove, or move that torrent. remove with delete_files != 0 also
 * deletes the downloaded files. move_storage waits until libtorrent reports the
 * move (or failure). Returns 0 on success, 1 when not found, 2 on failure. */
int bnlt_pause(bnlt_session *s, const uint8_t info_hash[20]);
int bnlt_resume(bnlt_session *s, const uint8_t info_hash[20]);
int bnlt_remove(bnlt_session *s, const uint8_t info_hash[20], int delete_files);
int bnlt_move_storage(bnlt_session *s, const uint8_t info_hash[20], const char *path);

/* Copies the .torrent bytes of a torrent that already has metadata.
 * Returns 0 on success, 1 when metadata is not ready or the torrent is absent,
 * 2 on failure, 3 when `cap` is too small (`*out_len` is the size needed). */
int bnlt_read_torrent(bnlt_session *s, const uint8_t info_hash[20], void *buf, size_t cap, size_t *out_len);

/* addr is "ip:port". Returns 0 on success, 1 when the torrent or address is bad. */
int bnlt_add_peer(bnlt_session *s, const uint8_t info_hash[20], const char *addr);

/* Writes the next alert's message into buf (NUL terminated); returns 1 when
 * there was one, 0 when the queue is empty. */
int bnlt_pop_alert(bnlt_session *s, char *buf, size_t len);

#ifdef __cplusplus
}
#endif
#endif
