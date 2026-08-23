// Package policy converts noisy hinge-angle readings into a stable posture
// decision. It is deliberately pure: no I/O, no clocks, no logging. Every
// input arrives in a Reading and every effect leaves as a Transition, which
// makes the whole decision surface testable without hardware.
//
// This is the part of the project that does not exist elsewhere. libinput
// already reacts to SW_TABLET_MODE; what no generic tool provides is a
// defensible policy for deciding when to assert it.
//
// # Safety asymmetry
//
// Asserting SW_TABLET_MODE causes libinput to switch off the internal keyboard
// and touchpad. Releasing it gives them back. Those two errors are not equally
// bad, so the policy is deliberately asymmetric:
//
//   - Asserting requires corroboration whenever the evidence is ambiguous.
//   - Releasing is permitted on weaker evidence, and several safety valves can
//     force it.
//
// When the policy cannot tell what is happening, it holds its current posture
// rather than guessing, and it never invents an assertion from a single
// surprising reading.
package policy

import (
	"errors"
	"math"
	"time"
)

// Posture is the physical arrangement of the machine. It is richer than the
// binary SW_TABLET_MODE switch the kernel exposes, because a hinge angle can
// distinguish states that the switch cannot.
type Posture int

const (
	// PostureUnknown means no posture has been committed yet, or the current
	// reading is undecidable. It is never a committed answer on its own.
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
// Tent asserts as well as tablet: in both the keyboard is folded away from the
// user and would otherwise register keypresses against whatever the machine is
// resting on. This is why TentMin, not TabletMin, is the threshold that matters
// for input suppression.
//
// Closed deliberately does not assert. A shut lid is not tablet mode, and
// asserting there would leave the keyboard suppressed on wake.
func (p Posture) TabletSwitch() bool {
	return p == PostureTent || p == PostureTablet
}

// Config holds the tunable policy. Angles are in degrees. The zero value is not
// usable; start from DefaultConfig.
type Config struct {
	// WrapGuard is the angle below which a reading may represent a hinge that
	// has folded past 360 and wrapped back to near zero.
	//
	// A small angle is genuinely ambiguous: it means "folded all the way back",
	// "lid shut", or "the sensor glitched". Which one is resolved by the lid
	// switch and by the posture already committed, never by the angle alone.
	WrapGuard float64

	// LaptopMax is the angle at or below which the machine is in laptop
	// posture. Must be < TentMin; the gap between them is the hysteresis dead
	// band that stops the posture flapping while the hinge rests near the
	// boundary.
	LaptopMax float64

	// TentMin is the angle at or above which the keyboard is folded away. This
	// is the threshold that asserts SW_TABLET_MODE.
	TentMin float64

	// TabletMin is the angle at or above which the machine is fully folded.
	// Must be > TentMin so that tent posture is reachable.
	TabletMin float64

	// EnterSamples is how many agreeing readings assert the switch on the
	// ordinary path, where the hinge has visibly travelled through the dead
	// band. Low by design: entering late means the keyboard is already
	// face-down registering keypresses.
	EnterSamples int

	// AmbiguousSamples is how many agreeing readings assert the switch when the
	// evidence is weak: the very first reading after startup, the first reading
	// after a gap in data, or a near-zero angle that might be a wrap, a shut
	// lid, or a glitch.
	//
	// This must be greater than EnterSamples. It is the main defence against a
	// single spurious reading switching off the user's keyboard.
	AmbiguousSamples int

	// LeaveSamples is how much accumulated evidence releases the switch. See
	// LeaveDecay: evidence accumulates rather than requiring a consecutive run,
	// so that a sensor alternating between postures still eventually releases.
	LeaveSamples float64

	// LeaveDecay is how much accumulated leave-evidence is cancelled by one
	// reading that agrees with the current asserting posture.
	//
	// A value of 1 makes releasing require a strictly consecutive run, which
	// lets a sensor alternating 50/50 between laptop and tablet keep the
	// keyboard disabled forever. A value below 1 guarantees that sustained
	// disagreement eventually wins, which is the safe direction.
	LeaveDecay float64

	// MaxSlewRate is the fastest plausible angular change in degrees per
	// second. Zero disables the check.
	MaxSlewRate float64

	// RejectTolerance is how much accumulated rejection evidence causes the
	// policy to abandon its baseline and re-establish one from scratch.
	//
	// Some tolerance is essential: without it a single rejected reading would
	// immediately become the reference point, so any glitch repeated twice
	// would be accepted -- turning the filter into a glitch amplifier. But a
	// strictly consecutive count is not enough either, because a sensor
	// alternating between a plausible and an implausible value never produces
	// a consecutive run, and the baseline locks onto whichever value it
	// happened to accept. Evidence therefore accumulates and decays, as with
	// LeaveSamples.
	RejectTolerance float64

	// RejectDecay is how much accumulated rejection evidence is cancelled by
	// one accepted reading. Must be below 1 so that sustained disagreement
	// eventually forces a fresh baseline.
	RejectDecay float64

	// SensorLostGrace is how long the switch may stay asserted with no fresh
	// reading before Tick force-releases it. Zero disables the dead-man path,
	// which is only appropriate when something else guarantees release.
	SensorLostGrace time.Duration

	// StaleAfter is the gap in data after which the previous reading is no
	// longer a meaningful baseline. Readings after such a gap are treated as
	// ambiguous.
	StaleAfter time.Duration
}

// DefaultConfig returns the calibration verified on an HP ENVY x360
// Convertible 15-bp1xx. The angles are starting points, not universal
// constants; other chassis report different ranges and may wrap the other way.
func DefaultConfig() Config {
	return Config{
		WrapGuard:        30,
		LaptopMax:        180,
		TentMin:          210,
		TabletMin:        300,
		EnterSamples:     1,
		AmbiguousSamples: 3,
		LeaveSamples:     3,
		LeaveDecay:       0.5,
		MaxSlewRate:      720,
		RejectTolerance:  3,
		RejectDecay:      0.5,
		StaleAfter:       2 * time.Second,
		SensorLostGrace:  5 * time.Second,
	}
}

// Validation errors returned by Config.Validate.
var (
	ErrThresholdOrder    = errors.New("policy: thresholds must satisfy 0 < WrapGuard < LaptopMax < TentMin < TabletMin")
	ErrThresholdFinite   = errors.New("policy: thresholds must be finite")
	ErrSampleCounts      = errors.New("policy: require 1 <= EnterSamples <= AmbiguousSamples and LeaveSamples >= 1")
	ErrDecayRange        = errors.New("policy: LeaveDecay must be in (0, 1]")
	ErrNegativeRateLimit = errors.New("policy: MaxSlewRate and MaxStepDegrees must not be negative")
	ErrRejectTolerance   = errors.New("policy: RejectTolerance must be at least 1")
)

// Validate reports why a config is unusable, or nil if it is sound.
//
// Step refuses to act on an invalid config rather than producing undefined
// posture bands, so this is enforced rather than advisory.
func (c Config) Validate() error {
	for _, v := range []float64{c.WrapGuard, c.LaptopMax, c.TentMin, c.TabletMin, c.LeaveSamples, c.LeaveDecay, c.MaxSlewRate, c.RejectTolerance, c.RejectDecay} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return ErrThresholdFinite
		}
	}
	// Strict ordering throughout. Equality is not merely useless here, it
	// silently deletes a posture: TentMin == TabletMin makes tent unreachable,
	// and WrapGuard == 0 disables wrap handling entirely.
	if !(c.WrapGuard > 0 && c.WrapGuard < c.LaptopMax && c.LaptopMax < c.TentMin && c.TentMin < c.TabletMin) {
		return ErrThresholdOrder
	}
	if c.EnterSamples < 1 || c.AmbiguousSamples < c.EnterSamples || c.LeaveSamples < 1 {
		return ErrSampleCounts
	}
	if !(c.LeaveDecay > 0 && c.LeaveDecay <= 1) || !(c.RejectDecay > 0 && c.RejectDecay < 1) {
		return ErrDecayRange
	}
	if c.MaxSlewRate < 0 {
		return ErrNegativeRateLimit
	}
	if c.RejectTolerance < 1 {
		return ErrRejectTolerance
	}
	return nil
}

