// Package policy converts noisy hinge-angle readings into a stable posture
// decision. It is deliberately pure: no I/O, no clocks, no logging. Every
// input arrives in a Reading and every effect leaves as a Transition, which
// makes the whole decision surface testable without hardware.
//
// This is the part of the project that does not exist elsewhere. libinput
// already reacts to SW_TABLET_MODE; what no generic tool provides is a
// defensible policy for deciding when to assert it.
package policy

import "time"

// Posture is the physical arrangement of the machine. It is richer than the
// binary SW_TABLET_MODE switch the kernel exposes, because a hinge angle can
// distinguish states that the switch cannot.
type Posture int

const (
	// PostureUnknown is the startup state, before any reading has been committed.
	PostureUnknown Posture = iota
	// PostureClosed means the lid is shut against the keyboard.
	PostureClosed
	// PostureLaptop means the keyboard is reachable and facing the user.
	PostureLaptop
	// PostureTent means the machine is folded past vertical so the keyboard
	// faces away or downward. The keyboard is unusable, so the switch asserts.
	PostureTent
	// PostureTablet means the screen is folded flat against the base.
	PostureTablet
)

func (p Posture) String() string {
	switch p {
	case PostureClosed:
		return "closed"
	case PostureLaptop:
		return "laptop"
	case PostureTent:
		return "tent"
	case PostureTablet:
		return "tablet"
	default:
		return "unknown"
	}
}

// TabletSwitch reports the value SW_TABLET_MODE should carry for this posture.
//
// Tent asserts the switch as well as tablet: in both the keyboard is folded
// away from the user and would otherwise register keypresses against whatever
// the machine is resting on. This is why the default TentMin (210 degrees),
// not TabletMin, is the threshold that matters for input suppression.
//
// Closed deliberately does NOT assert. A shut lid is not tablet mode, and
// asserting there would suppress the keyboard on wake.
func (p Posture) TabletSwitch() bool {
	return p == PostureTent || p == PostureTablet
}

// Config holds the tunable policy. Angles are in degrees. Zero values are not
// meaningful; use DefaultConfig and override.
type Config struct {
	// WrapGuard is the angle below which a reading is interpreted as having
	// wrapped past 360 rather than being genuinely flat. Hinge sensors on
	// convertibles typically report 360 -> 0 at the fully folded extreme, so a
	// small angle means "folded shut", never "opened flat".
	WrapGuard float64

	// LaptopMax is the angle at or below which the machine is in laptop
	// posture. Must be < TentMin; the gap between them is the hysteresis dead
	// band that stops the posture flapping while the hinge rests near the
	// boundary.
	LaptopMax float64

	// TentMin is the angle at or above which the keyboard is considered folded
	// away. This is the threshold that asserts SW_TABLET_MODE.
	TentMin float64

	// TabletMin is the angle at or above which the machine is fully folded.
	TabletMin float64

	// EnterSamples is how many consecutive agreeing readings are required to
	// assert the switch. Low by design: entering late means the keyboard is
	// already face-down registering keypresses.
	EnterSamples int

	// LeaveSamples is how many consecutive agreeing readings are required to
	// release the switch. Higher than EnterSamples because a spurious exit is
	// worse than a slightly delayed one, and the hinge passes through
	// intermediate angles on the way to being folded.
	LeaveSamples int

	// MaxSlewRate is the fastest angular change, in degrees per second, that is
	// treated as a real hinge movement. Faster changes are rejected as
	// implausible. Zero disables the check.
	//
	// This serves two distinct purposes. For accelerometer-derived angles it is
	// a jerk gate: the computed value is meaningless while the machine is being
	// carried. For real hinge sensors it filters driver glitches, which do
	// happen -- a spurious zero reading is indistinguishable from a fully
	// folded hinge and would otherwise switch the keyboard off at random.
	//
	// The comparison is circular, so the genuine 359 -> 0 wrap at full fold is
	// a 1 degree change and is never rejected.
	MaxSlewRate float64
}

