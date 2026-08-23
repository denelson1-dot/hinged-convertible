//go:build linux

// Package source reads posture signals from the kernel.
//
// Sources own the messiness of sysfs layouts, unit conversion and device
// enumeration, so that the policy package can stay pure.
package source

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Angle bounds used to sanity-check a converted reading.
//
// A hinge cannot report outside roughly one full turn. Anything else is a
// driver sentinel, a unit-conversion mistake, or garbage, and acting on it
// would switch the keyboard off for the wrong reason.
const (
	minPlausibleAngle = -5
	maxPlausibleAngle = 365

	// crosECUnreliable is the value the ChromeOS EC reports when it cannot
	// compute a lid angle. cros_ec_lid_angle passes it through to userspace
	// unfiltered, and it is well above any tablet threshold, so it must be
	// recognised rather than believed.
	crosECUnreliable = 500
)

// Polling bounds. A sensor's advertised rate is a hint, not a contract:
// firmware has been seen advertising rates low enough to miss a fold entirely
// and high enough to saturate the sensor hub with synchronous round trips.
const (
	minPollPeriod     = 20 * time.Millisecond
	maxPollPeriod     = 250 * time.Millisecond
	defaultPollPeriod = 100 * time.Millisecond
)

// HingeInfo describes a discovered hinge-angle sensor without opening it.
// Both the probe and the daemon use this, so they can never disagree about
// what counts as a hinge sensor or how its units are interpreted.
type HingeInfo struct {
	Dir      string // IIO device directory
	Name     string // contents of the `name` attribute
	Raw      string // full path to the raw angle attribute
	Channel  int    // -1 when the attribute is not indexed
	Label    string
	Scale    float64
	HasScale bool
	Offset   float64
	Period   time.Duration
}

// Units describes how a raw value becomes degrees, so that a wrong conversion
// is visible in diagnostics instead of silently scaling every threshold.
func (h HingeInfo) Units() string {
	switch {
	case !h.HasScale:
		return "no scale attribute; raw values treated as degrees"
	case h.Scale == 1:
		return "scale is exactly 1.0, which the kernel also uses when it cannot " +
			"identify the unit; raw values treated as degrees"
	default:
		return fmt.Sprintf("degrees = (raw + %g) * %g * 180/pi", h.Offset, h.Scale)
	}
}

// DiscoverHinges returns every IIO device that reports a hinge angle.
//
// Matching is deliberately broad. Two quite different drivers are in use:
//
//   - hid-sensor-custom-intel-hinge names the device "hinge" and publishes
//     three indexed channels labelled hinge, screen and keyboard.
//   - cros_ec_lid_angle names the device "cros-ec-lid-angle", publishes a
//     single unindexed in_angl_raw, and has no label and no scale at all.
//
// Devices are matched by name and channels by label, never by index: IIO
// device numbering is not stable across boots and channel order is a
// per-driver convention rather than an ABI guarantee.
func DiscoverHinges() []HingeInfo {
	dirs, _ := filepath.Glob("/sys/bus/iio/devices/iio:device*")
	sort.Strings(dirs)

	var out []HingeInfo
	for _, d := range dirs {
		name := strings.TrimSpace(readFile(filepath.Join(d, "name")))
		raw, channel, label, ok := findAngleChannel(d, name)
		if !ok {
			continue
		}
		h := HingeInfo{
			Dir: d, Name: name, Raw: raw, Channel: channel, Label: label,
			Period: samplingPeriod(d, channel),
		}
		// Scale and offset are usually shared by the whole device rather than
		// published per channel, because the driver declares them in
		// info_mask_shared_by_type. Check the shared name first; fall back to
		// the indexed one for drivers that do it the other way.
		h.Scale, h.HasScale = firstFloat(
			filepath.Join(d, "in_angl_scale"),
			indexed(d, "in_angl%s_scale", channel),
		)
		h.Offset, _ = firstFloat(
			filepath.Join(d, "in_angl_offset"),
			indexed(d, "in_angl%s_offset", channel),
		)
		out = append(out, h)
	}
	return out
}

