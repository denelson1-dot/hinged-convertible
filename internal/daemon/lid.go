//go:build linux

package daemon

import (
	"sync"

	"github.com/denelson1-dot/hinged-convertible/policy"
)

// lidTracker holds the most recent lid state for the policy.
//
// A shut lid must never assert tablet mode, and at startup the lid is the only
// thing distinguishing a machine folded past 360 from one simply closed. It
// reports absence rather than defaulting to "open", because a missing lid
// switch reading as a permanently open lid would defeat the whole point.
type lidTracker struct {
	mu    sync.RWMutex
	known bool
	state bool // true == closed
}

func newLidTracker() *lidTracker { return &lidTracker{} }

func (l *lidTracker) set(closed bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.known, l.state = true, closed
}

func (l *lidTracker) get() policy.OptBool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if !l.known {
		return policy.OptBool{}
	}
	return policy.Bool(l.state)
}
