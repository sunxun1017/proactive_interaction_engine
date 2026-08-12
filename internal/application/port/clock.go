package port

import "time"

type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

type Ticker interface {
	C() <-chan time.Time
	Stop()
}

type Clock interface {
	Now() time.Time
	NewTimer(time.Duration) Timer
	NewTicker(time.Duration) Ticker
}
