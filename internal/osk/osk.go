//go:build linux

// Package osk drives whichever on-screen keyboard a machine actually has.
//
// hinged deliberately does not ship a keyboard. A good OSK is a large project
// on its own -- layouts, locales, modifiers, gestures -- and several
// well-maintained ones already exist. What is missing on a convertible is
// something that knows when to show one, which is what this provides.
package osk

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Provider is one on-screen keyboard hinged knows how to drive.
type Provider struct {
	Name string

	// Probe reports whether this provider is usable here.
	Probe func() bool

	// Show and Hide are argv, never shell strings.
	Show []string
	Hide []string

	// PreHide runs before Hide. Onboard needs this: its auto-show setting must
	// be turned off first, or the focused text field immediately reopens the
	// keyboard the instant it is hidden.
	PreHide [][]string

	// Notes explains anything surprising, for `hinged doctor`.
	Notes string
}

// Providers lists everything hinged can drive, in preference order.
//
// GNOME and KDE come first and deliberately do nothing: their shells already
// react to SW_TABLET_MODE, which hinged now supplies, so showing a keyboard
// ourselves would fight the desktop rather than help it.
func Providers() []Provider {
	return []Provider{
		{
			Name:  "gnome-builtin",
			Probe: func() bool { return desktopIs("gnome") },
			Notes: "GNOME shows its own keyboard in response to SW_TABLET_MODE; hinged supplies the switch and stays out of the way",
		},
		{
			Name:  "kde-builtin",
			Probe: func() bool { return desktopIs("kde") || desktopIs("plasma") },
			Notes: "KWin reacts to tablet mode on its own; hinged supplies the switch and stays out of the way",
		},
		{
			Name:  "wvkbd",
			Probe: func() bool { return hasBinary("wvkbd-mobintl") },
			Show:  []string{"wvkbd-mobintl", "-L", "320"},
			Hide:  []string{"pkill", "-x", "wvkbd-mobintl"},
			Notes: "wlroots compositors (sway, Hyprland)",
		},
		{
			Name:  "squeekboard",
			Probe: func() bool { return hasBinary("squeekboard") },
			Show:  []string{"busctl", "--user", "set-property", "sm.puri.OSK0", "/sm/puri/OSK0", "sm.puri.OSK0", "Visible", "b", "true"},
			Hide:  []string{"busctl", "--user", "set-property", "sm.puri.OSK0", "/sm/puri/OSK0", "sm.puri.OSK0", "Visible", "b", "false"},
		},
		{
			Name:  "onboard",
			Probe: func() bool { return hasBinary("onboard") },
			Show:  []string{"onboard"},
			Hide:  []string{"pkill", "-KILL", "-x", "onboard"},
			PreHide: [][]string{
				// Order matters: with auto-show still enabled, hiding Onboard
				// while a text field has focus makes it reappear immediately.
				{"gsettings", "set", "org.onboard.auto-show", "enabled", "false"},
			},
			// SIGKILL rather than SIGTERM: Onboard's language-model destructor
			// crashes on clean shutdown under Python 3.12 and leaves a ~9 MB
			// core dump behind each time.
			Notes: "X11; killed with SIGKILL to avoid a crashing destructor",
		},
	}
}

// Controller shows and hides a chosen provider.
type Controller struct {
	p       Provider
	visible bool
}

// Detect picks the first usable provider.
func Detect() (*Controller, error) {
	for _, p := range Providers() {
		if p.Probe != nil && p.Probe() {
			return &Controller{p: p}, nil
		}
	}
	return nil, fmt.Errorf("no on-screen keyboard found; install one of: wvkbd, squeekboard, onboard")
}

// ByName selects a provider explicitly.
func ByName(name string) (*Controller, error) {
	if name == "none" {
		return &Controller{p: Provider{Name: "none"}}, nil
	}
	for _, p := range Providers() {
		if p.Name == name {
			return &Controller{p: p}, nil
		}
	}
	return nil, fmt.Errorf("unknown on-screen keyboard %q", name)
}

// Name returns the selected provider.
func (c *Controller) Name() string { return c.p.Name }

// Notes returns anything worth telling the user about this provider.
func (c *Controller) Notes() string { return c.p.Notes }

// DrivesItself reports whether the desktop reacts to SW_TABLET_MODE on its
// own, in which case hinged should not also drive a keyboard.
func (c *Controller) DrivesItself() bool { return len(c.p.Show) == 0 }

// Show displays the keyboard.
func (c *Controller) Show(ctx context.Context) error {
	if len(c.p.Show) == 0 {
		return nil
	}
	if err := run(ctx, c.p.Show); err != nil {
		return fmt.Errorf("showing %s: %w", c.p.Name, err)
	}
	c.visible = true
	return nil
}

// Hide dismisses the keyboard, running any pre-hide steps first.
func (c *Controller) Hide(ctx context.Context) error {
	if len(c.p.Hide) == 0 {
		return nil
	}
	for _, pre := range c.p.PreHide {
		_ = run(ctx, pre)
	}
	// pkill returns 1 when nothing matched, which is success for our purposes.
	_ = run(ctx, c.p.Hide)
	c.visible = false
	return nil
}

// Visible reports the last known state.
func (c *Controller) Visible() bool { return c.visible }

func run(ctx context.Context, argv []string) error {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	// Detach anything long-lived so it survives this call.
	if argv[0] == "onboard" || strings.HasPrefix(argv[0], "wvkbd") {
		return cmd.Start()
	}
	return cmd.Run()
}

func hasBinary(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func desktopIs(want string) bool {
	for _, v := range []string{os.Getenv("XDG_CURRENT_DESKTOP"), os.Getenv("XDG_SESSION_DESKTOP")} {
		if strings.Contains(strings.ToLower(v), want) {
			return true
		}
	}
	return false
}
