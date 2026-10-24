package runner

import (
	"time"

	"github.com/jroosing/bitnews/internal/usenet/nntp"
)

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) NewTimer(d time.Duration) nntp.Timer { return systemTimer{time.NewTimer(d)} }

type systemTimer struct{ t *time.Timer }

func (s systemTimer) C() <-chan time.Time { return s.t.C }

func (s systemTimer) Stop() bool { return s.t.Stop() }