// Reading is one observation from a source. Pointer fields are nil when the
// hardware cannot supply that signal, which is common: many machines expose a
// lid switch but no hinge angle, or vice versa.
type Reading struct {
	// Angle is the hinge angle in degrees. Values are normalised into
	// [0, 360) before use; non-finite values are discarded.
	//
	// A value type with an explicit presence flag rather than a *float64:
	// nothing can be nil-dereferenced, Reading stays comparable, and it
	// serialises directly for the D-Bus API.
	Angle    float64
	HasAngle bool

	// LidClosed is the lid switch state. A closed lid overrides the angle
	// entirely, so "unknown" and "open" must stay distinguishable.
	LidClosed OptBool

	// Trusted is false when the source knows this reading is unreliable, such
	// as an accelerometer-derived angle taken while the machine is moving.
	// Untrusted readings change nothing at all.
	Trusted bool

	// At is the observation time. It must come from a monotonic source; a zero
	// or backwards value causes the reading to be discarded rather than
	// silently bypassing the plausibility checks.
	At time.Time
}

// OptBool is a bool that may be absent. Many machines expose a lid switch but
// no hinge angle, or the reverse, and conflating "absent" with "false" here
// would mean a missing lid switch reading as a permanently open lid.
type OptBool struct {
	Value   bool
	Present bool
}

