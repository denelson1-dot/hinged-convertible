package policy

import (
	"math"
	"testing"
	"time"
)

// pollPeriod is the real polling interval on the reference hardware, derived
// from the sensor's advertised 10 Hz. Tests use it by default so that the
// plausibility gates are exercised at the cadence they will actually see.
const pollPeriod = 50 * time.Millisecond

// feedAt runs angles through Step at a given cadence, returning the final
// state and every committed transition.
//
// Cadence matters: the slew gate compares angular change against elapsed time,
// so the same sequence can be plausible when sampled slowly and implausible
// when sampled quickly.
func feedAt(t *testing.T, s State, c Config, lid OptBool, every time.Duration, angles ...float64) (State, []Transition) {
	t.Helper()
	var out []Transition
	// Continue the timeline from wherever the state left off, so chained calls
	// produce monotonically increasing timestamps. Restarting at zero would
	// make elapsed time non-positive and cause every reading to be discarded.
	at := s.lastAt
	if at.IsZero() {
		at = time.Unix(1000, 0)
	}
	for _, a := range angles {
		at = at.Add(every)
		var tr Transition
		var ok bool
		s, tr, ok = Step(s, Reading{Angle: a, HasAngle: true, LidClosed: lid, Trusted: true, At: at}, c)
		if ok {
			out = append(out, tr)
		}
	}
	return s, out
}

// feed uses the real polling cadence.
func feed(t *testing.T, s State, c Config, lid OptBool, angles ...float64) (State, []Transition) {
	t.Helper()
	return feedAt(t, s, c, lid, pollPeriod, angles...)
}

// ramp interpolates between two angles at a plausible rate, the way a hand
// actually folds a laptop. Feeding a large step directly is not a realistic
// input and would simply be rejected by the plausibility gates.
func ramp(from, to float64, step float64) []float64 {
	var out []float64
	if from < to {
		for a := from; a < to; a += step {
			out = append(out, a)
		}
	} else {
		for a := from; a > to; a -= step {
			out = append(out, a)
		}
	}
	return append(out, to)
}

// settle drives the machine to a posture the way the hardware would, then
// confirms it got there.
func settle(t *testing.T, s State, c Config, lid OptBool, from, to float64) State {
	t.Helper()
	s, _ = feed(t, s, c, lid, ramp(from, to, 20)...)
	// Hold steady so any pending debounce completes.
	s, _ = feed(t, s, c, lid, to, to, to, to)
	return s
}

// stateAt drives the machine into a posture through real readings, since
// State's posture is no longer settable from outside the package.
func stateAt(p Posture) State {
	c := DefaultConfig()
	var angle float64
	switch p {
	case PostureLaptop:
		angle = 110
	case PostureTent:
		angle = 250
	case PostureTablet:
		angle = 330
	}
	s := State{}
	at := time.Unix(1000, 0)
	for i := 0; i < 8; i++ {
		at = at.Add(pollPeriod)
		s, _, _ = Step(s, Reading{Angle: angle, HasAngle: true, LidClosed: Bool(false), Trusted: true, At: at}, c)
	}
	return s
}

func TestDefaultConfigIsValid(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatalf("DefaultConfig() must validate: %v", err)
	}
}

