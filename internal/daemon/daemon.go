//go:build linux

// Package daemon runs the posture loop and drives the world from it.
//
// It owns the clock, the goroutines, and the ordering rules. Everything it
// calls is either pure (the policy) or a single-purpose effect (the switch,
// the hooks), which keeps the parts that decide separate from the parts that
// act.
package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/denelson1-dot/hinged-convertible/internal/hooks"
	"github.com/denelson1-dot/hinged-convertible/internal/probe"
	"github.com/denelson1-dot/hinged-convertible/internal/source"
	"github.com/denelson1-dot/hinged-convertible/internal/uinput"
	"github.com/denelson1-dot/hinged-convertible/policy"
)

// errorReportInterval rate-limits repeated identical read failures, so a
// sensor that disappears cannot bury the journal at the moment it is the only
// diagnostic left.
const errorReportInterval = 30 * time.Second

// Options configures a run.
type Options struct {
	Policy policy.Config
	Uinput uinput.Config
	Hooks  []hooks.Hook

	// DryRun decides everything and reports it, but creates no virtual device
	// and runs no hooks. This is how you check a machine's behaviour before
	// letting it touch your input devices.
	DryRun bool

	Log *slog.Logger
}

// Run drives posture until ctx is cancelled.
//
// The virtual switch is always released and destroyed on the way out, on every
// exit path including signals, because libinput will not resume a suspended
// keyboard for a device that merely disappears while asserting.
func Run(ctx context.Context, o Options) error {
	log := o.Log
	if log == nil {
		log = slog.Default()
	}

	engine, err := policy.New(o.Policy)
	if err != nil {
		return fmt.Errorf("policy configuration: %w", err)
	}

	hinge, err := source.OpenHinge()
	if err != nil {
		return fmt.Errorf("no posture source: %w\n\nrun 'hinged doctor' to see what this machine exposes", err)
	}
	defer hinge.Close()
	log.Info("posture source",
		"attribute", hinge.Path(), "units", hinge.Describe(), "poll", hinge.Period())

	// The lid is a modifier, not a source: it settles what a near-zero angle
	// means, which the angle alone cannot.
	lid := newLidTracker()
	switches := openLidSwitches(ctx, log, lid)
	defer func() {
		for _, s := range switches {
			s.Close()
		}
	}()
	if len(switches) == 0 {
		log.Warn("no readable lid switch; near-zero angles will be treated as undecidable rather than as a fold",
			"remedy", "install packaging/udev/70-hinged-switch.rules")
	}

	var sw *uinput.Switch
	if o.DryRun {
		log.Info("dry run: no virtual device will be created and no hooks will run")
	} else {
		sw, err = uinput.Create(o.Uinput)
		if err != nil {
			return fmt.Errorf("creating the virtual switch: %w", err)
		}
		// Deferred as well as handled on the normal path, so a panic still
		// releases rather than stranding the user without a keyboard.
		defer func() {
			if cerr := sw.Close(); cerr != nil {
				log.Error("releasing the virtual switch", "err", cerr)
			} else {
				log.Info("virtual switch released and destroyed")
			}
		}()
		log.Info("virtual switch created", "name", sw.Name())
	}

	runner := hooks.NewRunner(o.Hooks, log)

	ticker := time.NewTicker(hinge.Period())
	defer ticker.Stop()

	var lastErr string
	var lastErrAt time.Time
	var suppressed int

	log.Info("watching", "enter", o.Policy.TentMin, "leave", o.Policy.LaptopMax,
		"wrap_guard", o.Policy.WrapGuard)

	for {
		select {
		case <-ctx.Done():
			log.Info("shutting down")
			return nil

		case now := <-ticker.C:
			deg, err := hinge.Degrees()
			if err != nil {
				// The dead-man path. A sensor that wedges while asserting must
				// not leave the keyboard suppressed, and releasing cannot be
				// allowed to depend on the sensor answering.
				if tr, ok := engine.Tick(now); ok {
					log.Warn("sensor stopped reporting; releasing for safety",
						"from", tr.From.String(), "to", tr.To.String())
					apply(ctx, log, sw, runner, tr, o.DryRun)
				}
				if msg := err.Error(); msg != lastErr || now.Sub(lastErrAt) > errorReportInterval {
					if suppressed > 0 {
						log.Warn("suppressed repeated read errors", "count", suppressed)
					}
					log.Error("reading hinge angle", "err", err)
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
			if !ok {
				continue
			}
			log.Info("posture",
				"from", tr.From.String(), "to", tr.To.String(),
				"angle", fmt.Sprintf("%.1f", tr.Angle),
				"reason", tr.Reason.String(),
				"switch", tr.To.TabletSwitch())
			apply(ctx, log, sw, runner, tr, o.DryRun)
		}
	}
}

// apply carries a decision into the world.
//
// Ordering rule, inherited from the implementation this replaces: when
// returning to laptop posture, restore input before doing anything slower. The
// machine must never be briefly awake-looking but dead to input.
func apply(ctx context.Context, log *slog.Logger, sw *uinput.Switch, runner *hooks.Runner, tr policy.Transition, dryRun bool) {
	releasing := !tr.To.TabletSwitch()

	setSwitch := func() {
		if dryRun || sw == nil || !tr.SwitchChanged {
			return
		}
		if err := sw.Set(tr.To.TabletSwitch()); err != nil {
			log.Error("driving the virtual switch", "err", err, "want", tr.To.TabletSwitch())
			return
		}
		log.Info("SW_TABLET_MODE", "value", tr.To.TabletSwitch())
	}

	if releasing {
		setSwitch()
		runner.Run(ctx, tr)
		return
	}
	runner.Run(ctx, tr)
	setSwitch()
}

func openLidSwitches(ctx context.Context, log *slog.Logger, lid *lidTracker) []*source.Switch {
	var out []*source.Switch
	for _, sd := range probe.Run().Switches {
		if !sd.Lid || !sd.Access.Readable {
			continue
		}
		s, err := source.OpenSwitch(sd.Handler, sd.Name)
		if err != nil {
			log.Warn("opening lid switch", "device", sd.Name, "err", err)
			continue
		}
		if closed, err := s.State(source.SwLid); err == nil {
			lid.set(closed)
			log.Info("lid switch", "device", sd.Name, "closed", closed)
		}
		out = append(out, s)
		go func(s *source.Switch) {
			for {
				ev, err := s.Read()
				if err != nil {
					if ctx.Err() == nil {
						log.Warn("lid switch stopped reporting", "device", s.Name(), "err", err)
					}
					return
				}
				if ev.Code == source.SwLid {
					lid.set(ev.Value)
					log.Info("lid", "closed", ev.Value)
				}
			}
		}(s)
	}
	return out
}
