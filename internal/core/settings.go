package core

import (
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
	"strings"
)

// Settings are the typed settings stored in the DB and changed through the
// API. Bootstrap configuration (flags and env) is not part of them.
type Settings struct {
	Queues QueueLimits `json:"queues"`
	Auth   Auth        `json:"auth"`
	Paths  Paths       `json:"paths"`
	// Network binds outbound Usenet and torrent sockets (off by default).
	Network NetworkSettings `json:"network"`
	// Seeding holds the global Share Limit defaults.
	Seeding SeedingSettings `json:"seeding"`
}

// QueueLimits is the concurrency limit of each Queue: how many non-force
// Downloads it runs at once.
type QueueLimits struct {
	Usenet  int `json:"usenet"`
	Torrent int `json:"torrent"`
}

// Auth holds the UI authentication options. Disable auth and LAN bypass
// apply to UI routes only and are off by default.
type Auth struct {
	DisableAuth bool `json:"disable_auth"`
	// LANBypassCIDRs lists the client networks that skip UI login. LAN
	// bypass is on when the list is non-empty.
	LANBypassCIDRs []string `json:"lan_bypass_cidrs"`
	// TrustedProxies lists the networks whose X-Forwarded-For is honoured.
	// Both network lists hold CIDRs such as "192.168.1.0/24"; a single
	// address is written as "/32" or "/128".
	TrustedProxies []string `json:"trusted_proxies"`
	// AllowedHosts lists the Host header values accepted while disable
	// auth or LAN bypass is on.
	AllowedHosts []string `json:"allowed_hosts"`
}

// Paths holds the data roots of each protocol. Changing them affects only
// new Downloads.
type Paths struct {
	Usenet  Roots `json:"usenet"`
	Torrent Roots `json:"torrent"`
}

// Roots are one protocol's absolute incomplete and complete directories.
// Ready output lands in <Complete>/<category>/<download> unless the
// Category overrides its dir.
type Roots struct {
	Incomplete string `json:"incomplete"`
	Complete   string `json:"complete"`
}

// DefaultSettings returns the Settings of a fresh install.
func DefaultSettings() Settings {
	return Settings{
		Queues: QueueLimits{Usenet: 1, Torrent: 5},
		Paths: Paths{
			Usenet:  Roots{Incomplete: "/data/usenet/incomplete", Complete: "/data/usenet/complete"},
			Torrent: Roots{Incomplete: "/data/torrents", Complete: "/data/torrents"},
		},
		Seeding: DefaultSeedingSettings(),
	}
}

// ErrInvalidSettings is wrapped by every error Settings.Validate returns.
var ErrInvalidSettings = errors.New("invalid settings")

// Validate reports whether the Settings may be stored. The error lists every
// problem found.
func (s Settings) Validate() error {
	var problems []error
	if s.Queues.Usenet < 0 {
		problems = append(problems, fmt.Errorf("usenet queue limit %d is negative", s.Queues.Usenet))
	}
	if s.Queues.Torrent < 0 {
		problems = append(problems, fmt.Errorf("torrent queue limit %d is negative", s.Queues.Torrent))
	}
	for _, cidr := range s.Auth.LANBypassCIDRs {
		if _, err := netip.ParsePrefix(cidr); err != nil {
			problems = append(problems, fmt.Errorf("lan bypass cidr: %w", err))
		}
	}
	for _, cidr := range s.Auth.TrustedProxies {
		if _, err := netip.ParsePrefix(cidr); err != nil {
			problems = append(problems, fmt.Errorf("trusted proxy: %w", err))
		}
	}
	for _, host := range s.Auth.AllowedHosts {
		if strings.TrimSpace(host) == "" {
			problems = append(problems, errors.New("allowed hosts must not contain blank entries"))
			break
		}
	}
	if (s.Auth.DisableAuth || len(s.Auth.LANBypassCIDRs) > 0) && len(s.Auth.AllowedHosts) == 0 {
		problems = append(problems, errors.New("disable auth and lan bypass require at least one allowed host"))
	}
	problems = append(problems, s.Paths.validate()...)
	problems = append(problems, s.Network.validate()...)
	problems = append(problems, s.Seeding.validate()...)
	if len(problems) > 0 {
		return fmt.Errorf("core: %w: %w", ErrInvalidSettings, errors.Join(problems...))
	}
	return nil
}

// validate checks the shape of the four data roots and nothing else: each is
// absolute and clean (no "..", no trailing or doubled separators), and no root
// lies inside another, which would let a finished Download be picked up again
// as incomplete data or its move target contain its own source. Equal roots are
// fine. Whether a root exists or is writable is the user's business and shows
// up when the first Download needs it.
func (p Paths) validate() []error {
	roots := []struct{ name, path string }{
		{"usenet incomplete", p.Usenet.Incomplete},
		{"usenet complete", p.Usenet.Complete},
		{"torrent incomplete", p.Torrent.Incomplete},
		{"torrent complete", p.Torrent.Complete},
	}
	var problems []error
	var shaped []struct{ name, path string }
	for _, root := range roots {
		switch {
		case !filepath.IsAbs(root.path):
			problems = append(problems, fmt.Errorf("%s root %q is not an absolute path", root.name, root.path))
		case filepath.Clean(root.path) != root.path:
			problems = append(problems, fmt.Errorf("%s root %q is not a clean path (no \"..\", \".\" or doubled and trailing separators)", root.name, root.path))
		default:
			shaped = append(shaped, root)
		}
	}
	for i, a := range shaped {
		for _, b := range shaped[i+1:] {
			switch {
			case isWithin(a.path, b.path):
				problems = append(problems, fmt.Errorf("%s root %q lies inside the %s root %q", b.name, b.path, a.name, a.path))
			case isWithin(b.path, a.path):
				problems = append(problems, fmt.Errorf("%s root %q lies inside the %s root %q", a.name, a.path, b.name, b.path))
			}
		}
	}
	return problems
}

// isWithin reports whether child is strictly below parent. Both are clean
// absolute paths.
func isWithin(parent, child string) bool {
	if parent == child {
		return false
	}
	if parent == string(filepath.Separator) {
		return true
	}
	return strings.HasPrefix(child, parent+string(filepath.Separator))
}
