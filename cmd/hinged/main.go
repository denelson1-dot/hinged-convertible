// Command hinged reports and controls convertible-laptop posture.
package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/denelson1-dot/hinged-convertible/internal/policy"
	"github.com/denelson1-dot/hinged-convertible/internal/probe"
)

const usage = `hinged - tablet mode for Linux convertibles

Usage:
  hinged doctor    Report what this machine exposes and what hinged would use
  hinged watch     Watch posture live, read-only, without changing anything
  hinged version   Print the version

Run 'hinged doctor' first. Its output is what to paste into a bug report.
`

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "doctor":
		doctor()
	case "watch":
		watch()
	case "version":
		fmt.Println(version)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}

func doctor() {
	r := probe.Run()
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)

	fmt.Println("MACHINE")
	fmt.Fprintf(w, "  vendor\t%s\n", orNone(r.Machine.Vendor))
	fmt.Fprintf(w, "  product\t%s\n", orNone(r.Machine.Product))
	fmt.Fprintf(w, "  chassis\t%s (DMI type %d)\n", r.Machine.ChassisDescription(), r.Machine.ChassisType)
	fmt.Fprintf(w, "  kernel\t%s\n", orNone(r.Machine.Kernel))
	w.Flush()

	fmt.Println("\nSWITCH DEVICES (evdev EV_SW)")
	if len(r.Switches) == 0 {
		fmt.Println("  none")
	}
	for _, s := range r.Switches {
		var caps []string
		if s.TabletMode {
			caps = append(caps, "SW_TABLET_MODE")
		}
		if s.Lid {
			caps = append(caps, "SW_LID")
		}
		fmt.Fprintf(w, "  %s\t%s\t%v\t%s\n", s.Handler, s.Name, caps, s.Access)
	}
	w.Flush()

	fmt.Println("\nHINGE SENSORS (IIO)")
	if len(r.Hinges) == 0 {
		fmt.Println("  none")
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

	fmt.Println("\nACCELEROMETERS (IIO)")
	if len(r.Accels) == 0 {
		fmt.Println("  none")
	}
	for _, a := range r.Accels {
		fmt.Fprintf(w, "  %s\tname=%s\t%s\n", a.Path, orNone(a.Name), a.Access)
	}
	w.Flush()

	// The slew gate is a rate test and goes inert past a known interval, so a
	// slow sensor silently loses that defence. Say so rather than implying a
	// protection that is not active.
	cfg := policy.DefaultConfig()
	if ceil := policy.SlewGateCeiling(cfg); ceil > 0 {
		for _, h := range r.Hinges {
			if h.Period > ceil {
				fmt.Printf("\n  note: this sensor polls every %v, beyond the %v ceiling at which\n"+
					"        the slew-rate glitch filter can still reject anything. Posture\n"+
					"        safety here rests on the wrap guard and corroboration instead.\n",
					h.Period, ceil)
			}
		}
	}
	w.Flush()

	fmt.Println("\nUINPUT (needed to synthesize a switch)")
	fmt.Printf("  /dev/uinput  %s\n", r.Uinput)

	fmt.Printf("\nSELECTED MECHANISM\n  %s\n", r.Mechanism)

	fmt.Println("\nNOTES")
	for _, n := range r.Notes {
		fmt.Printf("  - %s\n", wrap(n, 76, "    "))
	}
}

func orNone(s string) string {
	if s == "" {
		return "(unknown)"
	}
	return s
}

// wrap soft-wraps a note so doctor output stays readable when pasted into an
// issue, without depending on the terminal width.
func wrap(s string, width int, indent string) string {
	var out, line string
	for _, word := range splitWords(s) {
		if line == "" {
			line = word
			continue
		}
		if len(line)+1+len(word) > width {
			out += line + "\n" + indent
			line = word
			continue
		}
		line += " " + word
	}
	return out + line
}

func splitWords(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ' ' || r == '\n' || r == '\t' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