func TestConfigValidateRejectsBadConfigs(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		want   error
	}{
		{"laptop above tent", func(c *Config) { c.LaptopMax = 250 }, ErrThresholdOrder},
		{"tent above tablet", func(c *Config) { c.TentMin = 350 }, ErrThresholdOrder},
		{"wrap guard above laptop", func(c *Config) { c.WrapGuard = 200 }, ErrThresholdOrder},
		// Equality is not harmless: it silently deletes a posture.
		{"tent equals tablet makes tent unreachable", func(c *Config) { c.TentMin = c.TabletMin }, ErrThresholdOrder},
		{"laptop equals tent removes the dead band", func(c *Config) { c.LaptopMax = c.TentMin }, ErrThresholdOrder},
		{"zero wrap guard disables wrap handling", func(c *Config) { c.WrapGuard = 0 }, ErrThresholdOrder},
		{"negative wrap guard", func(c *Config) { c.WrapGuard = -1 }, ErrThresholdOrder},
		{"NaN threshold", func(c *Config) { c.TentMin = math.NaN() }, ErrThresholdFinite},
		{"Inf threshold", func(c *Config) { c.TabletMin = math.Inf(1) }, ErrThresholdFinite},
		{"zero enter samples", func(c *Config) { c.EnterSamples = 0 }, ErrSampleCounts},
		{"ambiguous below enter", func(c *Config) { c.AmbiguousSamples = 0 }, ErrSampleCounts},
		{"zero leave samples", func(c *Config) { c.LeaveSamples = 0 }, ErrSampleCounts},
		{"zero decay would allow permanent assertion", func(c *Config) { c.LeaveDecay = 0 }, ErrDecayRange},
		{"decay above one", func(c *Config) { c.LeaveDecay = 1.5 }, ErrDecayRange},
		{"NaN slew rate silently disables the gate", func(c *Config) { c.MaxSlewRate = math.NaN() }, ErrThresholdFinite},
		{"negative slew rate", func(c *Config) { c.MaxSlewRate = -1 }, ErrNegativeRateLimit},
		{"zero reject tolerance", func(c *Config) { c.RejectTolerance = 0 }, ErrRejectTolerance},
		{"reject decay of one locks the baseline", func(c *Config) { c.RejectDecay = 1 }, ErrDecayRange},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := DefaultConfig()
			tc.mutate(&c)
			if err := c.Validate(); err == nil {
				t.Fatal("expected rejection, got nil")
			} else if tc.want != nil && err != tc.want {
				t.Errorf("got %v, want %v", err, tc.want)
			}
		})
	}
}

// An invalid config must not be acted on. Holding is always safe; guessing
// with undefined posture bands is not.
func TestStepRefusesInvalidConfig(t *testing.T) {
	bad := DefaultConfig()
	bad.TentMin = bad.TabletMin
	s, _, ok := Step(State{}, Reading{Angle: 250.0, HasAngle: true, Trusted: true, At: time.Unix(1, 0)}, bad)
	if ok || s.Posture() != PostureUnknown {
		t.Error("Step acted on an invalid config")
	}
}

func TestClassifyBands(t *testing.T) {
	c := DefaultConfig()
	tests := []struct {
		name    string
		angle   float64
		lid     OptBool
		current Posture
		want    Posture
	}{
		// A near-zero angle is not decidable from the angle alone.
		{"wrapped while already folded", 5, Bool(false), PostureTablet, PostureTablet},
		{"wrapped while in tent", 5, Bool(false), PostureTent, PostureTablet},
		{"near-zero from laptop holds", 5, Bool(false), PostureLaptop, PostureUnknown},
		{"near-zero at startup holds without lid state", 5, OptBool{}, PostureUnknown, PostureUnknown},
		{"near-zero at startup with lid open is a fold", 5, Bool(false), PostureUnknown, PostureTablet},
		{"near-zero with lid shut is closed", 5, Bool(true), PostureLaptop, PostureClosed},

		// A shut lid settles it at any angle, not just near zero.
		{"lid shut at tent angle", 250, Bool(true), PostureTent, PostureClosed},
		{"lid shut at tablet angle", 330, Bool(true), PostureTablet, PostureClosed},
		{"lid shut at laptop angle", 100, Bool(true), PostureLaptop, PostureClosed},

		{"exactly at wrap guard is laptop", 30, Bool(false), PostureLaptop, PostureLaptop},
		{"normal laptop use", 100, Bool(false), PostureLaptop, PostureLaptop},
		{"exactly at laptop max", 180, Bool(false), PostureLaptop, PostureLaptop},
		{"dead band lower edge", 180.1, Bool(false), PostureLaptop, PostureUnknown},
		{"dead band upper edge", 209.9, Bool(false), PostureLaptop, PostureUnknown},
		{"exactly at tent min", 210, Bool(false), PostureLaptop, PostureTent},
		{"just below tablet min", 299.9, Bool(false), PostureTent, PostureTent},
		{"exactly at tablet min", 300, Bool(false), PostureTent, PostureTablet},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := classify(tc.angle, tc.lid, tc.current, c)
			if got != tc.want {
				t.Errorf("classify(%v, lid, %v) = %v, want %v", tc.angle, tc.current, got, tc.want)
			}
			if reason == ReasonNone {
				t.Error("classify must always explain itself")
			}
		})
	}
}

