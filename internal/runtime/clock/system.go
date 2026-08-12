package clock

import (
	"time"

	"proactive-interaction-engine/internal/application/port"
)

type System struct{}

func (System) Now() time.Time { return time.Now() }

func (System) NewTimer(duration time.Duration) port.Timer {
	return systemTimer{Timer: time.NewTimer(duration)}
}

func (System) NewTicker(duration time.Duration) port.Ticker {
	return systemTicker{Ticker: time.NewTicker(duration)}
}

type systemTimer struct{ *time.Timer }

func (t systemTimer) C() <-chan time.Time { return t.Timer.C }

type systemTicker struct{ *time.Ticker }

func (t systemTicker) C() <-chan time.Time { return t.Ticker.C }
