package apitest

import (
	"time"

	"github.com/fin-assist/go-worker"
)

// quietScheduler leaves background recalculation explicitly controlled in tests
// that inspect an enqueued source change. The real job is assembled and new worker
// tests supply a ticking clock to exercise production scheduling and delivery.
type quietScheduler struct{}
type quietTick struct{ ch chan time.Time }

func (quietScheduler) NewTimer(time.Duration) worker.Timer   { return quietTick{make(chan time.Time)} }
func (quietScheduler) NewTicker(time.Duration) worker.Ticker { return quietTick{make(chan time.Time)} }
func (t quietTick) C() <-chan time.Time                      { return t.ch }
func (quietTick) Stop()                                      {}
