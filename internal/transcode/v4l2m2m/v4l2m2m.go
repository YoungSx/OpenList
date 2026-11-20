// Package v4l2m2m 探测 Linux V4L2 M2M（memory-to-memory）编解码设备，
// 为 FFmpeg 选择 *_v4l2m2m 编解码器。
//
// 树莓派、Amlogic、高通等 ARM SoC 的硬件编解码走这套内核标准接口。
// FFmpeg 不把它算作 -hwaccel，-hwaccel auto 也不会用到它，必须用 -c:v
// 点名 h264_v4l2m2m 这类编解码器。设备没有声明的格式不能点名，否则 FFmpeg
// 找不到设备直接失败，所以这里按设备实际声明的格式和尺寸来选。
package v4l2m2m

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Size 设备为某个压缩格式声明的最大分辨率，0 表示未声明（不限制）
type Size struct {
	Width, Height uint32
}

// Caps 本机 V4L2 M2M 设备支持的压缩格式，键为 V4L2 FourCC，例如 "H264"
type Caps struct {
	Decode map[string]Size // 解码设备 OUTPUT 队列接受的格式
	Encode map[string]Size // 编码设备 CAPTURE 队列产出的格式
}

// decoders ffprobe 的 codec_name → V4L2 FourCC 与 FFmpeg 解码器
var decoders = map[string]struct{ fourcc, name string }{
	"h264":       {"H264", "h264_v4l2m2m"},
	"hevc":       {"HEVC", "hevc_v4l2m2m"},
	"mpeg1video": {"MPG1", "mpeg1_v4l2m2m"},
	"mpeg2video": {"MPG2", "mpeg2_v4l2m2m"},
	"mpeg4":      {"MPG4", "mpeg4_v4l2m2m"},
	"vc1":        {"VC1G", "vc1_v4l2m2m"},
	"vp8":        {"VP80", "vp8_v4l2m2m"},
	"vp9":        {"VP90", "vp9_v4l2m2m"},
}

// Decoder 返回能硬解该源的 FFmpeg 解码器，不能硬解时返回空串，调用方走软解。
// 只接受 8-bit 4:2:0：FFmpeg 的 V4L2 M2M 解码器输出 NV12，10-bit 源交给软解。
// 探测失败（编码或像素格式未知）时同样软解。
func (c Caps) Decoder(codec, pixFmt string, width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	switch pixFmt {
	case "yuv420p", "yuvj420p", "nv12":
	default:
		return ""
	}
	d, ok := decoders[strings.ToLower(codec)]
	if !ok {
		return ""
	}
	limit, ok := c.Decode[d.fourcc]
	if !ok {
		return ""
	}
	if (limit.Width > 0 && width > int(limit.Width)) || (limit.Height > 0 && height > int(limit.Height)) {
		return ""
	}
	return d.name
}

// Encoder selects a codec only when the NV12 output fits the declared limits.
// Unknown scale expressions fall back to software rather than guessing geometry.
func (c Caps) Encoder(codec string, width, height int, scale string) string {
	var fourcc, name string
	switch strings.ToLower(codec) {
	case "h264", "":
		fourcc, name = "H264", "h264_v4l2m2m"
	case "hevc", "h265":
		fourcc, name = "HEVC", "hevc_v4l2m2m"
	default:
		return ""
	}
	limit, ok := c.Encode[fourcc]
	if !ok {
		return ""
	}
	if scale != "" {
		fields := strings.Split(scale, ":")
		if len(fields) != 2 {
			return ""
		}
		w, errW := strconv.Atoi(fields[0])
		h, errH := strconv.Atoi(fields[1])
		if errW != nil || errH != nil {
			return ""
		}
		switch {
		case w > 0 && h > 0:
			width, height = w, h
		case w > 0 && (h == -1 || h == -2) && width > 0 && height > 0:
			height = scaledDimension(height, width, w, -h)
			width = w
		case h > 0 && (w == -1 || w == -2) && width > 0 && height > 0:
			width = scaledDimension(width, height, h, -w)
			height = h
		default:
			return ""
		}
	}
	if width <= 0 || height <= 0 || (limit.Width > 0 && width > int(limit.Width)) || (limit.Height > 0 && height > int(limit.Height)) {
		return ""
	}
	return name
}

