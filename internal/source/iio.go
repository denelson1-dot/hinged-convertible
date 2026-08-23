// Package source reads posture signals from the kernel.
//
// Sources produce readings and nothing else. They own the messiness of sysfs
// layouts, unit conversion, and device enumeration so that the policy package
// can stay pure.
package source

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Hinge reads a hinge angle from the IIO subsystem.
type Hinge struct {
	dir     string
	raw     string
	channel int
	scale   float64 // 0 means "no scale attribute"; raw is then already degrees
	offset  float64
	period  time.Duration
}

// OpenHinge finds a hinge-angle sensor.
//
// Devices are matched by their name attribute and channels by their label,
// never by index: IIO device numbering is not stable across boots, and a
// hinge sensor commonly exposes several angle channels. On the HP ENVY x360
// the same device reports hinge, screen and keyboard angles as channels 0, 1
// and 2, and only the first is the lid-to-base angle we want.
func OpenHinge() (*Hinge, error) {
	dirs, _ := filepath.Glob("/sys/bus/iio/devices/iio:device*")
	for _, d := range dirs {
		if strings.TrimSpace(readFile(filepath.Join(d, "name"))) != "hinge" {
			continue
		}
		ch, err := findHingeChannel(d)
		if err != nil {
			return nil, err
		}
		h := &Hinge{
			dir:     d,
			channel: ch,
			raw:     filepath.Join(d, fmt.Sprintf("in_angl%d_raw", ch)),
		}
		// Scale and offset are per-device on this hardware, not per-channel.
		// Fall back to a per-channel name for firmware that does it the other
		// way; absence of both is meaningful and handled in Degrees.
		if v, ok := parseFloat(firstNonEmpty(
			readFile(filepath.Join(d, "in_angl_scale")),
			readFile(filepath.Join(d, fmt.Sprintf("in_angl%d_scale", ch))),
		)); ok {
			h.scale = v
		}
		if v, ok := parseFloat(firstNonEmpty(
			readFile(filepath.Join(d, "in_angl_offset")),
			readFile(filepath.Join(d, fmt.Sprintf("in_angl%d_offset", ch))),
		)); ok {
			h.offset = v
		}
		h.period = samplingPeriod(d)

		if _, err := h.Degrees(); err != nil {
			return nil, fmt.Errorf("hinge sensor at %s is unreadable: %w", h.raw, err)
		}
		return h, nil
	}
	return nil, fmt.Errorf("no IIO device with name %q found", "hinge")
}

// findHingeChannel picks the angle channel labelled "hinge", falling back to
// channel 0 when the firmware publishes no labels at all.
func findHingeChannel(dir string) (int, error) {
	for i := 0; i < 8; i++ {
		label := strings.TrimSpace(readFile(filepath.Join(dir, fmt.Sprintf("in_angl%d_label", i))))
		if label == "hinge" {
			return i, nil
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "in_angl0_raw")); err == nil {
		return 0, nil
	}
	return 0, fmt.Errorf("no hinge angle channel found under %s", dir)
}

// samplingPeriod derives a polling interval from the sensor's own advertised
// rate, rather than hardcoding one. Sampling at twice the sensor's rate is
// enough to catch every update without burning CPU: the HP ENVY reports
// 10 Hz, so polling faster than 20 Hz only re-reads unchanged values.
func samplingPeriod(dir string) time.Duration {
	hz, ok := parseFloat(readFile(filepath.Join(dir, "in_angl_sampling_frequency")))
	if !ok || hz <= 0 {
		return 100 * time.Millisecond
	}
	return time.Duration(float64(time.Second) / (hz * 2))
}

// Path returns the sysfs attribute being read, for diagnostics.
func (h *Hinge) Path() string { return h.raw }

// Period returns the recommended polling interval for this sensor.
func (h *Hinge) Period() time.Duration { return h.period }

// Describe summarises the unit conversion, so a wrong scale is visible in
// diagnostics rather than silently scaling every threshold.
func (h *Hinge) Describe() string {
	if h.scale == 0 {
		return fmt.Sprintf("channel %d, no scale attribute; raw values treated as degrees", h.channel)
	}
	return fmt.Sprintf("channel %d, degrees = (raw + %g) * %g * 180/pi", h.channel, h.offset, h.scale)
}

// readSysfsRaw reads a sysfs attribute using raw syscalls rather than the os
// package.
//
// This is not premature optimisation, it is a partial correctness fix.
//
// Reading a HID-sensor IIO attribute triggers a round trip to the sensor hub,
// and the driver intermittently answers 0 instead of waiting for the real
// value. Go's os package makes this dramatically worse: it registers pollable
// descriptors with the runtime poller and opens them non-blocking, so it takes
// the early zero far more often.
//
// Measured on an HP ENVY x360 at 20 samples/second with the hinge stationary
// at 110 degrees:
//
//	os.ReadFile        3.7%  bad reads  (11 of 293)
//	syscall.Open+Read  0.24% bad reads  (1 of 418)
//
// So raw syscalls reduce the glitch rate roughly fifteen-fold but do NOT
// eliminate it. That residue is why the policy layer keeps a slew gate: a
// spurious zero is indistinguishable from a fully folded hinge, and acting on
// one would switch the keyboard off at random. Both defences are required, and
// neither is sufficient alone.
func readSysfsRaw(path string) (string, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY, 0)
	if err != nil {
		return "", &os.PathError{Op: "open", Path: path, Err: err}
	}
	defer syscall.Close(fd)

	var buf [64]byte
	n, err := syscall.Read(fd, buf[:])
	if err != nil {
		return "", &os.PathError{Op: "read", Path: path, Err: err}
	}
	return string(buf[:n]), nil
}

// Degrees reads the current angle.
func (h *Hinge) Degrees() (float64, error) {
	s, err := readSysfsRaw(h.raw)
	if err != nil {
		return 0, err
	}
	text := strings.TrimSpace(s)
	raw, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return 0, fmt.Errorf("unparsable hinge reading %q from %s: %w", text, h.raw, err)
	}
	// The IIO ABI defines angle channels in radians once scale and offset are
	// applied. Some firmware publishes no scale and reports whole degrees
	// directly. Guessing wrong scales every threshold by 57x, so the two cases
	// are handled explicitly rather than assumed.
	if h.scale == 0 {
		return raw + h.offset, nil
	}
	return (raw + h.offset) * h.scale * 180 / math.Pi, nil
}

// Close is a no-op; no handle is retained. It exists so callers can treat the
// sensor like any other resource.
func (h *Hinge) Close() error { return nil }

func readFile(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return string(b)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func parseFloat(s string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
