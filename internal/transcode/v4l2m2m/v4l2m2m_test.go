package v4l2m2m

import (
	"errors"
	"sort"
	"testing"
)

// 一台 Amlogic S805：H.264/HEVC/MPEG-2 硬解到 1080p，只能硬编 H.264
var s805 = Caps{
	Decode: map[string]Size{
		"H264": {1920, 1080},
		"HEVC": {1920, 1088},
		"MPG2": {1920, 1080},
	},
	Encode: map[string]Size{"H264": {1920, 1088}, "JPEG": {1920, 1088}},
}

func TestDecoder(t *testing.T) {
	cases := []struct {
		name          string
		caps          Caps
		codec, pixFmt string
		w, h          int
		want          string
	}{
		{"hevc 1080p", s805, "hevc", "yuv420p", 1920, 1080, "hevc_v4l2m2m"},
		{"h264 full range", s805, "h264", "yuvj420p", 1280, 720, "h264_v4l2m2m"},
		{"mpeg2", s805, "mpeg2video", "yuv420p", 720, 576, "mpeg2_v4l2m2m"},
		{"10-bit goes to software", s805, "hevc", "yuv420p10le", 1920, 1080, ""},
		{"4:2:2 goes to software", s805, "h264", "yuv422p", 1920, 1080, ""},
		{"above the device limit", s805, "hevc", "yuv420p", 3840, 2160, ""},
		{"format the device lacks", s805, "vp9", "yuv420p", 1920, 1080, ""},
		{"unknown probe", s805, "", "", 0, 0, ""},
		{"no device", Caps{}, "h264", "yuv420p", 1920, 1080, ""},
		{"no declared limit", Caps{Decode: map[string]Size{"H264": {}}}, "h264", "yuv420p", 3840, 2160, "h264_v4l2m2m"},
	}
	for _, c := range cases {
		if got := c.caps.Decoder(c.codec, c.pixFmt, c.w, c.h); got != c.want {
			t.Errorf("%s: Decoder(%q, %q, %d, %d) = %q, want %q", c.name, c.codec, c.pixFmt, c.w, c.h, got, c.want)
		}
	}
}

func TestEncoder(t *testing.T) {
	for codec, want := range map[string]string{
		"h264": "h264_v4l2m2m",
		"":     "h264_v4l2m2m",
		"hevc": "", // S805 编不了 HEVC，调用方回退软件编码
		"av1":  "",
	} {
		if got := s805.Encoder(codec, 1920, 1080, ""); got != want {
			t.Errorf("Encoder(%q) = %q, want %q", codec, got, want)
		}
	}
	if got := (Caps{Encode: map[string]Size{"HEVC": {1920, 1088}}}).Encoder("h265", 1920, 1080, ""); got != "hevc_v4l2m2m" {
		t.Errorf("Encoder(h265) = %q, want hevc_v4l2m2m", got)
	}
}

