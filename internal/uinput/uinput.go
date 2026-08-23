//go:build linux && (amd64 || arm64 || 386 || arm || riscv64 || s390x)

// Package uinput synthesizes a virtual SW_TABLET_MODE switch.
//
// This is the point of the whole project. Given a switch the kernel does not
// provide, libinput suspends the internal keyboard and touchpad on its own, and
// every desktop that already understands tablet mode reacts without any
// per-compositor code from us.
//
// It is hand-rolled rather than taken from a library because the available Go
// uinput packages model keyboards, mice and gamepads but not EV_SW, which is
// the one event type needed here. Owning it also means owning the device
// identity fields, which may determine whether libinput honours the device at
// all.
package uinput

import (
	"encoding/binary"
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"
)

// uinput ioctls, from include/uapi/linux/uinput.h. The base is 'U' (0x55).
//
//	_IO(type, nr)        = (type << 8) | nr
//	_IOW(type, nr, size) = (1 << 30) | (size << 16) | (type << 8) | nr
const (
	uiSetEvBit   = (1 << 30) | (4 << 16) | (0x55 << 8) | 100
	uiSetSwBit   = (1 << 30) | (4 << 16) | (0x55 << 8) | 109
	uiDevSetup   = (1 << 30) | (uinputSetupSize << 16) | (0x55 << 8) | 3
	uiDevCreate  = (0x55 << 8) | 1
	uiDevDestroy = (0x55 << 8) | 2
)

// Event types and codes, from include/uapi/linux/input-event-codes.h.
const (
	evSyn        = 0x00
	evSW         = 0x05
	synReport    = 0x00
	swTabletMode = 0x01
	swLid        = 0x00
)

// Bus types. BUS_VIRTUAL is the honest choice for a device that is not backed
// by hardware, and is what other synthetic-switch tools use.
const (
	BusVirtual = 0x06
	BusHost    = 0x19
	BusI8042   = 0x11
)

// uinputSetupSize is sizeof(struct uinput_setup): struct input_id (four u16),
// a fixed 80-byte name, and a u32 of ff_effects_max.
const (
	nameSize        = 80
	uinputSetupSize = 8 + nameSize + 4
)

// Config describes the virtual device.
//
// The identity fields are exposed because it is not documented whether
// libinput treats a synthetic switch identically to a kernel-driver one, and
// its model-keyed quirks imply device identity can matter. Being able to vary
// these without recompiling is what makes that testable.
type Config struct {
	Name    string
	BusType uint16
	Vendor  uint16
	Product uint16
	Version uint16
	WithLid bool // also declare SW_LID
	DevNode string
	Settle  time.Duration // how long to wait for udev after creation
}

// DefaultConfig returns a sensible virtual switch.
func DefaultConfig() Config {
	return Config{
		Name:    "hinged virtual tablet mode switch",
		BusType: BusVirtual,
		Vendor:  0,
		Product: 0,
		Version: 1,
		DevNode: "/dev/uinput",
		Settle:  200 * time.Millisecond,
	}
}

// Switch is a live virtual switch device.
type Switch struct {
	fd       int
	asserted bool
	closed   bool
	cfg      Config
}

// Create opens /dev/uinput and registers a device that emits SW_TABLET_MODE.
//
// The device is created in the released state and an explicit 0 is emitted
// immediately. That matters on restart: if a previous run died while
// asserting, downstream state may be latched, and publishing a known value is
// how a fresh process clears it rather than inheriting it.
func Create(cfg Config) (*Switch, error) {
	if cfg.DevNode == "" {
		cfg.DevNode = "/dev/uinput"
	}
	fd, err := syscall.Open(cfg.DevNode, syscall.O_WRONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		if err == syscall.EACCES || err == syscall.EPERM {
			return nil, fmt.Errorf(
				"%s is not writable: install packaging/udev/70-hinged-uinput.rules, "+
					"or run the system service which is granted the device explicitly: %w",
				cfg.DevNode, err)
		}
		if err == syscall.ENOENT {
			return nil, fmt.Errorf("%s does not exist; the uinput module may not be loaded "+
				"(try: sudo modprobe uinput): %w", cfg.DevNode, err)
		}
		return nil, &os.PathError{Op: "open", Path: cfg.DevNode, Err: err}
	}

	s := &Switch{fd: fd, cfg: cfg}
	if err := s.setup(); err != nil {
		syscall.Close(fd)
		return nil, err
	}

	// Publish a known state before anyone can observe an unknown one.
	if err := s.Set(false); err != nil {
		s.Close()
		return nil, fmt.Errorf("publishing initial released state: %w", err)
	}
	if cfg.Settle > 0 {
		time.Sleep(cfg.Settle)
	}
	return s, nil
}