// DefaultConfig returns the calibration verified on an HP ENVY x360
// Convertible 15-bp1xx. These are starting points, not universal constants;
// other chassis report different ranges and may wrap in the other direction.
func DefaultConfig() Config {
	return Config{
		WrapGuard:    30,
		LaptopMax:    180,
		TentMin:      210,
		TabletMin:    300,
		EnterSamples: 1,
		LeaveSamples: 3,
		MaxSlewRate:  720,
	}
}

// Valid reports whether the thresholds are internally consistent. A config
// that fails this would produce undefined posture bands.
func (c Config) Valid() bool {
	return c.WrapGuard >= 0 &&
		c.WrapGuard < c.LaptopMax &&
		c.LaptopMax < c.TentMin &&
		c.TentMin <= c.TabletMin &&
		c.EnterSamples >= 1 &&
		c.LeaveSamples >= 1
}

// Reading is one observation from a source. Pointer fields are nil when the
// underlying hardware cannot supply that signal, which is common: many
// machines expose a lid switch but no hinge angle, or vice versa.
type Reading struct {
	// Angle is the hinge angle in degrees, or nil if unavailable.
	Angle *float64

	// LidClosed is the lid switch state, or nil if unavailable. It resolves the
	// ambiguity at small angles, where "folded all the way around" and "shut"
	// produce the same reading.
	LidClosed *bool

	// Trusted is false when the source knows this reading is unreliable, such
	// as an accelerometer-derived angle taken while the machine is being moved,
	// or one computed near a geometric singularity. Untrusted readings advance
	// no debounce counters and commit nothing.
	Trusted bool

	// At is the observation time, used for slew-rate rejection.
	At time.Time
}

// State is the policy's memory between readings. The zero value is a valid
// starting state meaning "nothing decided yet".
type State struct {
	// Posture is the currently committed posture.
	Posture Posture

	candidate Posture
	samples   int
	lastAngle *float64
	lastAt    time.Time
}

// Transition describes a committed posture change. Reason is a short
// human-readable explanation intended for logs and for `hinged doctor`,
// because "why did my keyboard just switch off" is the question this project
// exists to answer.
type Transition struct {
	From   Posture
	To     Posture
	Reason string

	// SwitchChanged reports whether this transition also changes the value of
	// SW_TABLET_MODE. Posture can change without the switch changing, e.g.
	// tent -> tablet, and the uinput sink should stay quiet in that case.
	SwitchChanged bool
}

// classify maps a single angle to a posture, with no memory or debouncing.
// It is the pure geometry of the decision, separated so it can be tested
// independently of the state machine that stabilises it.
func classify(angle float64, lidClosed *bool, c Config) Posture {
	if angle < c.WrapGuard {
		// Ambiguous: the sensor reads near zero both when shut and when folded
		// the whole way around. The lid switch is the only thing that
		// distinguishes them. Without it, assume folded rather than shut,
		// because asserting the switch on a genuinely shut lid is the more
		// damaging error: it suppresses the keyboard on wake.
		if lidClosed != nil && *lidClosed {
			return PostureClosed
		}
		return PostureTablet
	}
	switch {
	case angle >= c.TabletMin:
		return PostureTablet
	case angle >= c.TentMin:
		return PostureTent
	case angle <= c.LaptopMax:
		return PostureLaptop
	default:
		// Inside the hysteresis dead band between LaptopMax and TentMin.
		// Deliberately undecidable: Step keeps the existing posture here.
		return PostureUnknown
	}
}

// required returns how many agreeing samples are needed to move from one
// posture to another. The asymmetry is the point: asserting the switch should
// feel immediate, releasing it should be conservative.
func required(from, to Posture, c Config) int {
	if to.TabletSwitch() && !from.TabletSwitch() {
		return c.EnterSamples
	}
	if !to.TabletSwitch() && from.TabletSwitch() {
		return c.LeaveSamples
	}
	// Posture changes that do not move the switch (tent <-> tablet) follow the
	// enter path: they are cosmetic and should track the hardware promptly.
	return c.EnterSamples
}

