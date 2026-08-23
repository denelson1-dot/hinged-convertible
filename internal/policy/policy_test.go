package policy

import (
	"testing"
	"time"
)

func ptr[T any](v T) *T { return &v }

// feedAt runs angles through Step at a given cadence, returning the final
// state and every committed transition.
//
// The cadence matters: the slew gate compares angular change against elapsed
// time, so the same angle sequence is plausible when sampled slowly and
// implausible when sampled quickly.
func feedAt(t *testing.T, s State, c Config, lid *bool, every time.Duration, angles ...float64) (State, []Transition) {
	t.Helper()
	var out []Transition
	// Continue the timeline from wherever the state left off, so chaining
	// calls produces monotonically increasing timestamps. Restarting at zero
	// would make elapsed time zero or negative and silently disable the
	// slew gate.
	at := s.lastAt
	if at.IsZero() {
		at = time.Unix(0, 0)
	}
	for _, a := range angles {
		at = at.Add(every)
		var tr *Transition
		s, tr = Step(s, Reading{Angle: ptr(a), LidClosed: lid, Trusted: true, At: at}, c)
		if tr != nil {
			out = append(out, *tr)
		}
	}
	return s, out
}

// feed uses a deliberately slow cadence so that large angle steps stay
// plausible and the slew gate does not interfere. Tests that exercise the
// gate itself use feedAt with a realistic polling interval.
func feed(t *testing.T, s State, c Config, lid *bool, angles ...float64) (State, []Transition) {
	t.Helper()
	return feedAt(t, s, c, lid, time.Second, angles...)
}

func TestDefaultConfigIsValid(t *testing.T) {
	if !DefaultConfig().Valid() {
		t.Fatal("DefaultConfig() must satisfy Valid()")
	}
}

func TestConfigValidationRejectsInconsistentThresholds(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"laptop above tent", func(c *Config) { c.LaptopMax = 250 }},
		{"tent above tablet", func(c *Config) { c.TentMin = 350 }},
		{"wrap guard above laptop", func(c *Config) { c.WrapGuard = 200 }},
		{"zero enter samples", func(c *Config) { c.EnterSamples = 0 }},
		{"zero leave samples", func(c *Config) { c.LeaveSamples = 0 }},
		{"negative wrap guard", func(c *Config) { c.WrapGuard = -1 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := DefaultConfig()
			tc.mutate(&c)
			if c.Valid() {
				t.Errorf("expected %s to be rejected by Valid()", tc.name)
			}
		})
	}
}

// classify is the pure geometry. Every band boundary is checked on both sides.
func TestClassifyBands(t *testing.T) {
	c := DefaultConfig()
	open, closed := ptr(false), ptr(true)

	tests := []struct {
		name  string
		angle float64
		lid   *bool
		want  Posture
	}{
		{"wrapped past 360, lid open", 5, open, PostureTablet},
		{"wrapped, lid closed means shut", 5, closed, PostureClosed},
		{"wrapped, no lid signal assumes folded", 5, nil, PostureTablet},
		{"just below wrap guard", 29.9, open, PostureTablet},
		{"exactly at wrap guard is laptop", 30, open, PostureLaptop},
		{"normal laptop use", 100, open, PostureLaptop},
		{"exactly at laptop max", 180, open, PostureLaptop},
		{"dead band lower edge", 180.1, open, PostureUnknown},
		{"dead band middle", 195, open, PostureUnknown},
		{"dead band upper edge", 209.9, open, PostureUnknown},
		{"exactly at tent min", 210, open, PostureTent},
		{"tent range", 260, open, PostureTent},
		{"just below tablet min", 299.9, open, PostureTent},
		{"exactly at tablet min", 300, open, PostureTablet},
		{"fully folded", 355, open, PostureTablet},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(tc.angle, tc.lid, c); got != tc.want {
				t.Errorf("classify(%v) = %v, want %v", tc.angle, got, tc.want)
			}
		})
	}
}

// The switch must assert for tent as well as tablet: in both the keyboard
// faces away from the user. Closed must never assert.
func TestTabletSwitchMapping(t *testing.T) {
	tests := []struct {
		p    Posture
		want bool
	}{
		{PostureUnknown, false},
		{PostureClosed, false},
		{PostureLaptop, false},
		{PostureTent, true},
		{PostureTablet, true},
	}
	for _, tc := range tests {
		if got := tc.p.TabletSwitch(); got != tc.want {
			t.Errorf("%v.TabletSwitch() = %v, want %v", tc.p, got, tc.want)
		}
	}
}

