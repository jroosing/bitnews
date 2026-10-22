# SABnzbd adapter fixtures

`contract/*.json` are replayed by `contract_test.go` through `internal/adapter/adaptertest`
(format: see that package's doc comment). `contract/files/` holds the NZBs that multipart
and `addurl` fixtures send or serve.

**These fixtures are hand-written from the SABnzbd API documentation and from what Sonarr and
Radarr are known to parse. They are NOT recorded traffic.** Every file says so in its
`provenance` field ("NOT recorded"); a test insists that a fixture says either that or
"recorded from <versions>".

M4-01 (record real Sonarr/Radarr/Prowlarr <-> SABnzbd traffic) replaces them. When a fixture is
replaced by a recording:

1. scrub API keys, host names and paths into `{{apikey}}`, `{{complete_dir}}` and similar;
2. change its `provenance` to the recorded versions (and note them here);
3. mark the matchers of volatile values (times, speeds, ids) with `$type:`, `$re:` or `$any`;
4. fix the adapter where the recording differs. The recording is the specification.

Media-manager versions recorded: none yet.
