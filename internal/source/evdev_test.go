//go:build linux && (amd64 || arm64 || 386 || arm || riscv64 || s390x)

package source

import (
	"testing"
	"unsafe"
)

// struct input_event is two C longs of timestamp followed by u16 type, u16
// code and s32 value. Hardcoding 24 desynchronises the event stream on every
// 32-bit target, and the failure is silent: reads land mid-timestamp and
// manufacture plausible-looking switch events.
func TestInputEventSizeMatchesArchitecture(t *testing.T) {
	want := 2*int(unsafe.Sizeof(uintptr(0))) + 8
	if inputEventSize != want {
		t.Errorf("inputEventSize = %d, want %d on this architecture", inputEventSize, want)
	}
	if wordBytes != int(unsafe.Sizeof(uintptr(0))) {
		t.Errorf("wordBytes = %d, want %d", wordBytes, unsafe.Sizeof(uintptr(0)))
	}
	if offType != 2*wordBytes || offCode != offType+2 || offValue != offCode+2 {
		t.Errorf("field offsets are inconsistent: type=%d code=%d value=%d", offType, offCode, offValue)
	}
}

// The ioctl size field must match the buffer actually passed, or the kernel
// writes a different number of bytes than we reserved.
func TestEviocgswEncodesBufferSize(t *testing.T) {
	size := (eviocgsw >> 16) & 0x3fff
	if size != wordBytes {
		t.Errorf("EVIOCGSW encodes size %d but the buffer is %d bytes", size, wordBytes)
	}
	if dir := uint32(eviocgsw) >> 30; dir != 2 {
		t.Errorf("direction bits = %d, want 2 (_IOC_READ)", dir)
	}
	if typ := (eviocgsw >> 8) & 0xff; typ != 0x45 {
		t.Errorf("ioctl type = %#x, want 'E'", typ)
	}
}

func TestSwitchCodesMatchKernel(t *testing.T) {
	if SwLid != 0x00 || SwTabletMode != 0x01 {
		t.Errorf("SW_LID=%d SW_TABLET_MODE=%d disagree with input-event-codes.h", SwLid, SwTabletMode)
	}
}
