package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/denelson1-dot/hinged-convertible/internal/policy"
	"github.com/denelson1-dot/hinged-convertible/internal/probe"
	"github.com/denelson1-dot/hinged-convertible/internal/source"
)

// watch runs the policy engine against live hardware and prints what it
// decides, without changing anything about the system.
//
// This is the tool for answering the two questions that determine whether
// hinged is needed on a given machine at all:
//
//  1. Does the kernel's SW_TABLET_MODE actually fire when you fold, or is it
//     inert? Many convertibles advertise the switch and never emit it.
//  2. Do the default angle thresholds match this chassis?
//
// It is read-only by design, so it is safe to run on hardware whose behaviour
// is unknown.
func watch() {
	r := probe.Run()

	h, err := source.OpenHinge()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hinge sensor unavailable: %v\n", err)
	} else {
		defer h.Close()
		fmt.Printf("hinge sensor: %s\n", h.Path())
		fmt.Printf("  %s\n", h.Describe())
		fmt.Printf("  polling every %v\n", h.Period())
	}

	switches := openReadableSwitches(r)
	defer func() {
		for _, s := range switches {
			s.Close()
		}
	}()

	if h == nil && len(switches) == 0 {
		fmt.Fprintln(os.Stderr, "\nNothing to watch: no readable hinge sensor and no readable switch.")
		fmt.Fprintln(os.Stderr, "Run 'hinged doctor' to see what is blocked and why.")
		os.Exit(1)
	}

	reportInitialSwitchState(switches)
	go watchSwitches(switches)

	fmt.Println("\nFold the machine. Ctrl-C to stop.")
	fmt.Println("Posture transitions are printed as the policy engine commits them.")
	fmt.Println()

	if h == nil {
		blockUntilInterrupt()
		return
	}
	runPolicyLoop(h)
}

func openReadableSwitches(r probe.Report) []*source.Switch {
	var out []*source.Switch
	for _, sd := range r.Switches {
		if !sd.Access.Readable {
			fmt.Printf("switch %-24s %s  (skipped: %s)\n", sd.Name, sd.Handler, sd.Access)
			continue
		}
		s, err := source.OpenSwitch(sd.Handler, sd.Name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "switch %s: %v\n", sd.Name, err)
			continue
		}
		out = append(out, s)
	}
	return out
}

// reportInitialSwitchState is the single most diagnostic line of output here.
// A switch stuck at 1 at startup is the failure that permanently disables the
// keyboard on affected HP machines; a switch that reads 0 and never changes is
// the inert case that makes a machine look supported when it is not.
func reportInitialSwitchState(switches []*source.Switch) {
	for _, s := range switches {
		tablet, err := s.State(source.SwTabletMode)
		if err != nil {
			fmt.Fprintf(os.Stderr, "switch %s: cannot read state: %v\n", s.Name(), err)
			continue
		}
		lid, _ := s.State(source.SwLid)
		fmt.Printf("switch %-24s SW_TABLET_MODE=%v SW_LID=%v\n", s.Name(), tablet, lid)
	}
}

func watchSwitches(switches []*source.Switch) {
	for _, s := range switches {
		go func(s *source.Switch) {
			for {
				ev, err := s.Read()
				if err != nil {
					return
				}
				name := "SW_?"
				switch ev.Code {
				case source.SwTabletMode:
					name = "SW_TABLET_MODE"
				case source.SwLid:
					name = "SW_LID"
				default:
					continue
				}
				fmt.Printf("%s  KERNEL SWITCH  %s %s=%v\n",
					time.Now().Format("15:04:05.000"), s.Name(), name, ev.Value)
			}
		}(s)
	}
}

func runPolicyLoop(h *source.Hinge) {
	cfg := policy.DefaultConfig()
	st := policy.State{}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	ticker := time.NewTicker(h.Period())
	defer ticker.Stop()

	var lastPrinted float64 = -1000

	for {
		select {
		case <-stop:
			fmt.Println("\nstopped")
			return
		case now := <-ticker.C:
			deg, err := h.Degrees()
			if err != nil {
				fmt.Fprintf(os.Stderr, "read error: %v\n", err)
				continue
			}

			var tr *policy.Transition
			st, tr = policy.Step(st, policy.Reading{
				Angle:   &deg,
				Trusted: true,
				At:      now,
			}, cfg)

			// Print the angle only when it moves meaningfully, so a stationary
			// machine does not scroll the terminal.
			if abs(deg-lastPrinted) >= 5 {
				fmt.Printf("%s  angle %6.1f  posture %-7s switch=%v\n",
					now.Format("15:04:05.000"), deg, st.Posture, st.Posture.TabletSwitch())
				lastPrinted = deg
			}

			if tr != nil {
				fmt.Printf("%s  >>> POSTURE %s -> %s  (%s)  switch=%v%s\n",
					now.Format("15:04:05.000"), tr.From, tr.To, tr.Reason,
					tr.To.TabletSwitch(), switchNote(tr))
			}
		}
	}
}

func switchNote(tr *policy.Transition) string {
	if tr.SwitchChanged {
		return "  [SW_TABLET_MODE would change]"
	}
	return ""
}

func blockUntilInterrupt() {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	fmt.Println("\nstopped")
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
