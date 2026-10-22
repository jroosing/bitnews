package web

import (
	"math"
	"net/http"
	"strconv"

	"gitlab.com/kyura4k/gitnews/internal/core"
)

// speedSection is the "Speed & queue" entry of the settings registry (M6-07).
func speedSection() SettingsSection {
	return SettingsSection{
		Slug: "speed", Title: "Speed & queue", Icon: "arrow-down", Order: 30,
		Description: "Speed limits, how many downloads run at once and free space.",
		Template:    "settings_speed.html", Data: settingsSpeedData, Routes: (*Server).registerSpeedRoutes,
	}
}

func settingsSpeedData(s *Server, r *http.Request) (any, error) {
	return s.cardPage(r, speedCards, nil), nil
}

func (s *Server) registerSpeedRoutes() { s.registerCards(speedCards) }

var speedCards = []*settingsCard{speedLimitsCard, speedQueueCard, speedSpaceCard}

// speedLimitFields lists the five limits: form field, and where each lives.
var speedLimitFields = []struct {
	name string
	get  func(*core.SpeedSettings) *int
}{
	{"global_down_kbps", func(s *core.SpeedSettings) *int { return &s.Global.DownKBps }},
	{"global_up_kbps", func(s *core.SpeedSettings) *int { return &s.Global.UpKBps }},
	{"usenet_down_kbps", func(s *core.SpeedSettings) *int { return &s.Usenet.DownKBps }},
	{"torrent_down_kbps", func(s *core.SpeedSettings) *int { return &s.Torrent.DownKBps }},
	{"torrent_up_kbps", func(s *core.SpeedSettings) *int { return &s.Torrent.UpKBps }},
}

// speedLimitsCard edits Settings.Speed. It applies live.
var speedLimitsCard = &settingsCard{
	key: "limits", route: "/settings/speed", slug: "speed", partial: "speed_limits_card",
	saved: "Speed limits saved. They apply right away.",
	values: func(s core.Settings) map[string]string {
		sp := s.Speed
		v := map[string]string{}
		for _, f := range speedLimitFields {
			v[f.name] = optNumber(*f.get(&sp))
		}
		return v
	},
	read: func(_ *Server, f map[string]string, next *core.Settings) map[string]string {
		errs := map[string]string{}
		for _, fld := range speedLimitFields {
			n, _, ok := wholeNumber(f[fld.name], core.MaxSpeedKBps)
			if !ok {
				errs[fld.name] = "Enter a whole number of KB/s, such as 5000, or leave it empty for no limit."
				continue
			}
			*fld.get(&next.Speed) = int(n)
		}
		return errs
	},
}

// maxQueueSize bounds how many downloads a queue may run at once.
const maxQueueSize = 1000

// speedQueueCard edits how many downloads run at once. The queue sizes apply
// live; post-processing concurrency is read when bitnews starts.
var speedQueueCard = &settingsCard{
	key: "queue", route: "/settings/speed/queue", slug: "speed", partial: "speed_queue_card",
	saved:       "Queue sizes saved.",
	restartNote: "Restart bitnews for the post-processing change to take effect.",
	values: func(s core.Settings) map[string]string {
		return map[string]string{
			"usenet_queue":         itoa(s.Queues.Usenet),
			"torrent_queue":        itoa(s.Queues.Torrent),
			"postproc_concurrency": itoa(s.Queues.EffectivePostprocConcurrency()),
		}
	},
	read: func(_ *Server, f map[string]string, next *core.Settings) map[string]string {
		errs := map[string]string{}
		for _, fld := range []struct {
			name string
			max  int64
			dst  *int
			msg  string
		}{
			{"usenet_queue", maxQueueSize, &next.Queues.Usenet, "Enter a whole number from 1 to 1000."},
			{"torrent_queue", maxQueueSize, &next.Queues.Torrent, "Enter a whole number from 1 to 1000."},
			{"postproc_concurrency", core.MaxPostprocConcurrency, &next.Queues.PostprocConcurrency, "Enter a whole number from 1 to 16."},
		} {
			n, blank, ok := wholeNumber(f[fld.name], fld.max)
			if !ok || blank || n < 1 {
				errs[fld.name] = fld.msg
				continue
			}
			*fld.dst = int(n)
		}
		return errs
	},
	restart: func(_ *Server, started, saved core.Settings) bool {
		return started.Queues.EffectivePostprocConcurrency() != saved.Queues.EffectivePostprocConcurrency()
	},
}

// Bounds of the free space to keep, in GB (1 GB = 2^30 bytes).
const (
	minFreeMinGB = 0.1
	minFreeMaxGB = 100_000
)

const gib = 1 << 30

// speedSpaceCard edits min_free. It applies live.
var speedSpaceCard = &settingsCard{
	key: "space", route: "/settings/speed/space", slug: "speed", partial: "speed_space_card",
	saved: "Free space saved. It applies right away.",
	values: func(s core.Settings) map[string]string {
		gb := float64(s.Queues.EffectiveMinFree()) / gib
		return map[string]string{"min_free_gb": strconv.FormatFloat(gb, 'f', -1, 64)}
	},
	read: func(_ *Server, f map[string]string, next *core.Settings) map[string]string {
		gb, blank, ok := decimalNumber(f["min_free_gb"], minFreeMaxGB)
		if !ok || blank || gb < minFreeMinGB {
			return map[string]string{"min_free_gb": "Enter an amount in GB from 0.1 to 100000, such as 5 or 2.5."}
		}
		next.Queues.MinFree = int64(math.Round(gb * gib))
		return nil
	},
}