// Bool returns a present OptBool.
func Bool(v bool) OptBool { return OptBool{Value: v, Present: true} }

// IsClosed reports whether the lid is known to be closed.
func (o OptBool) IsClosed() bool { return o.Present && o.Value }

// IsOpen reports whether the lid is known to be open. This is deliberately not
// the negation of IsClosed: an absent reading is neither.
func (o OptBool) IsOpen() bool { return o.Present && !o.Value }

// State is the policy's memory between readings. The zero value is a valid
// starting state meaning "nothing decided yet".
type State struct {
	posture       Posture
	candidate     Posture
	samples       int
	leaveEvidence float64
	ambiguous     bool // the next assertion needs extra corroboration
	lastAngle     float64
	hasLast       bool
	lastAt        time.Time
	rejectScore   float64
}

// Posture returns the committed posture. It is an accessor rather than a field
// so that a caller cannot desynchronise it from the debounce and plausibility
// history that makes the decision safe.
func (s State) Posture() Posture { return s.posture }

// Rejected reports the accumulated rejection evidence against the current
// baseline. A persistently non-zero value means the sensor is fighting the
// policy, which is a different diagnosis from a sensor that has gone silent.
func (s State) Rejected() float64 { return s.rejectScore }

// Reason explains why a posture was chosen. It is an enumeration rather than a
// string so that logs, the D-Bus API and `hinged doctor` can switch on it
// instead of matching prose.
type Reason int

const (
	ReasonNone Reason = iota
	ReasonLidClosed
	ReasonWrapped
	ReasonStartupFolded
	ReasonUnreachable
	ReasonDeadBand
	ReasonAboveTablet
	ReasonAboveTent
	ReasonBelowLaptop
	ReasonSensorLost
)

// String returns a human-readable explanation. "Why did my keyboard just switch
// off" is the question this project exists to answer, so every transition
// carries an answer.
func (r Reason) String() string {
	switch r {
	case ReasonLidClosed:
		return "lid closed"
	case ReasonWrapped:
		return "hinge wrapped past 360 while already folded"
	case ReasonStartupFolded:
		return "started with the hinge folded past 360, lid open"
	case ReasonUnreachable:
		return "near-zero angle is not reachable from this posture"
	case ReasonDeadBand:
		return "hinge is inside the hysteresis dead band"
	case ReasonAboveTablet:
		return "hinge folded past the tablet threshold"
	case ReasonAboveTent:
		return "hinge folded past the tent threshold"
	case ReasonBelowLaptop:
		return "hinge returned below the laptop threshold"
	case ReasonSensorLost:
		return "sensor stopped reporting; releasing for safety"
	default:
		return "no reason recorded"
	}
}