// findAngleChannel locates the angle attribute for a device, preferring a
// channel explicitly labelled "hinge" when the driver publishes labels.
func findAngleChannel(dir, name string) (raw string, channel int, label string, ok bool) {
	// Indexed channels, as published by the Intel ISH hinge driver.
	matches, _ := filepath.Glob(filepath.Join(dir, "in_angl[0-9]_raw"))
	sort.Strings(matches)
	for _, m := range matches {
		idx, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(filepath.Base(m), "in_angl"), "_raw"))
		if err != nil {
			continue
		}
		l := strings.TrimSpace(readFile(indexed(dir, "in_angl%s_label", idx)))
		if l == "hinge" {
			return m, idx, l, true
		}
	}
	// A single unlabelled indexed channel is unambiguous even without a label.
	if len(matches) == 1 && isHingeName(name) {
		return matches[0], 0, "", true
	}
	// Unindexed channel, as published by cros_ec_lid_angle.
	if p := filepath.Join(dir, "in_angl_raw"); fileExists(p) && isHingeName(name) {
		return p, -1, "", true
	}
	// Several unlabelled channels and no other evidence: refuse to guess.
	// Picking channel 0 would be a coin flip between the hinge angle and the
	// screen or keyboard angle relative to the ground.
	return "", 0, "", false
}

// isHingeName reports whether an IIO device name is a known hinge-angle
// provider. Kept small and explicit: matching too loosely here means reading
// an unrelated angle sensor and disabling the keyboard from it.
func isHingeName(name string) bool {
	switch name {
	case "hinge", "cros-ec-lid-angle":
		return true
	}
	return false
}

// Hinge reads a hinge angle from the IIO subsystem.
type Hinge struct{ info HingeInfo }

// OpenHinge finds and validates a hinge-angle sensor.
func OpenHinge() (*Hinge, error) {
	found := DiscoverHinges()
	if len(found) == 0 {
		return nil, fmt.Errorf("no IIO hinge-angle sensor found")
	}
	// Try every candidate, and retry the validation read a few times. At the
	// measured glitch rate a single bad read at startup would otherwise mean
	// no hinge source at all until the daemon is restarted.
	var lastErr error
	for _, info := range found {
		h := &Hinge{info: info}
		for attempt := 0; attempt < 3; attempt++ {
			if _, err := h.Degrees(); err == nil {
				return h, nil
			} else {
				lastErr = err
			}
		}
	}
	return nil, fmt.Errorf("no usable hinge sensor among %d candidate(s): %w", len(found), lastErr)
}

// Info returns the discovered sensor description.
func (h *Hinge) Info() HingeInfo { return h.info }

// Path returns the sysfs attribute being read, for diagnostics.
func (h *Hinge) Path() string { return h.info.Raw }

// Period returns the recommended polling interval.
func (h *Hinge) Period() time.Duration { return h.info.Period }

// Describe summarises the unit conversion.
func (h *Hinge) Describe() string { return h.info.Units() }

// readSysfsRaw reads a sysfs attribute using raw syscalls rather than the os
// package.
//
// Reading a HID-sensor IIO attribute triggers a round trip to the sensor hub,
// and the driver intermittently answers 0 instead of waiting for the real
// value. Reading through the os package is measurably worse than issuing the
// syscalls directly. Interleaved A/B at 20 samples/second with the hinge
// stationary:
//
//	os.ReadFile        ~3%   bad reads
//	syscall.Open+Read  ~0.3% bad reads
//
// The mechanism is NOT known. An earlier version of this comment blamed
// O_NONBLOCK and the runtime poller. That was tested and is wrong: an
// os.NewFile wrapper around a plain blocking descriptor, never registered with
// the poller, is affected just as badly. Something in the os.File read path is
// responsible; this project has not identified what. The effect replicates,
// the cause is open.
//
// Raw syscalls cut the rate by roughly an order of magnitude but do not
// eliminate it. That residue is why the policy layer refuses to read a
// near-zero angle as a fold unless the physical path supports it. Both
// defences are required and neither is sufficient alone.
func readSysfsRaw(path string) (string, error) {
	// O_CLOEXEC because this opens and closes at up to 20 Hz, and any exec
	// from another goroutine during that window would otherwise inherit the
	// descriptor. The os package always sets it; the raw path must too.
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return "", &os.PathError{Op: "open", Path: path, Err: err}
	}
	defer syscall.Close(fd)

	// Retry on EINTR. Go's stdlib wraps every syscall in ignoringEINTR; a raw
	// call has to do it explicitly or a signal arriving mid-read surfaces as a
	// spurious sensor failure.
	var buf [64]byte
	for {
		n, err := syscall.Read(fd, buf[:])
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return "", &os.PathError{Op: "read", Path: path, Err: err}
		}
		return string(buf[:n]), nil
	}
}

// ErrUnreliable reports a reading the hardware itself has flagged as invalid,
// as distinct from one we failed to read or parse.
type ErrUnreliable struct{ Raw float64 }

