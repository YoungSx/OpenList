//go:build linux && (386 || amd64 || arm || arm64 || loong64 || riscv64)

package v4l2m2m

import (
	"encoding/binary"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/unix"
)

// <linux/videodev2.h> 中用到的部分。ioctl 号按 asm-generic 编码，
// 适用于上面 build tag 列出的架构；结构体只含定长整数，布局与架构无关。
const (
	vidiocQuerycap       = 0x80685600 // _IOR('V', 0, struct v4l2_capability)
	vidiocEnumFmt        = 0xc0405602 // _IOWR('V', 2, struct v4l2_fmtdesc)
	vidiocEnumFramesizes = 0xc02c564a // _IOWR('V', 74, struct v4l2_frmsizeenum)

	capVideoM2MMplane = 0x00004000
	capVideoM2M       = 0x00008000
	capDeviceCaps     = 0x80000000

	bufTypeVideoCapture       = 1
	bufTypeVideoOutput        = 2
	bufTypeVideoCaptureMplane = 9
	bufTypeVideoOutputMplane  = 10

	fmtFlagCompressed   = 0x0001
	frmsizeTypeDiscrete = 1
)

type v4l2Capability struct {
	Driver       [16]uint8
	Card         [32]uint8
	BusInfo      [32]uint8
	Version      uint32
	Capabilities uint32
	DeviceCaps   uint32
	Reserved     [3]uint32
}

type v4l2Fmtdesc struct {
	Index       uint32
	Type        uint32
	Flags       uint32
	Description [32]uint8
	PixelFormat uint32
	MbusCode    uint32
	Reserved    [3]uint32
}

type v4l2Frmsizeenum struct {
	Index       uint32
	PixelFormat uint32
	Type        uint32
	// 联合体：discrete 是 {宽, 高}；stepwise/continuous 是
	// {min_w, max_w, step_w, min_h, max_h, step_h}
	Size     [6]uint32
	Reserved [2]uint32
}

type platform struct{}

func (platform) nodes() []string {
	paths, _ := filepath.Glob("/dev/video*")
	return paths
}

func (platform) id(path string) (uint64, bool) {
	var st unix.Stat_t
	if unix.Stat(path, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFCHR {
		return 0, false
	}
	return st.Ino, true
}

// scan 读取一个 M2M 设备的压缩格式：解码设备在 OUTPUT 队列接收压缩流，
// 编码设备在 CAPTURE 队列产出压缩流。不是 M2M 的设备（摄像头等）返回空能力。
func (platform) scan(path string) (Caps, error) {
	caps := Caps{Decode: map[string]Size{}, Encode: map[string]Size{}}
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return caps, err
	}
	defer unix.Close(fd)

	var vc v4l2Capability
	if ioctl(fd, vidiocQuerycap, unsafe.Pointer(&vc)) != nil {
		return caps, nil
	}
	c := vc.Capabilities
	if c&capDeviceCaps != 0 {
		c = vc.DeviceCaps
	}
	var output, capture uint32
	switch {
	case c&capVideoM2MMplane != 0:
		output, capture = bufTypeVideoOutputMplane, bufTypeVideoCaptureMplane
	case c&capVideoM2M != 0:
		output, capture = bufTypeVideoOutput, bufTypeVideoCapture
	default:
		return caps, nil
	}
	for _, f := range compressedFormats(fd, output) {
		caps.Decode[fourcc(f)] = maxSize(fd, f)
	}
	// The FFmpeg path below explicitly supplies NV12. Skip encoders whose
	// raw OUTPUT queue cannot accept it, even if CAPTURE advertises H.264.
	if acceptsNV12(fd, output) {
		rawLimit := rawMaxSize(fd, output)
		for _, f := range compressedFormats(fd, capture) {
			limit := maxSize(fd, f)
			if limit.Width == 0 || limit.Height == 0 {
				limit = rawLimit
			}
			caps.Encode[fourcc(f)] = limit
		}
	}
	return caps, nil
}

func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), req, uintptr(arg)); errno != 0 {
		return errno
	}
	return nil
}

func compressedFormats(fd int, bufType uint32) []uint32 {
	var formats []uint32
	for i := uint32(0); ; i++ {
		d := v4l2Fmtdesc{Index: i, Type: bufType}
		if ioctl(fd, vidiocEnumFmt, unsafe.Pointer(&d)) != nil {
			return formats
		}
		if d.Flags&fmtFlagCompressed != 0 {
			formats = append(formats, d.PixelFormat)
		}
	}
}

// maxSize 读取格式的最大分辨率；驱动没实现 VIDIOC_ENUM_FRAMESIZES 时返回 0（不限制）
func maxSize(fd int, pixFmt uint32) Size {
	var s Size
	for i := uint32(0); ; i++ {
		e := v4l2Frmsizeenum{Index: i, PixelFormat: pixFmt}
		if ioctl(fd, vidiocEnumFramesizes, unsafe.Pointer(&e)) != nil {
			return s
		}
		if e.Type != frmsizeTypeDiscrete {
			return Size{Width: e.Size[1], Height: e.Size[4]}
		}
		candidate := Size{Width: e.Size[0], Height: e.Size[1]}
		if i == 0 || wider(candidate, s) {
			s = candidate
		}
	}
}

func fourcc(f uint32) string {
	return string([]byte{byte(f), byte(f >> 8), byte(f >> 16), byte(f >> 24)})
}

func acceptsNV12(fd int, bufType uint32) bool {
	for i := uint32(0); ; i++ {
		d := v4l2Fmtdesc{Index: i, Type: bufType}
		if ioctl(fd, vidiocEnumFmt, unsafe.Pointer(&d)) != nil {
			return false
		}
		if f := fourcc(d.PixelFormat); f == "NV12" || f == "NM12" {
			return true
		}
	}
}

// The v4l2_format union is pointer-aligned (208 bytes on 64-bit, 204 on 32-bit).
// pix and pix_mp share the first width/height/pixelformat words.
type v4l2Format struct {
	Type uint32
	_    [unsafe.Sizeof(uintptr(0)) - 4]byte
	Data [200]byte
}

func rawMaxSize(fd int, bufType uint32) Size {
	format := v4l2Format{Type: bufType}
	raw := "NV12"
	if bufType == bufTypeVideoOutputMplane {
		raw = "NM12"
	}
	binary.NativeEndian.PutUint32(format.Data[0:4], ^uint32(0))
	binary.NativeEndian.PutUint32(format.Data[4:8], ^uint32(0))
	copy(format.Data[8:12], raw)
	request := uintptr(0xc0005640) | unsafe.Sizeof(format)<<16 // VIDIOC_TRY_FMT
	if ioctl(fd, request, unsafe.Pointer(&format)) != nil {
		return Size{}
	}
	return Size{Width: binary.NativeEndian.Uint32(format.Data[0:4]), Height: binary.NativeEndian.Uint32(format.Data[4:8])}
}
