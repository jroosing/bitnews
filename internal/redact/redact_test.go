package redact_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"gitlab.com/kyura4k/gitnews/internal/redact"
)

func TestURLMasksAPIKeyQueryValue(t *testing.T) {
	got := redact.URL("https://indexer.example/api?t=search&apikey=0123456789abcdef&q=ubuntu")
	want := "https://indexer.example/api?t=search&apikey=REDACTED&q=ubuntu"
	assert.Equal(t, want, got)
}

func TestURLMasksEverySecretParamCaseInsensitively(t *testing.T) {
	tests := []struct{ in, want string }{
		{"http://h/?apikey=s", "http://h/?apikey=REDACTED"},
		{"http://h/?api_key=s", "http://h/?api_key=REDACTED"},
		{"http://h/?key=s", "http://h/?key=REDACTED"},
		{"http://h/?token=s", "http://h/?token=REDACTED"},
		{"http://h/?password=s", "http://h/?password=REDACTED"},
		{"http://h/?pass=s", "http://h/?pass=REDACTED"},
		{"http://h/?nzbkey=s", "http://h/?nzbkey=REDACTED"},
		{"http://h/?ApiKey=s", "http://h/?ApiKey=REDACTED"},
		{"http://h/?TOKEN=s", "http://h/?TOKEN=REDACTED"},
		{"http://h/?api%5Fkey=s", "http://h/?api%5Fkey=REDACTED"},
		{"http://h/?apikey=a&apikey=b", "http://h/?apikey=REDACTED&apikey=REDACTED"},
		{"http://h/?apikey=", "http://h/?apikey=REDACTED"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, redact.URL(tt.in), tt.in)
	}
}

func TestURLKeepsNonSecretParamsAndSimilarNames(t *testing.T) {
	for _, in := range []string{
		"http://h/api?t=caps",
		"http://h/api?keyword=foo&monkey=1&passkey_hint=x",
		"http://h/api?apikey",
		"http://h/api",
	} {
		assert.Equal(t, in, redact.URL(in), "want unchanged")
	}
}

func TestURLMasksUserinfoPasswordButKeepsUsername(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://joey:hunter2@news.example:563/", "https://joey:REDACTED@news.example:563/"},
		{"http://joey:@h/", "http://joey:REDACTED@h/"},
		{"http://joey@h/", "http://joey@h/"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, redact.URL(tt.in), tt.in)
	}
}

func TestURLReturnsPlaceholderForMalformedInput(t *testing.T) {
	for _, in := range []string{
		"http://[::1/api?apikey=topsecret",
		"http://h:port/api?apikey=topsecret",
		"http://user:topsecret@h:x/",
		"%zz?apikey=topsecret",
		"http://h/\x7f?apikey=topsecret",
	} {
		assert.Equal(t, redact.Invalid, redact.URL(in), in)
	}
}

// Shapes of URLs that real indexers, indexer proxies and download clients
// hand out. All keys are synthetic.
func TestURLRedactsRealWorldIndexerURLs(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{
			"newznab search",
			"https://api.nzbgeek.info/api?t=search&q=ubuntu&cat=4000&apikey=5f3c9a1b2d4e6f708192a3b4c5d6e7f8&extended=1",
			"https://api.nzbgeek.info/api?t=search&q=ubuntu&cat=4000&apikey=REDACTED&extended=1",
		},
		{
			"newznab get",
			"https://indexer.example/api?t=get&id=0a1b2c3d4e5f&apikey=ABCDEF0123456789ABCDEF0123456789",
			"https://indexer.example/api?t=get&id=0a1b2c3d4e5f&apikey=REDACTED",
		},
		{
			"nzbhydra2 proxied download",
			"http://nzbhydra:5076/getnzb/api/7365492846927364?apikey=hydraSyntheticKey0001",
			"http://nzbhydra:5076/getnzb/api/7365492846927364?apikey=REDACTED",
		},
		{
			"prowlarr download link",
			"http://prowlarr:9696/3/download?apikey=0123456789abcdef0123456789abcdef&link=aHR0cHM6Ly9leGFtcGxl&file=Some.Release.1080p",
			"http://prowlarr:9696/3/download?apikey=REDACTED&link=aHR0cHM6Ly9leGFtcGxl&file=Some.Release.1080p",
		},
		{
			"sabnzbd api with nzb key",
			"http://sabnzbd:8080/sabnzbd/api?mode=addurl&name=https%3A%2F%2Fx&nzbkey=feedfacecafebeef0011223344556677&output=json",
			"http://sabnzbd:8080/sabnzbd/api?mode=addurl&name=https%3A%2F%2Fx&nzbkey=REDACTED&output=json",
		},
		{
			"sonarr style api_key",
			"http://sonarr:8989/api/v3/system/status?api_key=0f1e2d3c4b5a69788796a5b4c3d2e1f0",
			"http://sonarr:8989/api/v3/system/status?api_key=REDACTED",
		},
		{
			"basic auth and token",
			"https://admin:Sup3rS3cret!@indexer.example/rss?token=tok_synthetic_123&dl=1",
			"https://admin:REDACTED@indexer.example/rss?token=REDACTED&dl=1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, redact.URL(tt.in), tt.in)
		})
	}
}

