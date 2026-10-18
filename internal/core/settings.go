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
	for _, root := range []struct{ name, path string }{
		{"usenet incomplete", s.Paths.Usenet.Incomplete},
		{"usenet complete", s.Paths.Usenet.Complete},
		{"torrent incomplete", s.Paths.Torrent.Incomplete},
		{"torrent complete", s.Paths.Torrent.Complete},
	} {
		if !filepath.IsAbs(root.path) {
			problems = append(problems, fmt.Errorf("%s root %q is not an absolute path", root.name, root.path))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("core: %w: %w", ErrInvalidSettings, errors.Join(problems...))
	}
	return nil
}