func TestFirstTrustedReadingCommitsImmediately(t *testing.T) {
	c := DefaultConfig()
	s, trs := feed(t, State{}, c, ptr(false), 100)
	if s.Posture != PostureLaptop {
		t.Fatalf("posture = %v, want laptop", s.Posture)
	}
	if len(trs) != 1 {
		t.Fatalf("expected exactly one transition, got %d", len(trs))
	}
	if trs[0].From != PostureUnknown {
		t.Errorf("From = %v, want unknown", trs[0].From)
	}
}

// Entering tablet mode should feel instant: one sample past the threshold.
func TestEnterAssertsAfterOneSample(t *testing.T) {
	c := DefaultConfig()
	s, _ := feed(t, State{}, c, ptr(false), 100)
	s, trs := feed(t, s, c, ptr(false), 250)

	if len(trs) != 1 {
		t.Fatalf("expected 1 transition after a single tent reading, got %d", len(trs))
	}
	if !trs[0].SwitchChanged {
		t.Error("crossing the tent threshold must change the switch")
	}
	if s.Posture != PostureTent {
		t.Errorf("posture = %v, want tent", s.Posture)
	}
}

// Leaving requires three agreeing samples, so a single stray low reading in
// the middle of a fold must not release the switch.
func TestLeaveRequiresThreeSamples(t *testing.T) {
	c := DefaultConfig()
	s, _ := feed(t, State{}, c, ptr(false), 100, 250)
	if s.Posture != PostureTent {
		t.Fatalf("setup failed: posture = %v", s.Posture)
	}

	s, trs := feed(t, s, c, ptr(false), 100, 100)
	if len(trs) != 0 {
		t.Fatalf("released after 2 samples, want 3: %+v", trs)
	}
	if s.Posture != PostureTent {
		t.Errorf("posture = %v, want tent to persist", s.Posture)
	}

	s, trs = feed(t, s, c, ptr(false), 100)
	if len(trs) != 1 {
		t.Fatalf("expected release on the 3rd sample, got %d transitions", len(trs))
	}
	if s.Posture != PostureLaptop {
		t.Errorf("posture = %v, want laptop", s.Posture)
	}
}

// A stray reading partway through the leave debounce must reset the counter,
// not merely pause it. This is the flapping bug the hysteresis exists for.
func TestStrayReadingResetsLeaveDebounce(t *testing.T) {
	c := DefaultConfig()
	s, _ := feed(t, State{}, c, ptr(false), 100, 250)

	// Two samples toward laptop, then back to tent, then two more.
	// Without a reset this would total four and wrongly release.
	s, trs := feed(t, s, c, ptr(false), 100, 100, 250, 100, 100)
	if len(trs) != 0 {
		t.Fatalf("debounce counter did not reset: %+v", trs)
	}
	if s.Posture != PostureTent {
		t.Errorf("posture = %v, want tent", s.Posture)
	}
}

// The dead band is the whole point of the hysteresis: resting between the two
// thresholds must never produce a transition, no matter how long it sits.
func TestDeadBandNeverFlaps(t *testing.T) {
	c := DefaultConfig()
	s, _ := feed(t, State{}, c, ptr(false), 100)

	wobble := []float64{181, 190, 200, 209, 195, 185, 205, 199, 182, 208}
	s, trs := feed(t, s, c, ptr(false), wobble...)
	if len(trs) != 0 {
		t.Errorf("dead band produced %d transitions, want 0: %+v", len(trs), trs)
	}
	if s.Posture != PostureLaptop {
		t.Errorf("posture = %v, want laptop held throughout", s.Posture)
	}
}

