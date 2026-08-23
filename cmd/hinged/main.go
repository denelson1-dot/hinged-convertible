// Command hinged reports and controls convertible-laptop posture.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/denelson1-dot/hinged-convertible/internal/probe"
	"github.com/denelson1-dot/hinged-convertible/internal/watch"
	"github.com/denelson1-dot/hinged-convertible/policy"
)

const usage = `hinged - tablet mode for Linux convertibles

Usage:
  hinged doctor    Report what this machine exposes and what hinged would use
  hinged watch     Watch posture live, read-only, without changing anything
  hinged version   Print the version

Threshold flags (watch):
  -enter-angle   Angle at or above which the keyboard counts as folded away
  -leave-angle   Angle at or below which the machine is in laptop posture
  -wrap-guard    Angle below which a reading may be a hinge wrapped past 360
  -tablet-angle  Angle at or above which the machine is fully folded
  -max-slew      Reject angular change faster than this, degrees/second

The defaults are calibrated for one chassis and will not suit every machine.
Run 'hinged doctor' first; its output is what to paste into a bug report.
`

// version is set at build time via -ldflags.
var version = "dev"

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "hinged: %v\n", err)
		os.Exit(1)
	}
}

// run is the real entry point, separated from main so that it is testable and
// so that deferred cleanup actually runs before exit.
func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return flag.ErrHelp
	}

	switch args[0] {
	case "doctor":
		doctor(stdout)
		return nil

	case "watch":
		cfg, err := configFromFlags(args[1:], stderr)
		if err != nil {
			return err
		}
		// Signals are wired before any I/O and cancel a context rather than
		// only filling a channel. A handler that merely notifies is useless if
		// the loop is blocked on a wedged sensor read, and for a daemon that
		// means `systemctl stop` hangs until it escalates to SIGKILL.
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return watch.Run(ctx, stdout, cfg)

	case "version":
		fmt.Fprintln(stdout, version)
		return nil

	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return nil

	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", args[0], usage)
		return flag.ErrHelp
	}
}

// configFromFlags builds a policy config, letting any threshold be overridden.
// Compiled-in thresholds were the main reason this only suited one chassis.
func configFromFlags(args []string, stderr io.Writer) (policy.Config, error) {
	cfg := policy.DefaultConfig()

	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Float64Var(&cfg.TentMin, "enter-angle", cfg.TentMin, "assert tablet mode at or above this angle")
	fs.Float64Var(&cfg.LaptopMax, "leave-angle", cfg.LaptopMax, "laptop posture at or below this angle")
	fs.Float64Var(&cfg.WrapGuard, "wrap-guard", cfg.WrapGuard, "below this, a reading may be a hinge wrapped past 360")
	fs.Float64Var(&cfg.TabletMin, "tablet-angle", cfg.TabletMin, "fully folded at or above this angle")
	fs.Float64Var(&cfg.MaxSlewRate, "max-slew", cfg.MaxSlewRate, "reject angular change faster than this in degrees/second (0 disables)")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}

	// Validated here rather than letting the engine silently do nothing.
	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("%w\n\nthresholds must satisfy 0 < wrap-guard < leave-angle < enter-angle < tablet-angle", err)
	}
	return cfg, nil
}

func doctor(out io.Writer) {
	r := probe.Run()
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)

	fmt.Fprintln(out, "MACHINE")
	fmt.Fprintf(w, "  vendor\t%s\n", orNone(r.Machine.Vendor))
	fmt.Fprintf(w, "  product\t%s\n", orNone(r.Machine.Product))
	fmt.Fprintf(w, "  chassis\t%s (DMI type %d)\n", r.Machine.ChassisDescription(), r.Machine.ChassisType)
	fmt.Fprintf(w, "  kernel\t%s\n", orNone(r.Machine.Kernel))
	w.Flush()

	fmt.Fprintln(out, "\nSWITCH DEVICES (evdev EV_SW)")
	if len(r.Switches) == 0 {
		fmt.Fprintln(out, "  none")
	}
	for _, s := range r.Switches {
		var caps []string
		if s.TabletMode {
			caps = append(caps, "SW_TABLET_MODE")
		}
		if s.Lid {
			caps = append(caps, "SW_LID")
		}
		fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", s.Handler, safeName(s.Name), strings.Join(caps, ","), s.Access)
	}
	w.Flush()

	fmt.Fprintln(out, "\nHINGE SENSORS (IIO)")
	if len(r.Hinges) == 0 {
		fmt.Fprintln(out, "  none")
	}
	for _, h := range r.Hinges {
		fmt.Fprintf(w, "  %s\tname=%s\t%s\n", h.Dir, orNone(h.Name), h.Access)
		fmt.Fprintf(w, "  \tattribute\t%s\n", h.Raw)
		fmt.Fprintf(w, "  \tunits\t%s\n", h.Units())
		fmt.Fprintf(w, "  \tpoll\tevery %v\n", h.Period)
		if h.Reading != "" {
			fmt.Fprintf(w, "  \treading\t%s\n", h.Reading)
		}
	}
	w.Flush()

	fmt.Fprintln(out, "\nACCELEROMETERS (IIO)")
	if len(r.Accels) == 0 {
		fmt.Fprintln(out, "  none")
	}
	for _, a := range r.Accels {
		fmt.Fprintf(w, "  %s\tname=%s\t%s\n", a.Path, orNone(a.Name), a.Access)
	}
	w.Flush()

	fmt.Fprintln(out, "\nUINPUT (needed to synthesize a switch)")
	fmt.Fprintf(out, "  /dev/uinput  %s\n", r.Uinput)

	fmt.Fprintf(out, "\nSELECTED MECHANISM\n  %s\n", r.Mechanism)

	// The slew gate is a rate test and goes inert past a known interval, so a
	// slow sensor silently loses that defence. Say so rather than implying a
	// protection that is not active.
	cfg := policy.DefaultConfig()
	if ceil := policy.SlewGateCeiling(cfg); ceil > 0 {
		for _, h := range r.Hinges {
			if h.Period > ceil {
				fmt.Fprintf(out, "\n  note: this sensor polls every %v, past the %v ceiling at which the\n"+
					"        slew-rate glitch filter can still reject anything. Posture safety\n"+
					"        here rests on the wrap guard and corroboration instead.\n",
					h.Period, ceil)
			}
		}
	}

	fmt.Fprintln(out, "\nNOTES")
	for _, n := range r.Notes {
		fmt.Fprintf(out, "  - %s\n", wrap(n, 72, "    "))
	}
}

func orNone(s string) string {
	if s == "" {
		return "(unknown)"
	}
	return s
}

// safeName renders a kernel-supplied device name for a terminal. Names are
// attacker-influenced (uinput and USB product strings both reach this field),
// and doctor output is meant to be pasted into bug reports.
func safeName(s string) string {
	q := strconv.Quote(s)
	return q[1 : len(q)-1]
}

// wrap soft-wraps a note so doctor output stays readable when pasted into an
// issue, without depending on terminal width. It counts runes rather than
// bytes, so a non-ASCII device name does not wrap early.
func wrap(s string, width int, indent string) string {
	var b strings.Builder
	lineLen := 0
	for i, word := range strings.Fields(s) {
		wl := len([]rune(word))
		switch {
		case i == 0:
			b.WriteString(word)
			lineLen = wl
		case lineLen+1+wl > width:
			b.WriteString("\n" + indent + word)
			lineLen = wl
		default:
			b.WriteString(" " + word)
			lineLen += 1 + wl
		}
	}
	return b.String()
}
