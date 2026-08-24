//go:build linux

package hooks

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/denelson1-dot/hinged-convertible/policy"
)

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// A hook that starts a long-lived process -- an input panel, a keyboard --
// must not be killed out from under the user. Launching a UI is a legitimate
// thing for a posture hook to do, and the process needs to outlive the hook.
func TestAsyncHookIsNotKilledByTimeout(t *testing.T) {
	// A cancellable context, because the original bug was that the hook
	// inherited one. An earlier version of this test passed context.Background
	// and so could never have caught it.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A unique argument makes the process findable without depending on
	// shell features: `exec -a` is bash-only and /bin/sh here is dash.
	marker := "5.0" + strconv.Itoa(os.Getpid()%9973)
	r := NewRunner([]Hook{{
		Event:   "tablet",
		Command: []string{"sleep", marker},
		Timeout: 300 * time.Millisecond,
		Async:   true,
	}}, quiet())

	start := time.Now()
	r.Run(ctx, policy.Transition{To: policy.PostureTablet})
	defer exec_kill(marker)

	// Well past the timeout that used to kill it.
	time.Sleep(1500 * time.Millisecond)
	if countProcs(marker) == 0 {
		t.Fatalf("async hook was killed after %v; a panel launched on fold would "+
			"vanish while the machine is still folded", time.Since(start))
	}

	// It must also outlive the daemon's own context, which is what stopping
	// hinged does.
	cancel()
	time.Sleep(500 * time.Millisecond)
	if countProcs(marker) == 0 {
		t.Error("async hook died when the daemon context was cancelled")
	}
}

// Synchronous hooks still need a bound: they run in the posture loop, so one
// that wedges would stall every later transition.
func TestSyncHookIsStillBounded(t *testing.T) {
	r := NewRunner([]Hook{{
		Event:   "tablet",
		Command: []string{"sleep", "10"},
		Timeout: 300 * time.Millisecond,
	}}, quiet())

	start := time.Now()
	r.Run(context.Background(), policy.Transition{To: policy.PostureTablet})
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("synchronous hook ran for %v; the timeout must still bound it", d)
	}
}

func TestHookMatchesEvent(t *testing.T) {
	for _, tc := range []struct {
		event string
		p     policy.Posture
		want  bool
	}{
		{"tablet", policy.PostureTablet, true},
		{"tablet", policy.PostureLaptop, false},
		{"any", policy.PostureClosed, true},
		{"laptop", policy.PostureLaptop, true},
	} {
		if got := matches(tc.event, tc.p); got != tc.want {
			t.Errorf("matches(%q, %v) = %v", tc.event, tc.p, got)
		}
	}
}
