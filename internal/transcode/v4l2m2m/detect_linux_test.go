//go:build linux && (386 || amd64 || arm || arm64 || loong64 || riscv64)

package v4l2m2m

import (
	"testing"
	"unsafe"
)

func TestV4L2IoctlABI(t *testing.T) {
	if unsafe.Sizeof(v4l2Capability{}) != 104 || unsafe.Sizeof(v4l2Fmtdesc{}) != 64 || unsafe.Sizeof(v4l2Frmsizeenum{}) != 44 {
		t.Fatal("V4L2 fixed-size ABI changed")
	}
	expectedSize, expectedOffset := uintptr(204), uintptr(4)
	if unsafe.Sizeof(uintptr(0)) == 8 {
		expectedSize, expectedOffset = 208, 8
	}
	var format v4l2Format
	if unsafe.Sizeof(format) != expectedSize || unsafe.Offsetof(format.Data) != expectedOffset {
		t.Fatalf("format size=%d data offset=%d", unsafe.Sizeof(format), unsafe.Offsetof(format.Data))
	}
}
