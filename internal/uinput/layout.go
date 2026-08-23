//go:build linux && (amd64 || arm64 || 386 || arm || riscv64 || s390x)

package uinput

import (
	"encoding/binary"
	"unsafe"
)

// wordBytes is the size of a C long: 8 on 64-bit, 4 on 32-bit.
const wordBytes = (32 << (^uint(0) >> 63)) / 8

// inputEventSize is sizeof(struct input_event) for this architecture: two
// longs of timestamp, then u16 type, u16 code, s32 value. Writing the wrong
// size makes the kernel reject or misparse every event.
const inputEventSize = 2*wordBytes + 8

const (
	offType  = 2 * wordBytes
	offCode  = offType + 2
	offValue = offCode + 2
)

// nativeOrder returns the byte order the kernel uses for its structs, which is
// always the machine's own rather than a fixed one.
func nativeOrder() binary.ByteOrder {
	if isLittleEndian() {
		return binary.LittleEndian
	}
	return binary.BigEndian
}

func isLittleEndian() bool {
	var probe uint16 = 0x0102
	return *(*byte)(unsafe.Pointer(&probe)) == 0x02
}
