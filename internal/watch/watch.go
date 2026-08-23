//go:build linux

// Package watch runs the policy engine against live hardware and reports what
// it decides, without changing anything about the system.
//
// It answers the two questions that determine whether hinged is needed on a
// given machine:
//
//  1. Does the kernel's SW_TABLET_MODE actually fire when you fold, or is it
//     inert? Many convertibles advertise the switch and never emit it.
//  2. Do the angle thresholds match this chassis?
//
// It is read-only by design, so it is safe on hardware whose behaviour is
// unknown.
package watch

import (
	"context"
	"fmt"
	"io"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/denelson1-dot/hinged-convertible/internal/probe"
	"github.com/denelson1-dot/hinged-convertible/internal/source"
	"github.com/denelson1-dot/hinged-convertible/policy"
)

// errorReportInterval rate-limits repeated identical read failures.
//
// Reads happen at up to 20 Hz. A sensor that disappears -- an unbind, a failed
// resume, a module reload -- would otherwise emit twenty identical lines a
// second forever, which for a daemon means burying the journal at exactly the
// moment it is the only diagnostic left.
const errorReportInterval = 5 * time.Second

// Run drives the policy loop until ctx is cancelled.
func Run(ctx context.Context, w io.Writer, cfg policy.Config) error {
	r := probe.Run()

	h, hingeErr := source.OpenHinge()
	if hingeErr != nil {
		fmt.Fprintf(w, "hinge sensor unavailable: %v\n", hingeErr)
	} else {
		defer h.Close()
		fmt.Fprintf(w, "hinge sensor: %s\n", h.Path())
		fmt.Fprintf(w, "  %s\n", h.Describe())
		fmt.Fprintf(w, "  polling every %v\n", h.Period())
		if ceil := policy.SlewGateCeiling(cfg); ceil > 0 && h.Period() > ceil {
			fmt.Fprintf(w, "  note: past the %v slew-gate ceiling, so glitch rejection is inert here\n", ceil)
		}
	}

	switches := openReadableSwitches(w, r)
	defer func() {
		for _, s := range switches {
			s.Close()
		}
	}()

	if hingeErr != nil && len(switches) == 0 {
		return fmt.Errorf("nothing to watch: no readable hinge sensor and no readable switch; " +
			"run 'hinged doctor' to see what is blocked and why")
	}

	lid := &lidTracker{}
	reportInitialSwitchState(w, switches, lid)
	watchSwitches(ctx, w, switches, lid)

	fmt.Fprintln(w, "\nFold the machine. Ctrl-C to stop.")
	fmt.Fprintln(w, "Posture transitions are printed as the policy engine commits them.")
	fmt.Fprintln(w)

	if hingeErr != nil {
		<-ctx.Done()
		return nil
	}
	return runPolicyLoop(ctx, w, h, lid, cfg)
}

func openReadableSwitches(w io.Writer, r probe.Report) []*source.Switch {
	var out []*source.Switch
	for _, sd := range r.Switches {
		if !sd.Access.Readable {
			fmt.Fprintf(w, "switch %-24s %s  (skipped: %s)\n", safeName(sd.Name), sd.Handler, sd.Access)
			continue
		}
		s, err := source.OpenSwitch(sd.Handler, sd.Name)
		if err != nil {
			fmt.Fprintf(w, "switch %s: %v\n", safeName(sd.Name), err)
			continue
		}
		out = append(out, s)
	}
	return out
}

// reportInitialSwitchState is the most diagnostic output here. A switch stuck
// at 1 on startup is the failure that permanently disables the keyboard on
// affected machines; one that reads 0 and never changes is the inert case that
// makes a machine look supported when it is not.
func reportInitialSwitchState(w io.Writer, switches []*source.Switch, lid *lidTracker) {
	for _, s := range switches {
		tablet, err := s.State(source.SwTabletMode)
		if err != nil {
			fmt.Fprintf(w, "switch %s: cannot read state: %v\n", safeName(s.Name()), err)
			continue
		}
		closed, err := s.State(source.SwLid)
		if err == nil {
			lid.set(closed)
		}
		fmt.Fprintf(w, "switch %-24s SW_TABLET_MODE=%v SW_LID=%v\n", safeName(s.Name()), tablet, closed)
	}
}

