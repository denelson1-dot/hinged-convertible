//go:build linux && (amd64 || arm64 || 386 || arm || riscv64 || s390x)

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

// wordBytes is the size of a C long on this platform: 8 on 64-bit, 4 on
// 32-bit. The expression is the standard compile-time width probe -- ^uint(0)
// is all ones, and shifting right by 63 yields 1 only when uint is 64 bits.
const wordBytes = (32 << (^uint(0) >> 63)) / 8

// inputEventSize is sizeof(struct input_event) for this architecture.
//
// The struct is two longs of timestamp followed by __u16 type, __u16 code and
// __s32 value:
//
//	64-bit: 8 + 8 + 2 + 2 + 4 = 24 bytes
//	32-bit: 4 + 4 + 2 + 2 + 4 = 16 bytes
//
// Hardcoding 24 desynchronises the event stream permanently on 32-bit
// userspace: a read for 24 bytes consumes one and a half events, and every
// subsequent read interprets the middle of a timestamp as a type field. This
// is not hypothetical for this project -- Bay Trail and Cherry Trail
// convertibles ship 32-bit UEFI and are routinely given i386 installs.
//
// The kernel emits the compat layout for 32-bit userspace on a 64-bit kernel
// too (see input_event_size() in drivers/input/input-compat.h), so deriving
// the size from the userspace word width is correct in both cases.
const inputEventSize = 2*wordBytes + 8

// Offsets of the fields after the timestamp.
const (
	offType  = 2 * wordBytes
	offCode  = offType + 2
	offValue = offCode + 2
)

// eviocgsw is EVIOCGSW(wordBytes): read the current state of all switches.
//
// The encoding is _IOC(_IOC_READ, 'E', 0x1b, size), which on the asm-generic
// layout used by x86, ARM, arm64, RISC-V and s390 is:
//
//	dir=2 (read) << 30 | size << 16 | type='E'(0x45) << 8 | nr=0x1b
//
// The request size must match the buffer, and the kernel's switch bitmap is
// one long, so it varies with the architecture like the struct above.
//
// This encoding is NOT universal: MIPS and PowerPC use _IOC_SIZEBITS 13 and
// _IOC_DIRBITS 3, giving a different direction shift and therefore a
// different constant. No convertible laptop ships either architecture, so
// rather than compute an untested encoding, the build is restricted to the
// architectures this has been reasoned through -- a compile error is a far
// better outcome than an ioctl that silently reads the wrong memory.
const eviocgsw = (2 << 30) | (wordBytes << 16) | (0x45 << 8) | 0x1b

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
// event.
//
// This is what distinguishes an inert switch from one that is merely idle: an
// inert device reports 0 forever and never emits anything, while a working one
// reports its true current position immediately.
// It deliberately does not use os.File.Fd(). That method is not a getter: it
// calls SetBlocking(), which clears O_NONBLOCK and deregisters the descriptor
// from Go's runtime poller. Afterwards Close() can no longer interrupt a
// goroutine blocked in Read, so every reader leaks along with a pinned OS
// thread. SyscallConn.Control hands over the descriptor without that side
// effect, and keeps the File alive for the duration of the call.
func (s *Switch) State(code int) (bool, error) {
	rc, err := s.f.SyscallConn()
	if err != nil {
		return false, err
	}
	var mask [wordBytes]byte
	var errno syscall.Errno
	if err := rc.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall(
			syscall.SYS_IOCTL, fd, uintptr(eviocgsw), uintptr(unsafe.Pointer(&mask[0])))
	}); err != nil {
		return false, err
	}
	if errno != 0 {
		return false, fmt.Errorf("EVIOCGSW on %s: %w", s.path, errno)
	}
	var bits uint64
	if wordBytes == 8 {
		bits = hostEndian.Uint64(mask[:])
	} else {
		bits = uint64(hostEndian.Uint32(mask[:]))
	}
	return bits&(1<<uint(code)) != 0, nil
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
		if err := readFull(s.f, buf); err != nil {
			return Event{}, err
		}
		if hostEndian.Uint16(buf[offType:]) != evSW {
			continue
		}
		code := hostEndian.Uint16(buf[offCode:])
		value := int32(hostEndian.Uint32(buf[offValue:]))
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

func readFull(f *os.File, buf []byte) error {
	n := 0
	for n < len(buf) {
		m, err := f.Read(buf[n:])
		if err != nil {
			return err
		}
		if m == 0 {
			return fmt.Errorf("short read from %s", f.Name())
		}
		n += m
	}
	return nil
}

// hostEndian is the byte order the kernel uses for evdev structs, which is
// always the machine's native order rather than a fixed one.
var hostEndian = nativeEndian()

func nativeEndian() binary.ByteOrder {
	var probe uint16 = 0x0102
	if *(*byte)(unsafe.Pointer(&probe)) == 0x02 {
		return binary.LittleEndian
	}
	return binary.BigEndian
}