func TestTabletSwitchMapping(t *testing.T) {
	for _, tc := range []struct {
		p    Posture
		want bool
	}{
		{PostureUnknown, false}, {PostureClosed, false}, {PostureLaptop, false},
		{PostureTent, true}, {PostureTablet, true},
	} {
		if got := tc.p.TabletSwitch(); got != tc.want {
			t.Errorf("%v.TabletSwitch() = %v, want %v", tc.p, got, tc.want)
		}
	}
}

func TestPostureStrings(t *testing.T) {
	for p, want := range map[Posture]string{
		PostureUnknown: "unknown", PostureClosed: "closed", PostureLaptop: "laptop",
		PostureTent: "tent", PostureTablet: "tablet",
	} {
		if got := p.String(); got != want {
			t.Errorf("String() = %q, want %q", got, want)
		}
	}
}

// --- The safety-critical cases: never assert on weak evidence ---------------

// A single spurious zero must not assert. This is the documented driver glitch.
func TestSingleSpuriousZeroDoesNotAssert(t *testing.T) {
	c := DefaultConfig()
	s := settle(t, State{}, c, Bool(false), 110, 110)
	s, trs := feed(t, s, c, Bool(false), 110, 0, 110, 110, 0, 110)
	if len(trs) != 0 {
		t.Errorf("spurious zeros produced %d transitions: %+v", len(trs), trs)
	}
	if s.Posture() != PostureLaptop {
		t.Errorf("posture = %v, want laptop", s.Posture())
	}
}

// Two consecutive glitches must not assert either. The previous implementation
// adopted a rejected reading as its baseline, so any value repeated twice was
// accepted -- turning the glitch filter into a glitch amplifier.
func TestTwoConsecutiveGlitchesDoNotAssert(t *testing.T) {
	c := DefaultConfig()
	s := settle(t, State{}, c, Bool(false), 110, 110)
	s, trs := feed(t, s, c, Bool(false), 0, 0)
	if len(trs) != 0 || s.Posture() != PostureLaptop {
		t.Errorf("two glitches changed posture to %v with %d transitions", s.Posture(), len(trs))
	}
}

// Even a sustained run of zeros must not assert from laptop posture: a hinge
// cannot reach a wrapped near-zero reading without travelling through tent.
func TestSustainedZerosFromLaptopNeverAssert(t *testing.T) {
	c := DefaultConfig()
	s := settle(t, State{}, c, Bool(false), 110, 110)
	zeros := make([]float64, 50)
	s, trs := feed(t, s, c, Bool(false), zeros...)
	for _, tr := range trs {
		if tr.To.TabletSwitch() {
			t.Fatalf("a run of zeros asserted the switch: %+v", tr)
		}
	}
	if s.Posture() != PostureLaptop {
		t.Errorf("posture = %v, want laptop held", s.Posture())
	}
}

// A machine booted or restarted while already folded must still reach tablet
// posture. The lid switch is what makes that decidable at startup, and
// corroboration is what keeps a one-sample glitch from taking the same path.
func TestBootedFoldedReachesTablet(t *testing.T) {
	c := DefaultConfig()
	s, _ := feed(t, State{}, c, Bool(false), 5, 5, 5, 5, 5, 5)
	if s.Posture() != PostureTablet {
		t.Errorf("posture = %v, want tablet when started already folded", s.Posture())
	}
}

// The same startup path must not commit on a single glitch.
func TestBootGlitchDoesNotAssertAlone(t *testing.T) {
	c := DefaultConfig()
	_, trs := feed(t, State{}, c, Bool(false), 0)
	for _, tr := range trs {
		if tr.To.TabletSwitch() {
			t.Fatalf("one startup zero asserted: %+v", tr)
		}
	}
}

// A one-degree movement across the wrap guard must not disable the keyboard.
func TestSmallMovementAcrossWrapGuardDoesNotAssert(t *testing.T) {
	c := DefaultConfig()
	s := settle(t, State{}, c, Bool(false), 100, 31)
	_, trs := feed(t, s, c, Bool(false), 30.5, 29.5, 29, 28.5, 29)
	for _, tr := range trs {
		if tr.To.TabletSwitch() {
			t.Fatalf("a 1 degree move asserted the switch: %+v", tr)
		}
	}
}