func scaledDimension(value, original, target, alignment int) int {
	// Round up conservatively; FFmpeg's scale filter may round the last pixel down.
	n := (int64(value)*int64(target) + int64(original*alignment) - 1) / int64(original*alignment)
	return int(n) * alignment
}

// Empty 是否没有任何可用设备
func (c Caps) Empty() bool {
	return len(c.Decode) == 0 && len(c.Encode) == 0
}

func (c Caps) String() string {
	var dec, enc []string
	for f, s := range c.Decode {
		if s.Width > 0 && s.Height > 0 {
			f = fmt.Sprintf("%s(%dx%d)", f, s.Width, s.Height)
		}
		dec = append(dec, f)
	}
	for f := range c.Encode {
		enc = append(enc, f)
	}
	sort.Strings(dec)
	sort.Strings(enc)
	return fmt.Sprintf("decode=%v encode=%v", dec, enc)
}

// add 并入一个设备的能力；多个设备支持同一格式时取更宽松的尺寸上限
func (c *Caps) add(d Caps) {
	for f, s := range d.Decode {
		if prev, ok := c.Decode[f]; !ok || wider(s, prev) {
			c.Decode[f] = s
		}
	}
	for f, s := range d.Encode {
		if prev, ok := c.Encode[f]; !ok || wider(s, prev) {
			c.Encode[f] = s
		}
	}
}

// wider only replaces limits when both dimensions are at least as permissive.
func wider(a, b Size) bool {
	switch {
	case b.Width == 0 || b.Height == 0:
		return false
	case a.Width == 0 || a.Height == 0:
		return true
	}
	return a.Width >= b.Width && a.Height >= b.Height
}

// host 平台相关的设备访问，测试时替换
type host interface {
	nodes() []string                // 候选设备节点
	id(path string) (uint64, bool)  // 节点的 inode；不是字符设备时返回 false
	scan(path string) (Caps, error) // 打开设备读取能力；打不开时返回错误
}

// device 已探测过的设备节点
type device struct {
	id   uint64
	caps Caps
}

// registry 记住每个设备节点的能力，节点首次出现或 inode 变化时才打开它。
// 有的驱动同一时刻只允许打开一次（例如 Amlogic Meson8b 的编码器），
// 转码进行中再去探测会失败，还可能让正要启动的 FFmpeg 抢不到设备。
type registry struct {
	host    host
	mu      sync.Mutex
	devices map[string]device
}

var system = &registry{host: platform{}, devices: map[string]device{}}

// Detect 返回本机 V4L2 M2M 设备能力。每次调用都会发现新增和移除的节点，
// 驱动可以晚于 OpenList 加载；已知节点不再重复打开。
func Detect() Caps {
	return system.detect()
}

func (r *registry) detect() Caps {
	r.mu.Lock()
	defer r.mu.Unlock()

	caps := Caps{Decode: map[string]Size{}, Encode: map[string]Size{}}
	present := map[string]bool{}
	for _, path := range r.host.nodes() {
		id, ok := r.host.id(path)
		if !ok {
			continue
		}
		present[path] = true
		d, known := r.devices[path]
		if !known || d.id != id {
			c, err := r.host.scan(path)
			if err != nil {
				// 设备忙或没有权限，下次再试
				delete(r.devices, path)
				continue
			}
			d = device{id: id, caps: c}
			r.devices[path] = d
		}
		caps.add(d.caps)
	}
	for path := range r.devices {
		if !present[path] {
			delete(r.devices, path)
		}
	}
	return caps
}