func watchSwitches(ctx context.Context, w io.Writer, switches []*source.Switch, lid *lidTracker) {
	for _, s := range switches {
		go func(s *source.Switch) {
			for {
				ev, err := s.Read()
				if err != nil {
					// Distinguish shutdown from a device that went away; the
					// whole point of this tool is telling those apart.
					if ctx.Err() == nil {
						fmt.Fprintf(w, "switch %s: stopped reading: %v\n", safeName(s.Name()), err)
					}
					return
				}
				var name string
				switch ev.Code {
				case source.SwTabletMode:
					name = "SW_TABLET_MODE"
				case source.SwLid:
					name = "SW_LID"
					lid.set(ev.Value)
				default:
					continue
				}
				fmt.Fprintf(w, "%s  KERNEL SWITCH  %s %s=%v\n",
					time.Now().Format("15:04:05.000"), safeName(s.Name()), name, ev.Value)
			}
		}(s)
	}
}

func runPolicyLoop(ctx context.Context, w io.Writer, h *source.Hinge, lid *lidTracker, cfg policy.Config) error {
	engine, err := policy.New(cfg)
	if err != nil {
		return fmt.Errorf("policy config: %w", err)
	}

	ticker := time.NewTicker(h.Period())
	defer ticker.Stop()

	lastPrinted := math.Inf(-1)
	var lastErr string
	var lastErrAt time.Time
	var suppressed int

	for {
		select {
		case <-ctx.Done():
			fmt.Fprintln(w, "\nstopped")
			return nil

		case now := <-ticker.C:
			deg, err := h.Degrees()
			if err != nil {
				// Advance the dead-man clock even when the sensor is failing.
				// This is the path that releases the switch when a sensor
				// wedges while tablet mode is asserted.
				if tr, ok := engine.Tick(now); ok {
					printTransition(w, now, tr)
				}
				if msg := err.Error(); msg != lastErr || now.Sub(lastErrAt) > errorReportInterval {
					if suppressed > 0 {
						fmt.Fprintf(w, "  (%d identical errors suppressed)\n", suppressed)
					}
					fmt.Fprintf(w, "%s  read error: %v\n", now.Format("15:04:05.000"), err)
					lastErr, lastErrAt, suppressed = msg, now, 0
				} else {
					suppressed++
				}
				continue
			}
			lastErr, suppressed = "", 0

			tr, ok := engine.Step(policy.Reading{
				Angle:     deg,
				HasAngle:  true,
				LidClosed: lid.get(),
				Trusted:   true,
				At:        now,
			})

			// Print the angle only when it moves meaningfully, so a stationary
			// machine does not scroll the terminal.
			if math.Abs(deg-lastPrinted) >= 5 {
				fmt.Fprintf(w, "%s  angle %6.1f  posture %-7s switch=%v\n",
					now.Format("15:04:05.000"), deg, engine.Posture(), engine.Posture().TabletSwitch())
				lastPrinted = deg
			}
			if ok {
				printTransition(w, now, tr)
			}
		}
	}
}

func printTransition(w io.Writer, now time.Time, tr policy.Transition) {
	note := ""
	if tr.SwitchChanged {
		note = "  [SW_TABLET_MODE would change]"
	}
	fmt.Fprintf(w, "%s  >>> POSTURE %s -> %s  (%s)  switch=%v%s\n",
		now.Format("15:04:05.000"), tr.From, tr.To, tr.Reason,
		tr.To.TabletSwitch(), note)
}

// lidTracker holds the most recent lid state for the policy.
//
// Without it the lid override is inert: a shut lid must never assert tablet
// mode, and at startup the lid is the only thing distinguishing a machine
// folded past 360 from one simply closed.
type lidTracker struct {
	mu    sync.RWMutex
	known bool
	state bool // true == closed
}

func (l *lidTracker) set(closed bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.known, l.state = true, closed
}

// get reports lid state only when something actually provided it, so an absent
// lid switch stays absent rather than defaulting to "open".
func (l *lidTracker) get() policy.OptBool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if !l.known {
		return policy.OptBool{}
	}
	return policy.Bool(l.state)
}

// safeName renders a kernel-supplied device name for a terminal.
//
// Device names are attacker-influenced: uinput copies a caller-supplied name
// with no sanitisation, and USB product strings reach the same field. An
// unescaped name can carry ANSI escapes that forge output or drive the
// terminal, and users are told to paste this output into bug reports. Quoting
// also stops an embedded tab from corrupting column alignment.
func safeName(s string) string {
	q := strconv.Quote(s)
	return q[1 : len(q)-1]
}
