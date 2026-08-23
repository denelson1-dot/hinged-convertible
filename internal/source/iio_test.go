//go:build linux

package source

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeIIO builds a sysfs-shaped directory so the layout handling can be tested
// without hardware. Values are the ones real drivers publish.
func fakeIIO(t *testing.T, attrs map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, val := range attrs {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(val+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The Intel ISH hinge driver: three indexed channels, labelled, with a
// device-wide scale of exactly pi/180.
func TestFindAngleChannelPrefersLabelledHinge(t *testing.T) {
	dir := fakeIIO(t, map[string]string{
		"name":         "hinge",
		"in_angl0_raw": "110", "in_angl0_label": "hinge",
		"in_angl1_raw": "106", "in_angl1_label": "screen",
		"in_angl2_raw": "356", "in_angl2_label": "keyboard",
		"in_angl_scale":  "0.017453293",
		"in_angl_offset": "0",
	})
	raw, ch, label, ok := findAngleChannel(dir, "hinge")
	if !ok {
		t.Fatal("labelled hinge channel not found")
	}
	if ch != 0 || label != "hinge" {
		t.Errorf("channel=%d label=%q, want 0/hinge", ch, label)
	}
	if filepath.Base(raw) != "in_angl0_raw" {
		t.Errorf("raw = %q", raw)
	}
}

// Selecting by index rather than label would pick the wrong angle whenever a
// driver orders its channels differently.
func TestFindAngleChannelIgnoresIndexOrder(t *testing.T) {
	dir := fakeIIO(t, map[string]string{
		"name":         "hinge",
		"in_angl0_raw": "106", "in_angl0_label": "screen",
		"in_angl1_raw": "110", "in_angl1_label": "hinge",
	})
	_, ch, _, ok := findAngleChannel(dir, "hinge")
	if !ok || ch != 1 {
		t.Errorf("channel = %d (ok=%v), want 1 — the label must win over the index", ch, ok)
	}
}

// cros_ec_lid_angle: unindexed attribute, no label, no scale. Framework 12 and
// Chromebook convertibles look like this.
func TestFindAngleChannelHandlesUnindexedCrosEC(t *testing.T) {
	dir := fakeIIO(t, map[string]string{
		"name": "cros-ec-lid-angle", "in_angl_raw": "95",
	})
	raw, ch, _, ok := findAngleChannel(dir, "cros-ec-lid-angle")
	if !ok {
		t.Fatal("unindexed cros-ec angle channel not found")
	}
	if ch != -1 || filepath.Base(raw) != "in_angl_raw" {
		t.Errorf("channel=%d raw=%q, want -1/in_angl_raw", ch, raw)
	}
}

// Several unlabelled channels is a coin flip between the hinge angle and the
// screen or keyboard angle. Refusing is better than guessing.
func TestFindAngleChannelRefusesToGuess(t *testing.T) {
	dir := fakeIIO(t, map[string]string{
		"name": "hinge", "in_angl0_raw": "1", "in_angl1_raw": "2", "in_angl2_raw": "3",
	})
	if _, _, _, ok := findAngleChannel(dir, "hinge"); ok {
		t.Error("picked a channel with no label and no way to tell them apart")
	}
}

func TestFindAngleChannelIgnoresUnrelatedDevices(t *testing.T) {
	dir := fakeIIO(t, map[string]string{"name": "accel_3d", "in_angl_raw": "42"})
	if _, _, _, ok := findAngleChannel(dir, "accel_3d"); ok {
		t.Error("an unrelated device with an angle channel was treated as a hinge")
	}
}

// The 57x error: getting this wrong scales every threshold.
func TestConvertUnits(t *testing.T) {
	tests := []struct {
		name        string
		scale       float64
		hasScale    bool
		offset, raw float64
		want        float64
	}{
		{"pi/180 scale means raw is already degrees", 0.017453293, true, 0, 110, 110},
		{"no scale attribute means raw is degrees", 0, false, 0, 95, 95},
		// The kernel initialises scale to 1 and only overwrites it on a hit in
		// its unit table, so 1.0 also means "unit not recognised". Believing it
		// would turn 110 degrees into 6303.
		{"scale of exactly 1 is not trusted", 1, true, 0, 110, 110},
		{"offset is applied", 0, false, 5, 100, 105},
		{"true radian scale converts", 1.0 / 180 * math.Pi, true, 0, math.Pi / 2 * 180 / math.Pi, 90},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := &Hinge{info: HingeInfo{Scale: tc.scale, HasScale: tc.hasScale, Offset: tc.offset}}
			if got := h.convert(tc.raw); math.Abs(got-tc.want) > 0.01 {
				t.Errorf("convert(%v) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

// Firmware advertising an absurd rate must not produce a 5-second poll (too
// slow to notice a fold) or a 500us one (2000 sensor-hub round trips a second).
func TestSamplingPeriodIsClamped(t *testing.T) {
	tests := []struct{ hz, want string }{
		{"10.000000", "50ms"},
		{"0.100000", "250ms"},
		{"1000.000000", "20ms"},
		{"NaN", "100ms"},
		{"Inf", "100ms"},
		{"0", "100ms"},
		{"-5", "100ms"},
		{"garbage", "100ms"},
	}
	for _, tc := range tests {
		dir := fakeIIO(t, map[string]string{"in_angl_sampling_frequency": tc.hz})
		got := samplingPeriod(dir, 0)
		want, _ := time.ParseDuration(tc.want)
		if got != want {
			t.Errorf("hz=%q period = %v, want %v", tc.hz, got, want)
		}
		if got < minPollPeriod || got > maxPollPeriod {
			t.Errorf("hz=%q period %v escaped the clamp", tc.hz, got)
		}
	}
}

func TestUnitsDescribesEachCase(t *testing.T) {
	for _, h := range []HingeInfo{
		{}, {Scale: 1, HasScale: true}, {Scale: 0.017453293, HasScale: true},
	} {
		if h.Units() == "" {
			t.Errorf("HingeInfo%+v has no unit description", h)
		}
	}
}

func TestIsHingeName(t *testing.T) {
	for _, n := range []string{"hinge", "cros-ec-lid-angle"} {
		if !isHingeName(n) {
			t.Errorf("%q should be recognised", n)
		}
	}
	for _, n := range []string{"accel_3d", "gyro_3d", "", "hinge2"} {
		if isHingeName(n) {
			t.Errorf("%q should not be recognised", n)
		}
	}
}

func TestIndexedBuildsBothForms(t *testing.T) {
	if got := indexed("/d", "in_angl%s_scale", 2); got != "/d/in_angl2_scale" {
		t.Errorf("indexed = %q", got)
	}
	if got := indexed("/d", "in_angl%s_scale", -1); got != "/d/in_angl_scale" {
		t.Errorf("unindexed = %q", got)
	}
}
