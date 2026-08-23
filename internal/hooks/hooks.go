//go:build linux

// Package hooks runs user commands on posture changes.
//
// All desktop-specific behaviour lives here rather than being compiled in.
// The implementation this replaces hardcoded Cinnamon gsettings calls and an
// arbitrary-JavaScript D-Bus call into the shell; both are configuration now,
// which is what lets one binary serve GNOME, KDE, sway and everything else.
package hooks

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"time"

	"github.com/denelson1-dot/hinged-convertible/policy"
)

// DefaultTimeout bounds a hook that hangs, so a wedged command cannot stall
// posture handling.
const DefaultTimeout = 10 * time.Second

// Hook is one command bound to a posture event.
type Hook struct {
	// Event is a posture name ("tablet", "laptop", "tent", "closed") or "any".
	Event string

	// Command is an argv. It is never passed through a shell: posture data
	// reaches hooks as environment variables, and interpolating device names
	// into a shell string would be an injection waiting to happen.
	Command []string

	Timeout time.Duration
	Async   bool

	// IgnoreExit treats a non-zero exit as success.
	//
	// Cleanup hooks routinely exit non-zero when there is nothing to clean:
	// pkill returns 1 when no process matched, which is the ordinary case on
	// every transition where the keyboard was not showing. Logging that as a
	// failure trains people to ignore the log, which defeats the point of
	// reporting real failures at all.
	IgnoreExit bool
}

// Runner executes hooks and reports on their health.
type Runner struct {
	hooks []Hook
	log   *slog.Logger

	// failures counts consecutive failures per hook, so a persistently broken
	// hook is visible rather than silently doing nothing. The implementation
	// this replaces discarded every exit code, which is how it could look
	// healthy on a desktop where none of its commands existed.
	failures map[int]int
}

func NewRunner(hs []Hook, log *slog.Logger) *Runner {
	if log == nil {
		log = slog.Default()
	}
	return &Runner{hooks: hs, log: log, failures: map[int]int{}}
}

// Run fires every hook matching the transition.
func (r *Runner) Run(ctx context.Context, tr policy.Transition) {
	for i, h := range r.hooks {
		if !matches(h.Event, tr.To) {
			continue
		}
		if h.Async {
			go r.exec(ctx, i, h, tr)
			continue
		}
		r.exec(ctx, i, h, tr)
	}
}

func matches(event string, p policy.Posture) bool {
	return event == "any" || event == p.String()
}

func (r *Runner) exec(ctx context.Context, idx int, h Hook, tr policy.Transition) {
	if len(h.Command) == 0 {
		return
	}
	timeout := h.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, h.Command[0], h.Command[1:]...)
	cmd.Env = append(cmd.Environ(),
		"HINGED_POSTURE="+tr.To.String(),
		"HINGED_PREVIOUS="+tr.From.String(),
		"HINGED_ANGLE="+strconv.FormatFloat(tr.Angle, 'f', 1, 64),
		"HINGED_SWITCH="+strconv.FormatBool(tr.To.TabletSwitch()),
		"HINGED_REASON="+tr.Reason.String(),
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	start := time.Now()
	err := cmd.Run()
	dur := time.Since(start)

	if err == nil || (h.IgnoreExit && cctx.Err() == nil) {
		r.failures[idx] = 0
		r.log.Debug("hook ok", "event", h.Event, "cmd", h.Command[0], "took", dur)
		return
	}

	r.failures[idx]++
	// Captured stderr, not just an exit code. "Which of my hooks is broken and
	// why" has to be answerable from the log alone.
	r.log.Warn("hook failed",
		"event", h.Event,
		"cmd", h.Command[0],
		"err", err,
		"stderr", trim(stderr.String()),
		"consecutive_failures", r.failures[idx],
		"took", dur)
	if cctx.Err() == context.DeadlineExceeded {
		r.log.Warn("hook timed out", "cmd", h.Command[0], "timeout", timeout)
	}
}

// Health reports hooks that are currently failing, for `hinged status`.
func (r *Runner) Health() []string {
	var out []string
	for i, n := range r.failures {
		if n > 0 && i < len(r.hooks) {
			out = append(out, fmt.Sprintf("%s: %d consecutive failures running %v",
				r.hooks[i].Event, n, r.hooks[i].Command))
		}
	}
	return out
}

func trim(s string) string {
	const max = 400
	s = string(bytes.TrimSpace([]byte(s)))
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
