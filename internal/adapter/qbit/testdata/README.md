# qBittorrent adapter fixtures

`contract/*.json` are replayed by `contract_test.go` through `internal/adapter/adaptertest`
(format: see that package's doc comment). `contract/files/` holds the `.torrent` that the upload
fixture sends. Logins capture the `SID` cookie with `header:Set-Cookie:SID=([^;]+)`.

**These fixtures are hand-written from the qBittorrent WebUI API documentation and from what
Sonarr and Radarr are known to parse. They are NOT recorded traffic.** Every file says so in its
`provenance` field ("NOT recorded"); a test insists that a fixture says either that or
"recorded from <versions>".

M5-09 (record real Sonarr/Radarr/Prowlarr <-> qBittorrent traffic) was dropped. When real use
shows a difference, fix the adapter and, where useful, replace the fixture with a recording:

1. scrub passwords, SIDs, hashes, host names and paths into `{{pass}}`, `{{sid}}`, `{{hash.x}}`,
   `{{torrent_dir}}` and similar;
2. change its `provenance` to the recorded versions (and note them here);
3. mark the matchers of volatile values (times, speeds, ids) with `$type:`, `$re:` or `$any`;
4. fix the adapter where the recording differs. The recording is the specification.

## The import hold-off question (still open, checked by real use)

Which `state`/`progress` pairs make Sonarr and Radarr wait for an import is **not answered by any
recording yet**. The adapter assumes (from Sonarr's source as remembered) that `checkingUP` is
shown as queued, and keeps `progress` at 0.9999 as a second guard, for a Download that is Fetched
or Post-processing. `torrents_info_import_holdoff.json` encodes that assumption.

Media-manager versions recorded: none yet.