// The very first reading must not assert on its own: roughly one daemon start
// in four hundred would otherwise boot straight into tablet mode.
func TestFirstReadingDoesNotAssertAlone(t *testing.T) {
	c := DefaultConfig()
	s, trs := feed(t, State{}, c, Bool(false), 330)
	for _, tr := range trs {
		if tr.To.TabletSwitch() {
			t.Fatalf("first reading asserted: %+v", tr)
		}
	}
	if s.Posture().TabletSwitch() {
		t.Errorf("posture = %v after one reading", s.Posture())
	}
	// It should assert once corroborated.
	s, _ = feed(t, s, c, Bool(false), 330, 330, 330)
	if !s.Posture().TabletSwitch() {
		t.Errorf("posture = %v, want an asserting posture after corroboration", s.Posture())
	}
}

// A closed lid must never assert, at any angle.
func TestClosedLidNeverAsserts(t *testing.T) {
	c := DefaultConfig()
	for _, angle := range []float64{0, 5, 100, 200, 250, 330, 359} {
		s := stateAt(PostureLaptop)
		s, _ = feed(t, s, c, Bool(true), angle, angle, angle, angle, angle)
		if s.Posture().TabletSwitch() {
			t.Errorf("angle %v with lid shut gave %v, which asserts", angle, s.Posture())
		}
	}
}

// A backwards clock must not bypass the plausibility gates.
func TestBackwardsClockIsRejected(t *testing.T) {
	c := DefaultConfig()
	base := time.Unix(1000, 0)
	s := settle(t, State{}, c, Bool(false), 110, 110)
	for i := 0; i < 10; i++ {
		s, _, _ = Step(s, Reading{Angle: 0.0, HasAngle: true, Trusted: true, At: base}, c)
	}
	if s.Posture().TabletSwitch() {
		t.Errorf("posture = %v after backwards-clock readings", s.Posture())
	}
}

// A zero timestamp means the caller forgot to set At; accepting it would
// permanently disable every time-based check with no diagnostic.
func TestZeroTimestampIsRejected(t *testing.T) {
	c := DefaultConfig()
	s, _, ok := Step(State{}, Reading{Angle: 330.0, HasAngle: true, Trusted: true}, c)
	if ok || s.Posture() != PostureUnknown {
		t.Error("a reading with no timestamp must change nothing")
	}
}

func TestNonFiniteAnglesAreRejected(t *testing.T) {
	c := DefaultConfig()
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		s := stateAt(PostureLaptop)
		before := s.Posture()
		at := s.lastAt.Add(pollPeriod)
		s, _, ok := Step(s, Reading{Angle: bad, HasAngle: true, Trusted: true, At: at}, c)
		if ok || s.Posture() != before {
			t.Errorf("angle %v changed state", bad)
		}
	}
}

// A NaN must not destroy accumulated debounce progress either: a recurring NaN
// would otherwise make the switch impossible to release.
func TestNaNPreservesLeaveProgress(t *testing.T) {
	c := DefaultConfig()
	s := settle(t, State{}, c, Bool(false), 110, 330)
	if !s.Posture().TabletSwitch() {
		t.Fatalf("setup: %v", s.Posture())
	}
	at := s.lastAt
	for i := 0; i < 12; i++ {
		at = at.Add(pollPeriod)
		angle := 110.0
		if i%2 == 1 {
			angle = math.NaN()
		}
		s, _, _ = Step(s, Reading{Angle: angle, HasAngle: true, LidClosed: Bool(false), Trusted: true, At: at}, c)
	}
	if s.Posture().TabletSwitch() {
		t.Error("interleaved NaNs prevented the switch from releasing")
	}
}

// --- Plausibility gates -----------------------------------------------------

