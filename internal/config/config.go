//go:build linux

// Package config loads hinged's settings from XDG locations.
//
// Everything is optional. Deleting the file entirely must leave a working
// installation, because a tool whose defaults do not work is a tool nobody
// gets past the first five minutes with.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/denelson1-dot/hinged-convertible/internal/hooks"
	"github.com/denelson1-dot/hinged-convertible/internal/uinput"
	"github.com/denelson1-dot/hinged-convertible/policy"
)

// File is the parsed configuration.
type File struct {
	Policy policy.Config
	Uinput uinput.Config
	Hooks  []hooks.Hook
	Source string // where it came from, for diagnostics
}

// Default returns a working configuration with no file present.
func Default() File {
	return File{
		Policy: policy.DefaultConfig(),
		Uinput: uinput.DefaultConfig(),
		Source: "built-in defaults",
	}
}

// Path returns the config file location, honouring XDG_CONFIG_HOME.
func Path() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "hinged", "config.toml")
}

// Load reads the config file if present.
//
// A missing file is not an error. A malformed one is: silently falling back to
// defaults would mean a user's carefully tuned thresholds vanish because of a
// typo, and they would have no way to tell.
func Load(path string) (File, error) {
	f := Default()
	if path == "" {
		path = Path()
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	if err := parse(string(data), &f); err != nil {
		return f, fmt.Errorf("%s: %w", path, err)
	}
	f.Source = path
	if err := f.Policy.Validate(); err != nil {
		return f, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

// parse reads a deliberately small subset of TOML: `key = value` under
// `[section]` headers, plus `[[hooks]]` tables.
//
// Hand-rolled to keep the core dependency-free. Unknown keys are rejected
// rather than ignored, because a silent typo in a file that decides whether
// someone's keyboard switches off is not an acceptable failure mode.
func parse(text string, f *File) error {
	section := ""
	var pending *hooks.Hook

	flush := func() {
		if pending != nil {
			f.Hooks = append(f.Hooks, *pending)
			pending = nil
		}
	}

	for n, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if line == "[[hooks]]" {
			flush()
			pending = &hooks.Hook{Event: "any"}
			section = "hooks"
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			flush()
			section = strings.Trim(line, "[]")
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("line %d: expected key = value, got %q", n+1, line)
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if err := assign(f, section, key, val, pending); err != nil {
			return fmt.Errorf("line %d: %w", n+1, err)
		}
	}
	flush()
	return nil
}

func assign(f *File, section, key, val string, hook *hooks.Hook) error {
	switch section {
	case "posture":
		return assignPosture(&f.Policy, key, val)
	case "uinput":
		return assignUinput(&f.Uinput, key, val)
	case "hooks":
		if hook == nil {
			return fmt.Errorf("key %q outside a [[hooks]] table", key)
		}
		return assignHook(hook, key, val)
	default:
		return fmt.Errorf("unknown section %q", section)
	}
}

func assignPosture(c *policy.Config, key, val string) error {
	nums := map[string]*float64{
		"enter_angle": &c.TentMin, "leave_angle": &c.LaptopMax,
		"wrap_guard": &c.WrapGuard, "tablet_angle": &c.TabletMin,
		"max_slew_rate": &c.MaxSlewRate, "leave_samples": &c.LeaveSamples,
	}
	if p, ok := nums[key]; ok {
		v, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		*p = v
		return nil
	}
	ints := map[string]*int{
		"enter_samples": &c.EnterSamples, "ambiguous_samples": &c.AmbiguousSamples,
	}
	if p, ok := ints[key]; ok {
		v, err := strconv.Atoi(val)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		*p = v
		return nil
	}
	durs := map[string]*time.Duration{
		"sensor_lost_grace": &c.SensorLostGrace, "stale_after": &c.StaleAfter,
	}
	if p, ok := durs[key]; ok {
		d, err := time.ParseDuration(unquote(val))
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		*p = d
		return nil
	}
	return fmt.Errorf("unknown key %q in [posture]", key)
}

func assignUinput(c *uinput.Config, key, val string) error {
	switch key {
	case "name":
		c.Name = unquote(val)
	case "bustype":
		switch unquote(val) {
		case "virtual":
			c.BusType = uinput.BusVirtual
		case "host":
			c.BusType = uinput.BusHost
		case "i8042":
			c.BusType = uinput.BusI8042
		default:
			return fmt.Errorf("bustype: want virtual, host or i8042")
		}
	case "vendor", "product", "version":
		v, err := strconv.ParseUint(strings.TrimPrefix(unquote(val), "0x"), 0, 16)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		switch key {
		case "vendor":
			c.Vendor = uint16(v)
		case "product":
			c.Product = uint16(v)
		case "version":
			c.Version = uint16(v)
		}
	case "include_lid":
		c.WithLid = val == "true"
	default:
		return fmt.Errorf("unknown key %q in [uinput]", key)
	}
	return nil
}

func assignHook(h *hooks.Hook, key, val string) error {
	switch key {
	case "event":
		h.Event = unquote(val)
	case "command":
		parts, err := parseArray(val)
		if err != nil {
			return fmt.Errorf("command: %w", err)
		}
		h.Command = parts
	case "timeout":
		d, err := time.ParseDuration(unquote(val))
		if err != nil {
			return fmt.Errorf("timeout: %w", err)
		}
		h.Timeout = d
	case "async":
		h.Async = val == "true"
	case "ignore_exit":
		h.IgnoreExit = val == "true"
	default:
		return fmt.Errorf("unknown key %q in [[hooks]]", key)
	}
	return nil
}

// parseArray reads ["a", "b"]. Commands are argv arrays and never shell
// strings, so nothing a hook is given can be reinterpreted as syntax.
func parseArray(val string) ([]string, error) {
	val = strings.TrimSpace(val)
	if !strings.HasPrefix(val, "[") || !strings.HasSuffix(val, "]") {
		return nil, fmt.Errorf(`want an array like ["cmd", "arg"]`)
	}
	inner := strings.TrimSpace(val[1 : len(val)-1])
	if inner == "" {
		return nil, nil
	}
	var out []string
	for _, p := range splitTopLevel(inner) {
		p = strings.TrimSpace(p)
		if len(p) < 2 || !strings.HasPrefix(p, `"`) || !strings.HasSuffix(p, `"`) {
			return nil, fmt.Errorf("array elements must be quoted strings, got %q", p)
		}
		out = append(out, p[1:len(p)-1])
	}
	return out, nil
}

// splitTopLevel splits on commas that are not inside quotes, so an argument
// containing a comma survives intact.
func splitTopLevel(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			cur.WriteRune(r)
		case r == ',' && !inQuote:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && strings.HasPrefix(s, `"`) && strings.HasSuffix(s, `"`) {
		return s[1 : len(s)-1]
	}
	return s
}