// Sensor wrap: 360 -> 0 at the fully folded extreme. A small angle with the
// lid open means folded, and must NOT be read as "opened flat".
func TestWrapAroundHoldsTablet(t *testing.T) {
	c := DefaultConfig()
	s, _ := feed(t, State{}, c, ptr(false), 100, 250, 330)
	if s.Posture != PostureTablet {
		t.Fatalf("setup failed: posture = %v", s.Posture)
	}

	// Angle wraps past 360 to near zero while still folded.
	s, trs := feed(t, s, c, ptr(false), 355, 359, 2, 5, 1)
	for _, tr := range trs {
		if tr.To == PostureLaptop {
			t.Fatalf("wrap-around wrongly read as laptop: %+v", tr)
		}
	}
	if s.Posture != PostureTablet {
		t.Errorf("posture = %v, want tablet held across the wrap", s.Posture)
	}
}

// The same near-zero angle means "shut" when the lid switch agrees.
func TestLidClosedDisambiguatesZeroAngle(t *testing.T) {
	c := DefaultConfig()
	s, _ := feed(t, State{}, c, ptr(false), 100)
	s, _ = feed(t, s, c, ptr(true), 5, 5, 5)

	if s.Posture != PostureClosed {
		t.Errorf("posture = %v, want closed", s.Posture)
	}
	if s.Posture.TabletSwitch() {
		t.Error("a shut lid must never assert SW_TABLET_MODE")
	}
}

func slewConfig() Config { return DefaultConfig() }

// The gate defends against driver glitches, not just accelerometer motion:
// this hardware's sysfs attribute intermittently reads 0 via Go's os package,
// and an unfiltered zero looks exactly like a fully folded hinge.
func TestSlewGateOnByDefault(t *testing.T) {
	if DefaultConfig().MaxSlewRate <= 0 {
		t.Error("the slew gate must be enabled by default to filter driver glitches")
	}
}