func TestURLTreatsSemicolonAsParamSeparator(t *testing.T) {
	got := redact.URL("http://h/api?t=get;apikey=topsecret&x=1")
	want := "http://h/api?t=get;apikey=REDACTED&x=1"
	assert.Equal(t, want, got)
}

func TestURLMasksNewznabAndTrackerSecretParams(t *testing.T) {
	for _, name := range []string{"r", "passkey", "authkey", "torrent_pass", "rsskey", "apitoken", "access_token", "R", "PassKey"} {
		in := "http://h/rss?i=42&" + name + "=s3cr3t&num=50"
		want := "http://h/rss?i=42&" + name + "=REDACTED&num=50"
		assert.Equal(t, want, redact.URL(in), name)
	}
}

func TestURLMasksSecretParamsInFragment(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://h/cb#access_token=s3cr3t&state=xyz", "https://h/cb#access_token=REDACTED&state=xyz"},
		{"https://h/cb?x=1#token=s3cr3t", "https://h/cb?x=1#token=REDACTED"},
		{"https://h/cb#/route?apikey=s3cr3t", "https://h/cb#/route?apikey=REDACTED"},
		{"https://h/cb#section-2", "https://h/cb#section-2"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, redact.URL(tt.in), tt.in)
	}
}

func TestURLMasksTrackerPasskeysInPath(t *testing.T) {
	const hex32 = "0123456789abcdef0123456789ABCDEF"
	tests := []struct{ name, in, want string }{
		{"hex after announce", "http://t.example/announce/" + hex32 + "/extra", "http://t.example/announce/REDACTED/extra"},
		{"hex before announce", "http://t.example/" + hex32 + "/announce", "http://t.example/REDACTED/announce"},
		{"hex anywhere", "http://t.example/dl/" + hex32 + "/file.torrent", "http://t.example/dl/REDACTED/file.torrent"},
		{"longer hex", "http://t.example/dl/" + hex32 + "ab", "http://t.example/dl/REDACTED"},
		{"token after announce", "http://t.example/announce/Ab3_dE-f6Gh9Ij2Kl5Mn8Pq", "http://t.example/announce/REDACTED"},
		{"token before announce", "http://t.example/Ab3_dE-f6Gh9Ij2Kl5Mn8Pq/announce", "http://t.example/REDACTED/announce"},
		{"token after passkey", "http://t.example/passkey/Ab3_dE-f6Gh9Ij2Kl5Mn8Pq/x", "http://t.example/passkey/REDACTED/x"},
		{"announce case-insensitive", "http://t.example/Announce/Ab3_dE-f6Gh9Ij2Kl5Mn8Pq", "http://t.example/Announce/REDACTED"},
		{"with query and port", "udp://t.example:6969/announce/Ab3_dE-f6Gh9Ij2Kl5Mn8Pq?info_hash=x", "udp://t.example:6969/announce/REDACTED?info_hash=x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, redact.URL(tt.in))
		})
	}
}

func TestURLKeepsOrdinaryPathSegments(t *testing.T) {
	for _, in := range []string{
		// Long token-looking segment, but not next to announce/passkey.
		"http://h/download/Ab3_dE-f6Gh9Ij2Kl5Mn8Pq/file",
		"http://h/getnzb/api/7365492846927364",
		"http://h/dl/0123456789abcdef0123456789abcde",                  // 31 hex chars
		"http://t.example/announce/short",                              // too short to be a passkey
		"http://t.example/announce/Some.Release.1080p.WEB-DL.x264-GRP", // dots: a name, not a token
		"http://t.example/announce",
		"http://t.example/announce/",
		"mailto:joey@example.com",
	} {
		assert.Equal(t, in, redact.URL(in))
	}
}