func (e *ErrUnreliable) Error() string {
	return fmt.Sprintf("sensor reported %g, its sentinel for an unreliable angle", e.Raw)
}

// Degrees reads the current angle, converted and range-checked.
func (h *Hinge) Degrees() (float64, error) {
	s, err := readSysfsRaw(h.info.Raw)
	if err != nil {
		return 0, err
	}
	text := strings.TrimSpace(s)
	raw, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return 0, fmt.Errorf("unparsable hinge reading %q from %s: %w", text, h.info.Raw, err)
	}
	if raw == crosECUnreliable {
		return 0, &ErrUnreliable{Raw: raw}
	}

	deg := h.convert(raw)
	if math.IsNaN(deg) || math.IsInf(deg, 0) || deg < minPlausibleAngle || deg > maxPlausibleAngle {
		return 0, fmt.Errorf("hinge reading %g converts to %g degrees, outside the plausible range; "+
			"the unit conversion is probably wrong (%s)", raw, deg, h.info.Units())
	}
	return deg, nil
}

// convert applies the IIO unit conversion.
//
// The IIO ABI defines angle channels in radians once scale and offset are
// applied. Two cases deliberately bypass that:
//
//   - No scale attribute at all, as with cros_ec_lid_angle, which reports
//     whole degrees directly.
//   - A scale of exactly 1.0. The kernel initialises scale to 1 and only
//     overwrites it on a hit in its unit table, so 1.0 also means "the unit
//     was not recognised". Believing it would turn 110 degrees into 6303.
func (h *Hinge) convert(raw float64) float64 {
	if !h.info.HasScale || h.info.Scale == 1 {
		return raw + h.info.Offset
	}
	return (raw + h.info.Offset) * h.info.Scale * 180 / math.Pi
}

// Close is a no-op; no handle is retained.
func (h *Hinge) Close() error { return nil }

// samplingPeriod derives a polling interval from the sensor's advertised rate,
// clamped to a sane range.
//
// Sampling at twice the sensor's rate catches every update without waste. The
// clamp matters: firmware advertising 0.1 Hz would otherwise mean a five
// second wait to notice a fold, and firmware advertising 1 kHz would mean two
// thousand synchronous sensor-hub round trips a second on an interface already
// known to glitch under load.
func samplingPeriod(dir string, channel int) time.Duration {
	hz, ok := firstFloat(
		filepath.Join(dir, "in_angl_sampling_frequency"),
		indexed(dir, "in_angl%s_sampling_frequency", channel),
		filepath.Join(dir, "sampling_frequency"),
	)
	if !ok || hz <= 0 || math.IsInf(hz, 0) || math.IsNaN(hz) {
		return defaultPollPeriod
	}
	return clampPeriod(time.Duration(float64(time.Second) / (hz * 2)))
}

func clampPeriod(d time.Duration) time.Duration {
	if d < minPollPeriod {
		return minPollPeriod
	}
	if d > maxPollPeriod {
		return maxPollPeriod
	}
	return d
}

// indexed builds an attribute path, handling the unindexed form used by
// drivers whose channel is not numbered.
func indexed(dir, pattern string, channel int) string {
	if channel < 0 {
		return filepath.Join(dir, fmt.Sprintf(pattern, ""))
	}
	return filepath.Join(dir, fmt.Sprintf(pattern, strconv.Itoa(channel)))
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func readFile(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return string(b)
}

// firstFloat returns the first of several candidate attributes that parses.
func firstFloat(paths ...string) (float64, bool) {
	for _, p := range paths {
		if v, err := strconv.ParseFloat(strings.TrimSpace(readFile(p)), 64); err == nil {
			return v, true
		}
	}
	return 0, false
}

// DiscoverAccelerometers returns IIO accelerometers, for the dual-accelerometer
// angle derivation used on chassis with no hinge sensor.
//
// Detection is by capability, not by name. Real accelerometer drivers are
// named for their chip -- kxcjk1013, mxc4005, bmi160, lis2dw12 -- and none of
// those contain the word "accel". The presence of in_accel_x_raw is the ABI
// guarantee.
func DiscoverAccelerometers() []string {
	dirs, _ := filepath.Glob("/sys/bus/iio/devices/iio:device*")
	sort.Strings(dirs)
	var out []string
	for _, d := range dirs {
		if fileExists(filepath.Join(d, "in_accel_x_raw")) {
			out = append(out, d)
		}
	}
	return out
}