// Step advances the policy by one reading. It returns the new state and, when
// a posture change was committed, a non-nil Transition.
//
// Step never blocks, sleeps, or performs I/O. Callers own the polling loop and
// the clock, which is what makes the entire decision path testable.
func Step(s State, r Reading, c Config) (State, *Transition) {
	// An untrusted reading tells us nothing. Hold the current posture and the
	// current debounce progress rather than treating absence of signal as
	// evidence of change.
	if !r.Trusted || r.Angle == nil {
		return s, nil
	}
	angle := *r.Angle

	// Reject implausibly fast movement. During a carry or a sensor glitch the
	// reported angle can jump across the whole range; committing on that
	// produces exactly the spurious mode flip this policy exists to prevent.
	// The reading still updates the slew baseline so a genuine fast fold
	// settles on the following sample rather than being rejected forever.
	if c.MaxSlewRate > 0 && s.lastAngle != nil && !s.lastAt.IsZero() {
		if dt := r.At.Sub(s.lastAt).Seconds(); dt > 0 {
			if circularDelta(angle, *s.lastAngle)/dt > c.MaxSlewRate {
				s.lastAngle = &angle
				s.lastAt = r.At
				return s, nil
			}
		}
	}
	s.lastAngle = &angle
	s.lastAt = r.At

	target := classify(angle, r.LidClosed, c)

	// Inside the dead band, or otherwise undecidable: hold position and discard
	// any partial debounce, since the hinge is sitting in the ambiguous zone.
	if target == PostureUnknown {
		s.candidate = PostureUnknown
		s.samples = 0
		return s, nil
	}

	// Already where we want to be.
	if target == s.Posture {
		s.candidate = PostureUnknown
		s.samples = 0
		return s, nil
	}

	// First reading ever: commit immediately. There is no prior posture to
	// debounce against, and starting in the wrong mode is worse than starting
	// decisively.
	if s.Posture == PostureUnknown {
		from := s.Posture
		s.Posture = target
		s.candidate = PostureUnknown
		s.samples = 0
		return s, &Transition{
			From:          from,
			To:            target,
			Reason:        "initial posture from first trusted reading",
			SwitchChanged: from.TabletSwitch() != target.TabletSwitch(),
		}
	}

	if target == s.candidate {
		s.samples++
	} else {
		s.candidate = target
		s.samples = 1
	}

	if s.samples < required(s.Posture, target, c) {
		return s, nil
	}

	from := s.Posture
	s.Posture = target
	s.candidate = PostureUnknown
	s.samples = 0
	return s, &Transition{
		From:          from,
		To:            target,
		Reason:        reasonFor(from, target, angle),
		SwitchChanged: from.TabletSwitch() != target.TabletSwitch(),
	}
}

func reasonFor(from, to Posture, angle float64) string {
	switch {
	case to == PostureClosed:
		return "lid closed"
	case from == PostureClosed:
		return "lid opened"
	case to.TabletSwitch() && !from.TabletSwitch():
		return "hinge folded past the tent threshold"
	case !to.TabletSwitch() && from.TabletSwitch():
		return "hinge returned below the laptop threshold"
	default:
		return "hinge angle changed posture"
	}
}

// circularDelta is the shortest angular distance between two hinge readings,
// in degrees. A hinge that wraps 359 -> 0 at full fold has moved one degree,
// not 359, and treating that as a huge jump would reject exactly the readings
// that matter most.
func circularDelta(a, b float64) float64 {
	d := abs(a - b)
	for d > 360 {
		d -= 360
	}
	if d > 180 {
		return 360 - d
	}
	return d
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