// Transition describes a committed posture change.
type Transition struct {
	From  Posture
	To    Posture
	Angle float64

	// Reason explains the change.
	Reason Reason

	// SwitchChanged reports whether SW_TABLET_MODE changes value here. Posture
	// can change without the switch changing (tent to tablet), and a uinput
	// sink should stay quiet in that case.
	SwitchChanged bool
}

// normalize folds an angle into [0, 360). Firmware has been observed reporting
// negative angles when a calibration offset is applied below the mechanical
// stop, and nothing downstream should have to think about that.
func normalize(a float64) float64 {
	a = math.Mod(a, 360)
	if a < 0 {
		a += 360
	}
	return a
}

// classify maps one angle to a posture. It takes the committed posture because
// a near-zero reading is not decidable from the angle alone.
//
// It returns PostureUnknown to mean "no opinion, hold what you have", which is
// distinct from any committed answer.
func classify(angle float64, lidClosed OptBool, current Posture, c Config) (Posture, Reason) {
	// A shut lid settles the question at any angle. Checked first and
	// unconditionally: a folded-then-closed machine reports a stale or
	// meaningless hinge angle, and must not keep the keyboard suppressed.
	if lidClosed.IsClosed() {
		return PostureClosed, ReasonLidClosed
	}

	if angle < c.WrapGuard {
		// Genuinely ambiguous. The hinge cannot reach a wrapped near-zero
		// reading from laptop posture without travelling through tent and
		// tablet first, so the committed posture is a sufficient discriminator
		// and no guessing is required.
		if current == PostureTent || current == PostureTablet {
			return PostureTablet, ReasonWrapped
		}
		// At startup there is no committed posture to reason from, so a machine
		// booted or restarted while already folded would otherwise never reach
		// tablet posture at all. A lid known to be OPEN settles it: the machine
		// cannot be shut, so a sustained near-zero angle is a genuine fold.
		// AmbiguousSamples still gates the commit, which is what separates a
		// real fold from a one-sample glitch.
		if current == PostureUnknown && lidClosed.IsOpen() {
			return PostureTablet, ReasonStartupFolded
		}
		// From laptop posture this is always a glitch, never a fold. The hinge
		// cannot travel from the laptop band to a wrapped near-zero without
		// passing through the tent and tablet bands on the way, so a near-zero
		// reading here contradicts the physical path and is discarded.
		//
		// With the lid state unknown it is undecidable in either direction, and
		// guessing wrong would either suppress the keyboard on wake or switch it
		// off at random. Hold in both cases.
		return PostureUnknown, ReasonUnreachable
	}

	switch {
	case angle >= c.TabletMin:
		return PostureTablet, ReasonAboveTablet
	case angle >= c.TentMin:
		return PostureTent, ReasonAboveTent
	case angle <= c.LaptopMax:
		return PostureLaptop, ReasonBelowLaptop
	default:
		// The hysteresis dead band. Deliberately no opinion: without it the
		// posture flaps while the hinge rests near a threshold.
		return PostureUnknown, ReasonDeadBand
	}
}

// enterSamplesFor returns how many agreeing readings are needed to assert the
// switch, given how trustworthy the current evidence is.
func enterSamplesFor(ambiguous bool, c Config) int {
	if ambiguous {
		return c.AmbiguousSamples
	}
	return c.EnterSamples
}