// The 359 -> 0 wrap at full fold is a 1 degree move and must never be rejected
// as an implausible jump.
func TestCircularDeltaHandlesWrap(t *testing.T) {
	tests := []struct{ a, b, want float64 }{
		{359, 0, 1}, {0, 359, 1}, {355, 5, 10}, {110, 0, 110},
		{180, 0, 180}, {100, 100, 0}, {270, 90, 180},
	}
	for _, tc := range tests {
		if got := circularDelta(tc.a, tc.b); abs(got-tc.want) > 1e-9 {
			t.Errorf("circularDelta(%v,%v) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// A single spurious zero between two good readings must not move the machine.
// This is the exact real-world glitch observed on the HP ENVY x360.
func TestSpuriousZeroDoesNotFlipToTablet(t *testing.T) {
	c := DefaultConfig()
	const poll = 50 * time.Millisecond
	s, _ := feedAt(t, State{}, c, ptr(false), poll, 110)
	s, trs := feedAt(t, s, c, ptr(false), poll, 110, 0, 110, 110, 110, 0, 110, 110)
	if len(trs) != 0 {
		t.Errorf("spurious zeros produced %d transitions, want 0: %+v", len(trs), trs)
	}
	if s.Posture != PostureLaptop {
		t.Errorf("posture = %v, want laptop held throughout", s.Posture)
	}
}

// With the gate enabled, movement faster than MaxSlewRate is a glitch or a
// carry, not a fold.
func TestSlewRateRejectsImplausibleJumps(t *testing.T) {
	c := slewConfig()
	const poll = 50 * time.Millisecond

	s, _ := feedAt(t, State{}, c, ptr(false), poll, 100)
	// 100 -> 300 is 160 degrees the short way round, in 50ms: 3200 deg/s,
	// far above the 720 deg/s limit.
	s, trs := feedAt(t, s, c, ptr(false), poll, 300)

	if len(trs) != 0 {
		t.Fatalf("committed on an implausible slew: %+v", trs)
	}
	if s.Posture != PostureLaptop {
		t.Errorf("posture = %v, want laptop", s.Posture)
	}
}

// A genuine fast fold must still settle rather than being rejected forever:
// the rejected reading updates the baseline, so the next one is plausible.
func TestSlewRejectionRecoversOnNextReading(t *testing.T) {
	c := slewConfig()
	const poll = 50 * time.Millisecond

	s, _ := feedAt(t, State{}, c, ptr(false), poll, 100)
	s, trs := feedAt(t, s, c, ptr(false), poll, 300)
	if len(trs) != 0 {
		t.Fatal("first jump should have been rejected")
	}

	// Sampled again after a longer interval, the same angle is plausible.
	s, trs = feedAt(t, s, c, ptr(false), time.Second, 300)
	if len(trs) != 1 {
		t.Fatalf("expected the settled reading to commit, got %d", len(trs))
	}
	if s.Posture != PostureTablet {
		t.Errorf("posture = %v, want tablet", s.Posture)
	}
}

// Untrusted readings carry no information and must not advance anything.
func TestUntrustedReadingIsInert(t *testing.T) {
	c := DefaultConfig()
	s, _ := feed(t, State{}, c, ptr(false), 100)
	before := s

	at := time.Unix(0, 0).Add(time.Second)
	s, tr := Step(s, Reading{Angle: ptr(320.0), LidClosed: ptr(false), Trusted: false, At: at}, c)
	if tr != nil {
		t.Fatalf("untrusted reading committed a transition: %+v", tr)
	}
	if s.Posture != before.Posture {
		t.Errorf("posture changed on an untrusted reading")
	}
}

func TestNilAngleIsInert(t *testing.T) {
	c := DefaultConfig()
	s, _ := feed(t, State{}, c, ptr(false), 100)

	s, tr := Step(s, Reading{Angle: nil, Trusted: true, At: time.Unix(1, 0)}, c)
	if tr != nil || s.Posture != PostureLaptop {
		t.Error("a reading with no angle must change nothing")
	}
}

// tent -> tablet is a real posture change but must not toggle the switch,
// so the uinput sink stays quiet.
func TestPostureChangeWithoutSwitchChange(t *testing.T) {
	c := DefaultConfig()
	s, _ := feed(t, State{}, c, ptr(false), 100, 250)

	_, trs := feed(t, s, c, ptr(false), 330)
	if len(trs) != 1 {
		t.Fatalf("expected 1 transition, got %d", len(trs))
	}
	if trs[0].To != PostureTablet {
		t.Errorf("To = %v, want tablet", trs[0].To)
	}
	if trs[0].SwitchChanged {
		t.Error("tent -> tablet must not change SW_TABLET_MODE")
	}
}

// Every committed transition must carry a non-empty reason: it is what
// `hinged doctor` and the logs use to answer "why did my keyboard switch off".
func TestTransitionsAlwaysExplainThemselves(t *testing.T) {
	c := DefaultConfig()
	_, trs := feed(t, State{}, c, ptr(false), 100, 250, 330, 100, 100, 100)
	if len(trs) == 0 {
		t.Fatal("expected transitions")
	}
	for _, tr := range trs {
		if tr.Reason == "" {
			t.Errorf("transition %v -> %v has no reason", tr.From, tr.To)
		}
	}
}

// A full physical fold and unfold should produce exactly one switch assertion
// and one release, with no intermediate chatter.
func TestFullFoldCycleProducesCleanSwitchEvents(t *testing.T) {
	c := DefaultConfig()
	fold := []float64{95, 110, 140, 170, 185, 200, 215, 240, 270, 300, 330, 355}
	unfold := []float64{330, 300, 270, 240, 215, 200, 185, 170, 140, 110, 95, 95}

	s, trs := feed(t, State{}, c, ptr(false), fold...)
	s, more := feed(t, s, c, ptr(false), unfold...)
	trs = append(trs, more...)

	var asserts, releases int
	for _, tr := range trs {
		if !tr.SwitchChanged {
			continue
		}
		if tr.To.TabletSwitch() {
			asserts++
		} else {
			releases++
		}
	}
	if asserts != 1 {
		t.Errorf("switch asserted %d times, want exactly 1", asserts)
	}
	if releases != 1 {
		t.Errorf("switch released %d times, want exactly 1", releases)
	}
	if s.Posture != PostureLaptop {
		t.Errorf("final posture = %v, want laptop", s.Posture)
	}
}

// The zero State must be a usable starting point; callers should not need to
// construct it via a helper.
func TestZeroStateIsUsable(t *testing.T) {
	c := DefaultConfig()
	s, tr := Step(State{}, Reading{Angle: ptr(100.0), Trusted: true, At: time.Unix(1, 0)}, c)
	if tr == nil {
		t.Fatal("zero state should accept a first reading")
	}
	if s.Posture != PostureLaptop {
		t.Errorf("posture = %v, want laptop", s.Posture)
	}
}