func TestCircularDeltaHandlesWrapAndExtremes(t *testing.T) {
	tests := []struct{ a, b, want float64 }{
		{359, 0, 1}, {0, 359, 1}, {355, 5, 10}, {110, 0, 110},
		{180, 0, 180}, {100, 100, 0}, {270, 90, 180}, {720, 0, 0}, {-10, 350, 0},
	}
	for _, tc := range tests {
		if got := circularDelta(tc.a, tc.b); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("circularDelta(%v,%v) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// circularDelta must terminate on non-finite input. The previous subtraction
// loop spun forever on +Inf, wedging the daemon with the switch frozen.
func TestCircularDeltaTerminatesOnNonFinite(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		circularDelta(math.Inf(1), 100)
		circularDelta(math.NaN(), 100)
		circularDelta(1e300, 100)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("circularDelta did not terminate on non-finite input")
	}
}

// The slew gate is a rate test, so it is inert beyond a known interval. That
// limit must be exposed rather than left implicit, because a slow sensor
// silently loses this defence entirely.
func TestSlewGateCeilingIsHonest(t *testing.T) {
	c := DefaultConfig()
	ceil := SlewGateCeiling(c)
	if ceil != 250*time.Millisecond {
		t.Errorf("SlewGateCeiling = %v, want 250ms at the default 720 deg/s", ceil)
	}
	if !implausible(0, 110, 100*time.Millisecond, c) {
		t.Error("a 110 degree jump in 100ms must be rejected")
	}
	// Beyond the ceiling nothing can be rejected, by arithmetic.
	if implausible(0, 180, ceil+time.Millisecond, c) {
		t.Error("past the ceiling the gate cannot reject; the docs must not claim otherwise")
	}
}

// Slow sensors lose the slew gate, so the posture-gated wrap guard has to
// carry the load alone. Verify it does, at a cadence where the gate is
// provably inert.
func TestWrapGuardProtectsSlowSensorsAlone(t *testing.T) {
	c := DefaultConfig()
	slow := 2 * SlewGateCeiling(c)
	s, _ := feedAt(t, State{}, c, Bool(false), slow, 110, 110, 110, 110)
	if s.Posture() != PostureLaptop {
		t.Fatalf("setup: %v", s.Posture())
	}
	_, trs := feedAt(t, s, c, Bool(false), slow, 0, 0, 0, 0, 0, 0)
	for _, tr := range trs {
		if tr.To.TabletSwitch() {
			t.Fatalf("zeros asserted on a slow sensor with no slew gate: %+v", tr)
		}
	}
}

// A genuine sustained movement must eventually be accepted, or a fast fold
// would be rejected forever.
func TestPersistentMovementIsEventuallyAccepted(t *testing.T) {
	c := DefaultConfig()
	s := settle(t, State{}, c, Bool(false), 110, 110)
	// A jump larger than the step cap, held steady.
	held := make([]float64, 12)
	for i := range held {
		held[i] = 330
	}
	s, _ = feed(t, s, c, Bool(false), held...)
	if s.Posture() != PostureTablet {
		t.Errorf("posture = %v, want tablet once the movement proved persistent", s.Posture())
	}
}

func TestRejectedReadingsAreObservable(t *testing.T) {
	c := DefaultConfig()
	s := settle(t, State{}, c, Bool(false), 110, 110)
	s, _ = feed(t, s, c, Bool(false), 0)
	if s.Rejected() == 0 {
		t.Error("a rejected reading must be visible to the caller, so that " +
			"'every reading is being discarded' is distinguishable from 'the sensor is idle'")
	}
}

// --- Hysteresis and debounce ------------------------------------------------

func TestDeadBandNeverFlaps(t *testing.T) {
	c := DefaultConfig()
	s := settle(t, State{}, c, Bool(false), 100, 100)
	s, trs := feed(t, s, c, Bool(false), 181, 190, 200, 209, 195, 185, 205, 199, 182, 208)
	if len(trs) != 0 {
		t.Errorf("dead band produced %d transitions: %+v", len(trs), trs)
	}
	if s.Posture() != PostureLaptop {
		t.Errorf("posture = %v, want laptop held", s.Posture())
	}
}

// Releasing must not require a strictly consecutive run: a sensor alternating
// between postures would otherwise keep the keyboard disabled forever.
func TestAlternatingSensorStillReleases(t *testing.T) {
	c := DefaultConfig()
	s := settle(t, State{}, c, Bool(false), 110, 250)
	if !s.Posture().TabletSwitch() {
		t.Fatalf("setup: %v", s.Posture())
	}

	at := s.lastAt
	for i := 0; i < 60 && s.Posture().TabletSwitch(); i++ {
		at = at.Add(pollPeriod)
		angle := 250.0
		if i%2 == 0 {
			angle = 100
		}
		s, _, _ = Step(s, Reading{Angle: angle, HasAngle: true, LidClosed: Bool(false), Trusted: true, At: at}, c)
	}
	if s.Posture().TabletSwitch() {
		t.Error("a sensor disagreeing half the time kept the switch asserted indefinitely")
	}
}

// A full physical fold and unfold produces exactly one assert and one release.
func TestFullFoldCycleIsClean(t *testing.T) {
	c := DefaultConfig()
	var trs []Transition
	s, a := feed(t, State{}, c, Bool(false), ramp(95, 355, 15)...)
	trs = append(trs, a...)
	s, b := feed(t, s, c, Bool(false), 355, 355, 355)
	trs = append(trs, b...)
	s, d := feed(t, s, c, Bool(false), ramp(355, 95, 15)...)
	trs = append(trs, d...)
	s, e := feed(t, s, c, Bool(false), 95, 95, 95, 95)
	trs = append(trs, e...)

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
		t.Errorf("switch asserted %d times, want 1 (%+v)", asserts, trs)
	}
	if releases != 1 {
		t.Errorf("switch released %d times, want 1 (%+v)", releases, trs)
	}
	if s.Posture() != PostureLaptop {
		t.Errorf("final posture = %v, want laptop", s.Posture())
	}
}

// tent -> tablet is a real posture change that must not toggle the switch.
func TestPostureChangeWithoutSwitchChange(t *testing.T) {
	c := DefaultConfig()
	s := settle(t, State{}, c, Bool(false), 110, 250)
	if s.Posture() != PostureTent {
		t.Fatalf("setup: %v", s.Posture())
	}
	_, trs := feed(t, s, c, Bool(false), ramp(250, 330, 20)...)
	var found bool
	for _, tr := range trs {
		if tr.To == PostureTablet {
			found = true
			if tr.SwitchChanged {
				t.Error("tent -> tablet must not change SW_TABLET_MODE")
			}
		}
	}
	if !found {
		t.Errorf("expected a transition to tablet, got %+v", trs)
	}
}

// --- Contract ---------------------------------------------------------------

func TestUntrustedReadingIsInert(t *testing.T) {
	c := DefaultConfig()
	s := settle(t, State{}, c, Bool(false), 110, 110)
	before := s.Posture()
	at := s.lastAt.Add(pollPeriod)
	s, _, ok := Step(s, Reading{Angle: 330.0, HasAngle: true, Trusted: false, At: at}, c)
	if ok || s.Posture() != before {
		t.Error("an untrusted reading must change nothing")
	}
}

func TestNilAngleIsInert(t *testing.T) {
	c := DefaultConfig()
	s := settle(t, State{}, c, Bool(false), 110, 110)
	s, _, ok := Step(s, Reading{HasAngle: false, Trusted: true, At: s.lastAt.Add(pollPeriod)}, c)
	if ok || s.Posture() != PostureLaptop {
		t.Error("a reading with no angle must change nothing")
	}
}

func TestTransitionsCarryDiagnostics(t *testing.T) {
	c := DefaultConfig()
	_, trs := feed(t, State{}, c, Bool(false), ramp(95, 355, 15)...)
	if len(trs) == 0 {
		t.Fatal("expected transitions")
	}
	for _, tr := range trs {
		if tr.Reason == ReasonNone {
			t.Errorf("%v -> %v has no reason", tr.From, tr.To)
		}
		if tr.Angle < 0 || tr.Angle >= 360 {
			t.Errorf("%v -> %v carries an unnormalised angle %v", tr.From, tr.To, tr.Angle)
		}
	}
}

func TestZeroStateIsUsable(t *testing.T) {
	c := DefaultConfig()
	s, _ := feed(t, State{}, c, Bool(false), 100, 100)
	if s.Posture() != PostureLaptop {
		t.Errorf("posture = %v, want laptop", s.Posture())
	}
}

// Whatever a fuzzer throws at it, the machine must not panic, must not hang,
// and must never leave the switch asserted after sustained laptop readings.
func FuzzStep(f *testing.F) {
	f.Add(100.0, 250.0, 0.0, 330.0, true)
	f.Add(math.NaN(), 0.0, -50.0, 1e300, false)
	f.Fuzz(func(t *testing.T, a, b, cc, d float64, lidClosed bool) {
		cfg := DefaultConfig()
		s := State{}
		at := time.Unix(1000, 0)
		for _, angle := range []float64{a, b, cc, d} {
			at = at.Add(pollPeriod)
			s, _, _ = Step(s, Reading{Angle: angle, HasAngle: true, LidClosed: Bool(lidClosed), Trusted: true, At: at}, cfg)
		}
		// Sustained, plausible laptop readings must always win in the end.
		for i := 0; i < 40; i++ {
			at = at.Add(pollPeriod)
			s, _, _ = Step(s, Reading{Angle: 100.0, HasAngle: true, LidClosed: Bool(false), Trusted: true, At: at}, cfg)
		}
		if s.Posture().TabletSwitch() {
			t.Errorf("switch still asserted after 40 laptop readings: %v", s.Posture())
		}
	})
}

// --- Dead-man release -------------------------------------------------------

// A sensor that stops answering must not leave the keyboard suppressed. This
// is the one release path that does not depend on the sensor's cooperation.
func TestTickReleasesWhenSensorGoesSilent(t *testing.T) {
	c := DefaultConfig()
	s := settle(t, State{}, c, Bool(false), 110, 330)
	if !s.Posture().TabletSwitch() {
		t.Fatalf("setup: %v", s.Posture())
	}

	// Not yet: within the grace period nothing should happen.
	s, _, ok := Tick(s, s.lastAt.Add(c.SensorLostGrace/2), c)
	if ok {
		t.Error("released before the grace period elapsed")
	}

	s, tr, ok := Tick(s, s.lastAt.Add(c.SensorLostGrace+time.Second), c)
	if !ok {
		t.Fatal("sensor went silent past the grace period and the switch was not released")
	}
	if tr.Reason != ReasonSensorLost || !tr.SwitchChanged {
		t.Errorf("transition = %+v, want a sensor-lost release", tr)
	}
	if s.Posture().TabletSwitch() {
		t.Errorf("posture = %v, want the switch released", s.Posture())
	}
}

// Tick must do nothing when the switch is not asserted; there is nothing to
// make safe, and a spurious transition would churn the logs.
func TestTickIsInertWhenNotAsserted(t *testing.T) {
	c := DefaultConfig()
	s := settle(t, State{}, c, Bool(false), 110, 110)
	if _, _, ok := Tick(s, s.lastAt.Add(time.Hour), c); ok {
		t.Error("Tick fired while in laptop posture")
	}
}

func TestTickDisabledByZeroGrace(t *testing.T) {
	c := DefaultConfig()
	c.SensorLostGrace = 0
	s := settle(t, State{}, c, Bool(false), 110, 330)
	if _, _, ok := Tick(s, s.lastAt.Add(time.Hour), c); ok {
		t.Error("Tick fired with the dead-man path disabled")
	}
}

// The Engine wrapper must reject a bad config at construction rather than
// silently doing nothing on every sample.
func TestEngineRejectsInvalidConfigAtConstruction(t *testing.T) {
	bad := DefaultConfig()
	bad.TentMin = bad.TabletMin
	if _, err := New(bad); err == nil {
		t.Fatal("New accepted an invalid config")
	}
	e, err := New(DefaultConfig())
	if err != nil {
		t.Fatalf("New rejected a valid config: %v", err)
	}
	if e.Posture() != PostureUnknown {
		t.Errorf("fresh engine posture = %v", e.Posture())
	}
}

func TestReasonsAllStringify(t *testing.T) {
	for r := ReasonNone; r <= ReasonSensorLost; r++ {
		if r.String() == "" {
			t.Errorf("Reason(%d) has no string", int(r))
		}
	}
}

// OptBool must keep "absent" distinct from "open": conflating them would make
// a missing lid switch read as a permanently open lid.
func TestOptBoolDistinguishesAbsentFromOpen(t *testing.T) {
	var absent OptBool
	if absent.IsOpen() || absent.IsClosed() {
		t.Error("an absent lid reading is neither open nor closed")
	}
	if !Bool(false).IsOpen() || Bool(false).IsClosed() {
		t.Error("Bool(false) must be open")
	}
	if !Bool(true).IsClosed() || Bool(true).IsOpen() {
		t.Error("Bool(true) must be closed")
	}
}