// Step advances the policy by one reading, returning the new state and, when a
// posture change was committed, a non-nil Transition.
//
// Step never blocks, sleeps, allocates unboundedly, or performs I/O. Callers
// own the polling loop and the clock, which is what makes the entire decision
// path testable without hardware.
func Step(s State, r Reading, c Config) (State, Transition, bool) {
	// An unusable config cannot be acted on safely. Holding is always
	// available and never disables anyone's keyboard.
	if c.Validate() != nil {
		return s, Transition{}, false
	}

	// An untrusted reading carries no information. Hold posture and debounce
	// progress alike, and do not disturb the plausibility baseline.
	if !r.Trusted || !r.HasAngle {
		return s, Transition{}, false
	}
	angle := r.Angle
	if math.IsNaN(angle) || math.IsInf(angle, 0) {
		return s, Transition{}, false
	}
	angle = normalize(angle)

	// A zero timestamp means the caller forgot to set At. Discarding is the
	// only safe response: treating it as valid would permanently disable every
	// time-based check with no diagnostic.
	if r.At.IsZero() {
		return s, Transition{}, false
	}

	if s.hasLast && !s.lastAt.IsZero() {
		dt := r.At.Sub(s.lastAt)
		switch {
		case dt <= 0:
			// Time did not advance. Rejected rather than allowed through,
			// because a backwards clock would otherwise bypass every
			// plausibility check and assert on a single reading.
			return s, Transition{}, false

		case c.StaleAfter > 0 && dt > c.StaleAfter:
			// Too long since the last reading for it to be a useful reference.
			// Re-baseline and demand corroboration before asserting.
			s.lastAngle, s.hasLast, s.lastAt = angle, true, r.At
			s.rejectScore, s.ambiguous = 0, true
			return s, Transition{}, false

		default:
			if implausible(angle, s.lastAngle, dt, c) {
				s.rejectScore++
				if s.rejectScore < c.RejectTolerance {
					// Hold the old baseline. Adopting a rejected value
					// immediately is what would let any glitch repeated twice
					// through the filter.
					s.lastAt = r.At
					return s, Transition{}, false
				}
				// The sensor has disagreed with our reference often enough
				// that the reference, not the sensor, is what to distrust.
				// Abandon it and re-establish from this reading, demanding
				// corroboration before any assertion.
				s.rejectScore, s.ambiguous = 0, true
			} else {
				s.rejectScore = math.Max(0, s.rejectScore-c.RejectDecay)
			}
		}
	} else {
		// No baseline yet: this is the first reading, or the first after a
		// gap. Nothing has been corroborated.
		s.ambiguous = true
	}

	s.lastAngle, s.hasLast, s.lastAt = angle, true, r.At

	target, reason := classify(angle, r.LidClosed, s.posture, c)

	// No opinion. Hold posture and preserve debounce progress: an undecidable
	// reading is not evidence against the candidate, and discarding progress
	// here biases towards leaving the keyboard disabled.
	if target == PostureUnknown {
		return s, Transition{}, false
	}

	// Accumulate evidence for releasing the switch separately from the
	// candidate counter, so that a sensor alternating between two asserting
	// postures cannot starve the release path indefinitely.
	if s.posture.TabletSwitch() {
		if target.TabletSwitch() {
			s.leaveEvidence = math.Max(0, s.leaveEvidence-c.LeaveDecay)
		} else {
			s.leaveEvidence++
		}
	} else {
		s.leaveEvidence = 0
	}

	if target == s.posture {
		s.candidate, s.samples = PostureUnknown, 0
		s.ambiguous = false
		return s, Transition{}, false
	}

	if target == s.candidate {
		s.samples++
	} else {
		s.candidate, s.samples = target, 1
	}

	if !s.enough(target, c) {
		return s, Transition{}, false
	}

	from := s.posture
	s.posture = target
	s.candidate, s.samples = PostureUnknown, 0
	s.leaveEvidence = 0
	s.ambiguous = false

	return s, Transition{
		From:          from,
		To:            target,
		Angle:         angle,
		Reason:        reason,
		SwitchChanged: from.TabletSwitch() != target.TabletSwitch(),
	}, true
}

// Tick advances time without a reading, and is how the switch gets released
// when the sensor stops answering.
//
// This is the dead-man path. Without it, a sensor that wedges while tablet
// mode is asserted leaves the keyboard suppressed indefinitely, because Step
// is only ever called when there is something to report. Releasing must never
// depend on the sensor's cooperation.
//
// Callers should invoke this on a timer regardless of read success, and
// especially on the read-error path.
func Tick(s State, now time.Time, c Config) (State, Transition, bool) {
	if c.Validate() != nil || c.SensorLostGrace <= 0 {
		return s, Transition{}, false
	}
	if !s.posture.TabletSwitch() || s.lastAt.IsZero() {
		return s, Transition{}, false
	}
	if now.Sub(s.lastAt) < c.SensorLostGrace {
		return s, Transition{}, false
	}
	from := s.posture
	s.posture = PostureLaptop
	s.candidate, s.samples, s.leaveEvidence = PostureUnknown, 0, 0
	s.ambiguous, s.hasLast, s.rejectScore = true, false, 0
	return s, Transition{
		From:          from,
		To:            PostureLaptop,
		Reason:        ReasonSensorLost,
		SwitchChanged: true,
	}, true
}