func TestString(t *testing.T) {
	want := "decode=[H264(1920x1080) HEVC(1920x1088) MPG2(1920x1080)] encode=[H264 JPEG]"
	if got := s805.String(); got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

// fakeHost 模拟设备节点，记录每个节点被打开的次数
type fakeHost struct {
	ids   map[string]uint64
	caps  map[string]Caps
	busy  map[string]bool
	scans map[string]int
}

func newFakeHost() *fakeHost {
	return &fakeHost{ids: map[string]uint64{}, caps: map[string]Caps{}, busy: map[string]bool{}, scans: map[string]int{}}
}

func (h *fakeHost) nodes() []string {
	var paths []string
	for p := range h.ids {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

func (h *fakeHost) id(path string) (uint64, bool) {
	id, ok := h.ids[path]
	return id, ok
}

func (h *fakeHost) scan(path string) (Caps, error) {
	h.scans[path]++
	if h.busy[path] {
		return Caps{}, errors.New("device busy")
	}
	return h.caps[path], nil
}

func (h *fakeHost) add(path string, id uint64, c Caps) {
	h.ids[path], h.caps[path] = id, c
}

func TestDetectMergesDevices(t *testing.T) {
	h := newFakeHost()
	h.add("/dev/video0", 1, Caps{Decode: map[string]Size{"H264": {1920, 1080}, "HEVC": {1920, 1088}}})
	h.add("/dev/video1", 2, Caps{Encode: map[string]Size{"H264": {1920, 1088}}})
	h.add("/dev/video2", 3, Caps{Decode: map[string]Size{"H264": {4096, 2304}}})
	h.add("/dev/video3", 4, Caps{}) // 摄像头
	got := (&registry{host: h, devices: map[string]device{}}).detect()
	want := "decode=[H264(4096x2304) HEVC(1920x1088)] encode=[H264]"
	if got.String() != want {
		t.Errorf("detect() = %s, want %s", got, want)
	}
}

func TestDetectOpensKnownDevicesOnce(t *testing.T) {
	h := newFakeHost()
	h.add("/dev/video0", 1, s805)
	h.add("/dev/video3", 4, Caps{})
	r := &registry{host: h, devices: map[string]device{}}
	for i := 0; i < 3; i++ {
		if got := r.detect(); got.Encoder("h264", 1920, 1080, "") == "" {
			t.Fatalf("detect() #%d lost the encoder: %s", i, got)
		}
	}
	for path, n := range h.scans {
		if n != 1 {
			t.Errorf("%s opened %d times, want 1", path, n)
		}
	}
}

func TestDetectRetriesBusyDevice(t *testing.T) {
	h := newFakeHost()
	h.add("/dev/video1", 2, Caps{Encode: map[string]Size{"H264": {1920, 1088}}})
	h.busy["/dev/video1"] = true
	r := &registry{host: h, devices: map[string]device{}}
	if got := r.detect(); !got.Empty() {
		t.Fatalf("busy device reported: %s", got)
	}
	h.busy["/dev/video1"] = false
	if got := r.detect(); got.Encoder("h264", 1920, 1080, "") != "h264_v4l2m2m" {
		t.Fatalf("device not picked up after it became free: %s", got)
	}
}

func TestDetectFollowsDeviceChanges(t *testing.T) {
	h := newFakeHost()
	h.add("/dev/video0", 1, s805)
	r := &registry{host: h, devices: map[string]device{}}
	r.detect()

	// 驱动重新加载后同一节点换成了别的设备
	h.add("/dev/video0", 7, Caps{Encode: map[string]Size{"HEVC": {1920, 1088}}})
	if got := r.detect(); got.Encoder("hevc", 1920, 1080, "") != "hevc_v4l2m2m" || got.Encoder("h264", 1920, 1080, "") != "" {
		t.Errorf("replaced device not rescanned: %s", got)
	}
	if h.scans["/dev/video0"] != 2 {
		t.Errorf("/dev/video0 opened %d times, want 2", h.scans["/dev/video0"])
	}

	delete(h.ids, "/dev/video0")
	if got := r.detect(); !got.Empty() {
		t.Errorf("removed device still reported: %s", got)
	}
}

func TestEncoderGeometry(t *testing.T) {
	for _, tc := range []struct {
		name, scale   string
		width, height int
		want          bool
	}{
		{"source 1080p", "", 1920, 1080, true},
		{"source 4k", "", 3840, 2160, false},
		{"downscale", "1920:-2", 3840, 2160, true},
		{"portrait exceeds height", "1920:-2", 1080, 1920, false},
		{"explicit size", "1280:720", 3840, 2160, true},
		{"height scale", "-2:720", 3840, 2160, true},
		{"unknown dimensions", "", 0, 0, false},
		{"unknown expression", "min(1920,iw):-2", 3840, 2160, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := s805.Encoder("h264", tc.width, tc.height, tc.scale)
			if (got != "") != tc.want {
				t.Fatalf("Encoder=%q, hardware wanted=%v", got, tc.want)
			}
		})
	}
}

func TestMergeKeepsRealGeometry(t *testing.T) {
	caps := Caps{Decode: map[string]Size{}, Encode: map[string]Size{}}
	caps.add(Caps{Decode: map[string]Size{"H264": {1920, 1080}}})
	caps.add(Caps{Decode: map[string]Size{"H264": {1080, 1920}}})
	if got := caps.Decoder("h264", "yuv420p", 1920, 1920); got != "" {
		t.Fatalf("combined unsupported geometry: %s", got)
	}
	if got := caps.Decoder("h264", "yuv420p", 0, 1080); got != "" {
		t.Fatalf("unknown width should use software: %s", got)
	}
}
