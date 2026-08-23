package source

import (
	"encoding/binary"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// evdev event types and switch codes, from
// include/uapi/linux/input-event-codes.h.
const (
	evSW = 0x05

	SwLid        = 0x00 // SW_LID
	SwTabletMode = 0x01 // SW_TABLET_MODE
)

// eviocgsw is EVIOCGSW(8): read the current state of all switches on a device.
//
// The encoding is _IOC(_IOC_READ, 'E', 0x1b, 8):
//
//	dir=2 (read) << 30 | size=8 << 16 | type='E'(0x45) << 8 | nr=0x1b
//
// It is spelled out rather than pulled from golang.org/x/sys so the core stays
// dependency-free.
const eviocgsw = (2 << 30) | (8 << 16) | (0x45 << 8) | 0x1b

// inputEventSize is sizeof(struct input_event) on 64-bit Linux:
// struct timeval (two 64-bit longs) + __u16 type + __u16 code + __s32 value.
const inputEventSize = 24

// Switch reads an evdev switch device such as a lid or tablet-mode switch.
type Switch struct {
	name string
	path string
	f    *os.File
}

// OpenSwitch opens an evdev node by its handler name, e.g. "event11".
func OpenSwitch(handler, name string) (*Switch, error) {
	path := "/dev/input/" + handler
	f, err := os.Open(path)
	if err != nil {
		if os.IsPermission(err) {
			return nil, fmt.Errorf(
				"%s is not readable by this user: install the udev rule in packaging/udev, "+
					"or add yourself to the 'input' group and log back in: %w", path, err)
		}
		return nil, err
	}
	return &Switch{name: name, path: path, f: f}, nil
}

// Name returns the device name reported by the kernel.
func (s *Switch) Name() string { return s.name }

// Path returns the device node.
func (s *Switch) Path() string { return s.path }

// State queries the current value of one switch code without waiting for an
// event. This is what distinguishes an inert switch from one that is merely
// idle: an inert device reports 0 forever and never emits anything, while a
// working one reports its true current position at startup.
func (s *Switch) State(code int) (bool, error) {
	var mask uint64
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		s.f.Fd(),
		uintptr(eviocgsw),
		uintptr(unsafe.Pointer(&mask)),
	)
	if errno != 0 {
		return false, fmt.Errorf("EVIOCGSW on %s: %w", s.path, errno)
	}
	return mask&(1<<uint(code)) != 0, nil
}

// Event is one switch transition.
type Event struct {
	Code  int
	Value bool
}

// Read blocks until the next switch event on this device.
//
// Non-switch events are skipped rather than returned, so callers only ever see
// posture-relevant transitions.
func (s *Switch) Read() (Event, error) {
	buf := make([]byte, inputEventSize)
	for {
		if _, err := readFull(s.f, buf); err != nil {
			return Event{}, err
		}
		typ := binary.LittleEndian.Uint16(buf[16:18])
		if typ != evSW {
			continue
		}
		code := binary.LittleEndian.Uint16(buf[18:20])
		value := int32(binary.LittleEndian.Uint32(buf[20:24]))
		return Event{Code: int(code), Value: value != 0}, nil
	}
}

// Close releases the device.
func (s *Switch) Close() error {
	if s.f == nil {
		return nil
	}
	return s.f.Close()
}

func readFull(f *os.File, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := f.Read(buf[n:])
		if err != nil {
			return n, err
		}
		if m == 0 {
			return n, fmt.Errorf("short read from %s", f.Name())
		}
		n += m
	}
	return n, nil
}