// Engine wraps the pure state machine with a validated config, so that
// misconfiguration is an error the caller must handle at construction rather
// than a silent no-op on every sample.
type Engine struct {
	cfg   Config
	state State
}

// New returns an Engine, or an error describing why the config is unusable.
func New(c Config) (*Engine, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &Engine{cfg: c}, nil
}

// Step advances the engine by one reading.
func (e *Engine) Step(r Reading) (Transition, bool) {
	var tr Transition
	var ok bool
	e.state, tr, ok = Step(e.state, r, e.cfg)
	return tr, ok
}

// Tick advances the engine's clock without a reading.
func (e *Engine) Tick(now time.Time) (Transition, bool) {
	var tr Transition
	var ok bool
	e.state, tr, ok = Tick(e.state, now, e.cfg)
	return tr, ok
}

// Posture returns the currently committed posture.
func (e *Engine) Posture() Posture { return e.state.posture }

// Rejected returns accumulated rejection evidence against the sensor baseline.
func (e *Engine) Rejected() float64 { return e.state.rejectScore }

// Config returns the validated configuration in use.
func (e *Engine) Config() Config { return e.cfg }

// enough reports whether the accumulated evidence justifies committing target.
func (s State) enough(target Posture, c Config) bool {
	switch {
	case target.TabletSwitch() && !s.posture.TabletSwitch():
		// Asserting: the dangerous direction. Ambiguous evidence needs more.
		return s.samples >= enterSamplesFor(s.ambiguous, c)
	case !target.TabletSwitch() && s.posture.TabletSwitch():
		// Releasing: the safe direction, driven by accumulated evidence.
		return s.leaveEvidence >= c.LeaveSamples
	default:
		// Cosmetic moves that leave the switch unchanged.
		return s.samples >= c.EnterSamples
	}
}

// implausible reports whether a reading is too far from the previous one to be
// a real hinge movement.
//
// This is deliberately a rate test and not an absolute one. Over a long
// sampling interval a large angular change genuinely is plausible, so a fixed
// degrees-per-sample cap would reject real folds on slow sensors.
//
// A consequence worth stating plainly: circularDelta can never exceed 180, so
// this check cannot reject anything once the interval exceeds
// 180/MaxSlewRate seconds -- a quarter second at the default. Slow sensors get
// no protection from this gate at all and rely instead on the posture-gated
// wrap guard in classify and on AmbiguousSamples. SlewGateCeiling exposes this
// so that `hinged doctor` can report it rather than leaving it implicit.
func implausible(angle, last float64, dt time.Duration, c Config) bool {
	if c.MaxSlewRate <= 0 {
		return false
	}
	return circularDelta(angle, last)/dt.Seconds() > c.MaxSlewRate
}

// SlewGateCeiling returns the longest sampling interval at which the slew gate
// can still reject anything. Beyond it the gate is arithmetically inert,
// because the largest possible circular delta is 180 degrees.
//
// A caller polling more slowly than this should say so in its diagnostics
// instead of implying a protection that is not active.
func SlewGateCeiling(c Config) time.Duration {
	if c.MaxSlewRate <= 0 {
		return 0
	}
	return time.Duration(180 / c.MaxSlewRate * float64(time.Second))
}

// circularDelta is the shortest angular distance between two hinge readings,
// in degrees, always within [0, 180]. A hinge that wraps 359 to 0 at full fold
// has moved one degree, not 359, and treating that as a huge jump would reject
// exactly the readings that matter most.
func circularDelta(a, b float64) float64 {
	d := math.Abs(math.Mod(a-b, 360))
	if d > 180 {
		return 360 - d
	}
	return d
}
