// Package probe discovers what a machine can tell us about its posture.
//
// Everything here works from /proc and /sys and needs no privileges. That is
// deliberate: the first question a user with unsupported hardware asks is "why
// doesn't this work on my laptop", and they must be able to answer it without
// first being told to run something as root.
package probe

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Linux evdev switch codes. The SW bitmap in /proc/bus/input/devices is a hex
// mask of these; see include/uapi/linux/input-event-codes.h.
const (
	swLid        = 0 // SW_LID
	swTabletMode = 1 // SW_TABLET_MODE
)

// Report is the full picture of a machine's posture-sensing hardware.
type Report struct {
	Machine   Machine
	Switches  []SwitchDevice
	Hinges    []HingeSensor
	Accels    []AccelSensor
	Uinput    Access
	Mechanism string // the source `hinged` would choose, given this hardware
	Notes     []string
}

// Machine identifies the chassis, which is how vendors' quirks are keyed.
type Machine struct {
	Vendor      string
	Product     string
	ChassisType int
	Kernel      string
}

// ChassisDescription names the DMI chassis type. Types 31 and 32 are the ones
// the kernel's intel-vbtn and intel-hid drivers allow-list for tablet-mode
// reporting; a convertible reporting type 10 is a common reason the kernel
// stays silent about tablet mode.
func (m Machine) ChassisDescription() string {
	switch m.ChassisType {
	case 8:
		return "Portable"
	case 9:
		return "Laptop"
	case 10:
		return "Notebook"
	case 30:
		return "Tablet"
	case 31:
		return "Convertible"
	case 32:
		return "Detachable"
	case 0:
		return "unknown"
	default:
		return fmt.Sprintf("type %d", m.ChassisType)
	}
}

// SwitchDevice is an evdev node exposing EV_SW capabilities.
type SwitchDevice struct {
	Name       string
	Handler    string // e.g. "event11"
	Sysfs      string
	TabletMode bool // advertises SW_TABLET_MODE
	Lid        bool // advertises SW_LID
	DevNode    string
	Access     Access
}

// HingeSensor is an IIO device reporting a hinge angle.
type HingeSensor struct {
	Path   string
	Name   string
	Label  string
	Raw    string // path to in_angl0_raw
	Scale  *float64
	Offset *float64
	Access Access
}

// Units reports how a raw reading converts to degrees.
//
// The IIO ABI specifies angles in radians after scale and offset are applied.
// The original single-machine implementation this project grew from assumed
// the raw value was already degrees, which happened to be true on its hardware
// and is not true in general.
func (h HingeSensor) Units() string {
	if h.Scale == nil {
		return "raw (no scale attribute; value is likely already degrees)"
	}
	return fmt.Sprintf("radians after scale=%g", *h.Scale)
}

// AccelSensor is an IIO accelerometer. A lid/base pair can be used to compute
// a hinge angle on machines with no dedicated hinge sensor.
type AccelSensor struct {
	Path   string
	Name   string
	Label  string
	Access Access
}

// Access records whether we can actually read something, which is usually
// more useful than whether it exists.
type Access struct {
	Exists   bool
	Readable bool
	Reason   string
}

func (a Access) String() string {
	switch {
	case !a.Exists:
		return "absent"
	case a.Readable:
		return "readable"
	default:
		return "blocked: " + a.Reason
	}
}

func checkAccess(path string) Access {
	st, err := os.Stat(path)
	if err != nil {
		return Access{Reason: err.Error()}
	}
	a := Access{Exists: true}
	f, err := os.Open(path)
	if err != nil {
		a.Reason = permissionReason(err, st.Mode())
		return a
	}
	f.Close()
	a.Readable = true
	return a
}

func permissionReason(err error, mode os.FileMode) string {
	if os.IsPermission(err) {
		return fmt.Sprintf("permission denied (mode %v)", mode.Perm())
	}
	return err.Error()
}

// Run collects everything. It never fails as a whole: a machine missing any
// given subsystem is the normal case, and the report says so rather than
// erroring out.
func Run() Report {
	r := Report{
		Machine:  readMachine(),
		Switches: readSwitches(),
		Uinput:   checkAccess("/dev/uinput"),
	}
	r.Hinges, r.Accels = readIIO()
	r.Mechanism, r.Notes = choose(r)
	return r
}

func readMachine() Machine {
	m := Machine{
		Vendor:  strings.TrimSpace(readFile("/sys/class/dmi/id/sys_vendor")),
		Product: strings.TrimSpace(readFile("/sys/class/dmi/id/product_name")),
		Kernel:  strings.TrimSpace(readFile("/proc/sys/kernel/osrelease")),
	}
	if v, err := strconv.Atoi(strings.TrimSpace(readFile("/sys/class/dmi/id/chassis_type"))); err == nil {
		m.ChassisType = v
	}
	return m
}

func readFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// readSwitches parses /proc/bus/input/devices, which is world-readable. This
// lets us enumerate switch-capable devices without opening any of them, so
// discovery works for an unprivileged user even when reading the device node
// itself would be denied.
func readSwitches() []SwitchDevice {
	f, err := os.Open("/proc/bus/input/devices")
	if err != nil {
		return nil
	}
	defer f.Close()

	var out []SwitchDevice
	var cur SwitchDevice
	var haveSW bool

	flush := func() {
		if haveSW && (cur.TabletMode || cur.Lid) {
			if cur.Handler != "" {
				cur.DevNode = "/dev/input/" + cur.Handler
				cur.Access = checkAccess(cur.DevNode)
			}
			out = append(out, cur)
		}
		cur, haveSW = SwitchDevice{}, false
	}

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.TrimSpace(line) == "":
			flush()
		case strings.HasPrefix(line, "N: Name="):
			cur.Name = strings.Trim(strings.TrimPrefix(line, "N: Name="), `"`)
		case strings.HasPrefix(line, "S: Sysfs="):
			cur.Sysfs = strings.TrimPrefix(line, "S: Sysfs=")
		case strings.HasPrefix(line, "H: Handlers="):
			for _, h := range strings.Fields(strings.TrimPrefix(line, "H: Handlers=")) {
				if strings.HasPrefix(h, "event") {
					cur.Handler = h
				}
			}
		case strings.HasPrefix(line, "B: SW="):
			mask, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, "B: SW=")), 16, 64)
			if err == nil {
				haveSW = true
				cur.TabletMode = mask&(1<<swTabletMode) != 0
				cur.Lid = mask&(1<<swLid) != 0
			}
		}
	}
	flush()
	return out
}

// readIIO enumerates the IIO subsystem, separating hinge-angle sensors from
// accelerometers. Device numbering is not stable across boots, so everything
// is matched by name or label and never by index.
func readIIO() ([]HingeSensor, []AccelSensor) {
	paths, _ := filepath.Glob("/sys/bus/iio/devices/iio:device*")
	sort.Strings(paths)

	var hinges []HingeSensor
	var accels []AccelSensor

	for _, p := range paths {
		name := strings.TrimSpace(readFile(filepath.Join(p, "name")))
		label := strings.TrimSpace(readFile(filepath.Join(p, "label")))

		raw := filepath.Join(p, "in_angl0_raw")
		if _, err := os.Stat(raw); err == nil {
			h := HingeSensor{
				Path:   p,
				Name:   name,
				Label:  label,
				Raw:    raw,
				Access: checkAccess(raw),
				Scale:  readFloat(filepath.Join(p, "in_angl0_scale")),
				Offset: readFloat(filepath.Join(p, "in_angl0_offset")),
			}
			hinges = append(hinges, h)
			continue
		}
		if strings.Contains(name, "accel") || strings.Contains(label, "accel") {
			accels = append(accels, AccelSensor{
				Path:   p,
				Name:   name,
				Label:  label,
				Access: checkAccess(filepath.Join(p, "name")),
			})
		}
	}
	return hinges, accels
}

func readFloat(path string) *float64 {
	s := strings.TrimSpace(readFile(path))
	if s == "" {
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil
	}
	return &v
}

// choose applies the detection priority and explains the outcome. The
// explanation matters as much as the choice: it is what a user pastes into a
// bug report.
func choose(r Report) (string, []string) {
	var notes []string

	var tabletSwitches []SwitchDevice
	for _, s := range r.Switches {
		if s.TabletMode {
			tabletSwitches = append(tabletSwitches, s)
		}
	}

	if len(tabletSwitches) > 0 {
		names := make([]string, 0, len(tabletSwitches))
		blocked := 0
		for _, s := range tabletSwitches {
			names = append(names, fmt.Sprintf("%q (%s)", s.Name, s.Handler))
			if !s.Access.Readable {
				blocked++
			}
		}
		notes = append(notes, fmt.Sprintf(
			"The kernel already advertises SW_TABLET_MODE on %d device(s): %s.",
			len(tabletSwitches), strings.Join(names, ", ")))
		notes = append(notes,
			"libinput reacts to that switch on its own, disabling the internal keyboard "+
				"and touchpad. If the switch actually fires when you fold the machine, you may "+
				"not need hinged at all. Run `hinged watch` to find out.")
		if blocked > 0 {
			notes = append(notes, fmt.Sprintf(
				"%d of them cannot be read by this user. Install the udev rule "+
					"(packaging/udev) or add yourself to the 'input' group to check whether "+
					"they actually fire.", blocked))
		}
	}

	if len(r.Hinges) > 0 {
		h := r.Hinges[0]
		notes = append(notes, fmt.Sprintf(
			"Hinge angle sensor present at %s (%s), %s.", h.Path, h.Name, h.Access))
		if len(tabletSwitches) > 0 {
			notes = append(notes,
				"Both a switch and a hinge sensor are present. If the switch turns out to be "+
					"inert or stuck, hinged can derive posture from the angle instead and "+
					"synthesize a correct switch.")
		}
		if h.Access.Readable {
			return "iio-hinge", notes
		}
	}

	if len(tabletSwitches) > 0 {
		return "evdev-switch", notes
	}
	if len(r.Accels) >= 2 {
		notes = append(notes, fmt.Sprintf(
			"No hinge sensor, but %d accelerometers were found. A lid/base pair can be used "+
				"to compute the angle, with jerk and tilt gating.", len(r.Accels)))
		return "accel-pair", notes
	}
	if len(r.Accels) == 1 {
		notes = append(notes,
			"Only one accelerometer and no switch or hinge sensor. Posture can only be "+
				"guessed from screen orientation, which is unreliable; this is opt-in.")
		return "accel-single", notes
	}

	notes = append(notes,
		"No usable posture source was detected. hinged can still be driven manually "+
			"via `hinged toggle` or its D-Bus API.")
	return "manual", notes
}
