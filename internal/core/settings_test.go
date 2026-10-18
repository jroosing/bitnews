package core_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

func TestDefaultQueueLimitsAreOneUsenetAndFiveTorrents(t *testing.T) {
	s := core.DefaultSettings()

	if s.Queues.Usenet != 1 {
		t.Errorf("Usenet Queue limit = %d, want 1", s.Queues.Usenet)
	}
	if s.Queues.Torrent != 5 {
		t.Errorf("torrent Queue limit = %d, want 5", s.Queues.Torrent)
	}
}

func TestDefaultRootsFollowTheTRaSHLayout(t *testing.T) {
	p := core.DefaultSettings().Paths

	want := map[string][2]string{
		"usenet incomplete":  {p.Usenet.Incomplete, "/data/usenet/incomplete"},
		"usenet complete":    {p.Usenet.Complete, "/data/usenet/complete"},
		"torrent incomplete": {p.Torrent.Incomplete, "/data/torrents"},
		"torrent complete":   {p.Torrent.Complete, "/data/torrents"},
	}
	for name, got := range want {
		if got[0] != got[1] {
			t.Errorf("%s root = %q, want %q", name, got[0], got[1])
		}
	}
}

func TestDefaultSettingsAreValid(t *testing.T) {
	if err := core.DefaultSettings().Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestAuthBypassesAreOffByDefault(t *testing.T) {
	a := core.DefaultSettings().Auth

	if a.DisableAuth {
		t.Error("disable auth is on by default")
	}
	if len(a.LANBypassCIDRs) != 0 {
		t.Errorf("LAN bypass CIDRs = %v, want none", a.LANBypassCIDRs)
	}
}

func TestQueueLimitMayBeZeroButNotNegative(t *testing.T) {
	s := core.DefaultSettings()
	s.Queues = core.QueueLimits{Usenet: 0, Torrent: 0}
	if err := s.Validate(); err != nil {
		t.Errorf("zero limits: %v", err)
	}

	s.Queues = core.QueueLimits{Usenet: -1, Torrent: 5}
	if err := s.Validate(); !errors.Is(err, core.ErrInvalidSettings) {
		t.Errorf("negative Usenet Queue limit: got %v, want ErrInvalidSettings", err)
	}
	s.Queues = core.QueueLimits{Usenet: 1, Torrent: -1}
	if err := s.Validate(); !errors.Is(err, core.ErrInvalidSettings) {
		t.Errorf("negative torrent Queue limit: got %v, want ErrInvalidSettings", err)
	}
}

func TestEveryDataRootMustBeAbsolute(t *testing.T) {
	roots := map[string]func(*core.Settings) *string{
		"usenet incomplete":  func(s *core.Settings) *string { return &s.Paths.Usenet.Incomplete },
		"usenet complete":    func(s *core.Settings) *string { return &s.Paths.Usenet.Complete },
		"torrent incomplete": func(s *core.Settings) *string { return &s.Paths.Torrent.Incomplete },
		"torrent complete":   func(s *core.Settings) *string { return &s.Paths.Torrent.Complete },
	}
	for name, root := range roots {
		for _, bad := range []string{"", "data/usenet", "./data"} {
			s := core.DefaultSettings()
			*root(&s) = bad
			if err := s.Validate(); !errors.Is(err, core.ErrInvalidSettings) {
				t.Errorf("%s root %q: got %v, want ErrInvalidSettings", name, bad, err)
			}
		}
	}
}

func TestNetworkListsMustBeCIDRs(t *testing.T) {
	good := []string{"192.168.1.0/24", "10.0.0.0/8", "fd00::/8", "127.0.0.1/32"}
	for _, bad := range []string{"", "192.168.1.0", "192.168.1.0/33", "lan", "10.0.0.0/8 "} {
		s := core.DefaultSettings()
		s.Auth.AllowedHosts = []string{"bitnews.lan"}
		s.Auth.LANBypassCIDRs = append(append([]string{}, good...), bad)
		if err := s.Validate(); !errors.Is(err, core.ErrInvalidSettings) {
			t.Errorf("LAN bypass CIDR %q: got %v, want ErrInvalidSettings", bad, err)
		}

		s = core.DefaultSettings()
		s.Auth.TrustedProxies = append(append([]string{}, good...), bad)
		if err := s.Validate(); !errors.Is(err, core.ErrInvalidSettings) {
			t.Errorf("trusted proxy %q: got %v, want ErrInvalidSettings", bad, err)
		}
	}

	s := core.DefaultSettings()
	s.Auth.AllowedHosts = []string{"bitnews.lan"}
	s.Auth.LANBypassCIDRs = good
	s.Auth.TrustedProxies = good
	if err := s.Validate(); err != nil {
		t.Errorf("valid CIDRs: %v", err)
	}
}

func TestAllowedHostsMustNotContainBlankEntries(t *testing.T) {
	for _, bad := range []string{"", "   "} {
		s := core.DefaultSettings()
		s.Auth.AllowedHosts = []string{"bitnews.lan", bad}
		if err := s.Validate(); !errors.Is(err, core.ErrInvalidSettings) {
			t.Errorf("allowed host %q: got %v, want ErrInvalidSettings", bad, err)
		}
	}

	s := core.DefaultSettings()
	s.Auth.AllowedHosts = []string{"bitnews.lan", "192.168.1.10:9494"}
	if err := s.Validate(); err != nil {
		t.Errorf("valid allowed hosts: %v", err)
	}
}

func TestAuthBypassRequiresAllowedHosts(t *testing.T) {
	bypasses := map[string]func(*core.Settings){
		"disable auth": func(s *core.Settings) { s.Auth.DisableAuth = true },
		"LAN bypass":   func(s *core.Settings) { s.Auth.LANBypassCIDRs = []string{"192.168.1.0/24"} },
	}
	for name, enable := range bypasses {
		s := core.DefaultSettings()
		enable(&s)
		if err := s.Validate(); !errors.Is(err, core.ErrInvalidSettings) {
			t.Errorf("%s without allowed hosts: got %v, want ErrInvalidSettings", name, err)
		}

		s.Auth.AllowedHosts = []string{"bitnews.lan"}
		if err := s.Validate(); err != nil {
			t.Errorf("%s with allowed hosts: %v", name, err)
		}
	}
}

func TestSettingsUseTheDocumentedJSONKeys(t *testing.T) {
	b, err := json.Marshal(core.DefaultSettings())
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}

	want := map[string][]string{
		"queues": {"usenet", "torrent"},
		"auth":   {"disable_auth", "lan_bypass_cidrs", "trusted_proxies", "allowed_hosts"},
		"paths":  {"usenet", "torrent"},
	}
	for group, keys := range want {
		for _, key := range keys {
			if _, ok := got[group][key]; !ok {
				t.Errorf("missing key %s.%s in %s", group, key, b)
			}
		}
	}
	for _, proto := range []string{"usenet", "torrent"} {
		roots, _ := got["paths"][proto].(map[string]any)
		for _, key := range []string{"incomplete", "complete"} {
			if _, ok := roots[key]; !ok {
				t.Errorf("missing key paths.%s.%s in %s", proto, key, b)
			}
		}
	}
}

func TestSettingsSurviveAJSONRoundTrip(t *testing.T) {
	in := core.DefaultSettings()
	in.Auth = core.Auth{
		DisableAuth:    true,
		LANBypassCIDRs: []string{"192.168.1.0/24"},
		TrustedProxies: []string{"172.18.0.2/32"},
		AllowedHosts:   []string{"bitnews.lan"},
	}

	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out core.Settings
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("round trip changed Settings:\n in: %+v\nout: %+v", in, out)
	}
}