func (s *Switch) setup() error {
	if err := ioctl(s.fd, uiSetEvBit, evSW); err != nil {
		return fmt.Errorf("UI_SET_EVBIT(EV_SW): %w", err)
	}
	if err := ioctl(s.fd, uiSetSwBit, swTabletMode); err != nil {
		return fmt.Errorf("UI_SET_SWBIT(SW_TABLET_MODE): %w", err)
	}
	if s.cfg.WithLid {
		if err := ioctl(s.fd, uiSetSwBit, swLid); err != nil {
			return fmt.Errorf("UI_SET_SWBIT(SW_LID): %w", err)
		}
	}

	// struct uinput_setup: input_id{bustype,vendor,product,version}, name[80],
	// ff_effects_max.
	var setup [uinputSetupSize]byte
	le := binary.LittleEndian
	if !isLittleEndian() {
		// The kernel reads these in native order.
		binary.BigEndian.PutUint16(setup[0:], s.cfg.BusType)
		binary.BigEndian.PutUint16(setup[2:], s.cfg.Vendor)
		binary.BigEndian.PutUint16(setup[4:], s.cfg.Product)
		binary.BigEndian.PutUint16(setup[6:], s.cfg.Version)
	} else {
		le.PutUint16(setup[0:], s.cfg.BusType)
		le.PutUint16(setup[2:], s.cfg.Vendor)
		le.PutUint16(setup[4:], s.cfg.Product)
		le.PutUint16(setup[6:], s.cfg.Version)
	}
	name := s.cfg.Name
	if len(name) > nameSize-1 {
		name = name[:nameSize-1]
	}
	copy(setup[8:8+nameSize], name)

	if err := ioctlPtr(s.fd, uiDevSetup, unsafe.Pointer(&setup[0])); err != nil {
		return fmt.Errorf("UI_DEV_SETUP: %w", err)
	}
	if err := ioctl(s.fd, uiDevCreate, 0); err != nil {
		return fmt.Errorf("UI_DEV_CREATE: %w", err)
	}
	return nil
}

// Set drives SW_TABLET_MODE and reports whether the value changed.
//
// Every write is followed by a SYN_REPORT: without it the kernel buffers the
// event and no consumer ever sees it.
func (s *Switch) Set(asserted bool) error {
	if s.closed {
		return fmt.Errorf("virtual switch is closed")
	}
	value := int32(0)
	if asserted {
		value = 1
	}
	if err := s.emit(evSW, swTabletMode, value); err != nil {
		return err
	}
	if err := s.emit(evSyn, synReport, 0); err != nil {
		return err
	}
	s.asserted = asserted
	return nil
}

// Asserted reports the last published value.
func (s *Switch) Asserted() bool { return s.asserted }

// Name returns the device name as registered.
func (s *Switch) Name() string { return s.cfg.Name }

func (s *Switch) emit(typ, code uint16, value int32) error {
	// struct input_event, native word size. The timestamp may be left zero;
	// the kernel fills it in.
	buf := make([]byte, inputEventSize)
	order := nativeOrder()
	order.PutUint16(buf[offType:], typ)
	order.PutUint16(buf[offCode:], code)
	order.PutUint32(buf[offValue:], uint32(value))

	for {
		_, err := syscall.Write(s.fd, buf)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return fmt.Errorf("writing event type=%d code=%d value=%d: %w", typ, code, value, err)
		}
		return nil
	}
}

// Close releases the switch, always publishing a released state first.
//
// This ordering is not cosmetic. libinput does not resume a suspended keyboard
// when a tablet-mode switch device simply disappears while asserting: the
// touchpad path calls tp_resume(), the keyboard path only detaches its
// listener. Destroying an asserting device therefore leaves the internal
// keyboard dead until the compositor rebuilds its libinput context. Emitting 0
// first is what makes an ordinary exit safe.
//
// It is idempotent and safe to call from a signal handler path.
func (s *Switch) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true

	var firstErr error
	if s.asserted {
		if err := s.releaseUnchecked(); err != nil {
			firstErr = err
		}
		// Give consumers a moment to process the release before the device
		// vanishes underneath them.
		time.Sleep(50 * time.Millisecond)
	}
	if err := ioctl(s.fd, uiDevDestroy, 0); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("UI_DEV_DESTROY: %w", err)
	}
	if err := syscall.Close(s.fd); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// releaseUnchecked publishes 0 without the closed guard, for use from Close.
func (s *Switch) releaseUnchecked() error {
	if err := s.emit(evSW, swTabletMode, 0); err != nil {
		return err
	}
	return s.emit(evSyn, synReport, 0)
}

func ioctl(fd int, req uintptr, arg uintptr) error {
	for {
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, arg)
		if errno == syscall.EINTR {
			continue
		}
		if errno != 0 {
			return errno
		}
		return nil
	}
}

func ioctlPtr(fd int, req uintptr, p unsafe.Pointer) error {
	for {
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(p))
		if errno == syscall.EINTR {
			continue
		}
		if errno != 0 {
			return errno
		}
		return nil
	}
}
