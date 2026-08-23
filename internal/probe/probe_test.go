package probe

import (
	"strings"
	"testing"
)

// A record captured verbatim from the reference machine, plus the HP WMI
// device that carries both switch and key capabilities.
const realDevices = `I: Bus=0019 Vendor=0000 Product=0005 Version=0000
N: Name="Lid Switch"
P: Phys=PNP0C0D/button/input0
S: Sysfs=/devices/platform/PNP0C0D:01/input/input0
H: Handlers=event0 
B: EV=21
B: SW=1

I: Bus=0019 Vendor=0000 Product=0000 Version=0000
N: Name="Intel Virtual Switches"
S: Sysfs=/devices/pci0000:00/INT33D6:01/input/input18
H: Handlers=event11 
B: EV=21
B: SW=2

I: Bus=0019 Vendor=0000 Product=0000 Version=0000
N: Name="HP WMI hotkeys"
S: Sysfs=/devices/virtual/input/input19
H: Handlers=kbd event12 
B: EV=33
B: SW=2

I: Bus=0000 Vendor=0000 Product=0000 Version=0000
N: Name="HDA Intel PCH Headphone"
H: Handlers=event15 
B: EV=21
B: SW=4

`

func TestParseSwitchesFindsOnlyPostureDevices(t *testing.T) {
	got, err := parseSwitches(strings.NewReader(realDevices))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d devices, want 3 (audio jacks carry EV_SW but not lid or tablet mode): %+v", len(got), got)
	}

	byHandler := map[string]SwitchDevice{}
	for _, d := range got {
		byHandler[d.Handler] = d
	}
	if d := byHandler["event0"]; !d.Lid || d.TabletMode {
		t.Errorf("event0 = %+v, want lid only", d)
	}
	if d := byHandler["event11"]; !d.TabletMode || d.Lid {
		t.Errorf("event11 = %+v, want tablet mode only", d)
	}
	if _, ok := byHandler["event15"]; ok {
		t.Error("a headphone jack switch was treated as a posture device")
	}
}

// The handler line can list several handlers; only the event node is usable.
func TestParseSwitchesPicksEventHandler(t *testing.T) {
	got, _ := parseSwitches(strings.NewReader(realDevices))
	for _, d := range got {
		if d.Name == "HP WMI hotkeys" && d.Handler != "event12" {
			t.Errorf("handler = %q, want event12 (the kbd handler must be ignored)", d.Handler)
		}
	}
}

// Device names are copied verbatim from uinput and USB product strings, so a
// name containing a newline must not be able to forge a record claiming that
// the real keyboard is a tablet-mode switch.
func TestParseSwitchesResistsNameInjection(t *testing.T) {
	hostile := "I: Bus=0003 Vendor=dead Product=beef Version=0001\n" +
		"N: Name=\"evil\nB: SW=3\nH: Handlers=event3 kbd\nN: Name=x\"\n" +
		"H: Handlers=event99 \n" +
		"B: EV=3\n" +
		"\n"
	got, err := parseSwitches(strings.NewReader(hostile))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, d := range got {
		if d.Handler == "event3" {
			t.Fatalf("a crafted device name forged a record for event3: %+v", d)
		}
	}
}

func TestParseSwitchesHandlesMultiWordBitmaps(t *testing.T) {
	in := "N: Name=\"Wide\"\nH: Handlers=event7 \nB: SW=1 2\n\n"
	got, err := parseSwitches(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || !got[0].TabletMode {
		t.Errorf("multi-word bitmap dropped the device or misread it: %+v", got)
	}
}

func TestParseSwitchesEmptyInput(t *testing.T) {
	got, err := parseSwitches(strings.NewReader(""))
	if err != nil || len(got) != 0 {
		t.Errorf("empty input: got %v, %v", got, err)
	}
}

func TestChassisDescription(t *testing.T) {
	for typ, want := range map[int]string{
		31: "Convertible", 32: "Detachable", 10: "Notebook", 30: "Tablet", 0: "unknown",
	} {
		if got := (Machine{ChassisType: typ}).ChassisDescription(); got != want {
			t.Errorf("chassis %d = %q, want %q", typ, got, want)
		}
	}
}

// "absent" and "blocked" must never be conflated: they send a user down
// completely different diagnostic paths, and this tool exists to tell them
// apart.
func TestAccessStringDistinguishesAbsentFromBlocked(t *testing.T) {
	if got := (Access{}).String(); got != "absent" {
		t.Errorf("absent = %q", got)
	}
	if got := (Access{Exists: true, Readable: true}).String(); got != "readable" {
		t.Errorf("readable = %q", got)
	}
	blocked := Access{Exists: true, Reason: "permission denied"}.String()
	if !strings.HasPrefix(blocked, "blocked:") {
		t.Errorf("blocked = %q", blocked)
	}
}

// choose() is a pure function of a Report and needs no hardware.
func TestChooseReportsDetachableRatherThanGuessing(t *testing.T) {
	mech, notes := choose(Report{Machine: Machine{ChassisType: 32}})
	if mech != "detachable" {
		t.Errorf("mechanism = %q, want detachable", mech)
	}
	if len(notes) == 0 {
		t.Error("a detachable verdict must explain that angle policy does not apply")
	}
}

func TestChooseFallsBackToManual(t *testing.T) {
	mech, notes := choose(Report{})
	if mech != "manual" {
		t.Errorf("mechanism = %q, want manual when nothing is detected", mech)
	}
	if len(notes) == 0 {
		t.Error("an unsupported machine must be told why")
	}
}

func TestChooseReportsBlockedSwitchesDistinctly(t *testing.T) {
	r := Report{Switches: []SwitchDevice{{
		Name: "Intel Virtual Switches", Handler: "event11", TabletMode: true,
		Access: Access{Exists: true, Reason: "permission denied"},
	}}}
	_, notes := choose(r)
	joined := strings.Join(notes, " ")
	if !strings.Contains(joined, "cannot be read") {
		t.Errorf("a blocked switch must be reported as blocked, not absent: %q", joined)
	}
}
