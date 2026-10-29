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

/* Status of the first torrent; returns 0 on success, 1 when there is none. */
int bnlt_status_first(bnlt_session *s, bnlt_status *out);

/* Writes the next alert's message into buf (NUL terminated); returns 1 when
 * there was one, 0 when the queue is empty. */
int bnlt_pop_alert(bnlt_session *s, char *buf, size_t len);

#ifdef __cplusplus
}
#endif
#endif
