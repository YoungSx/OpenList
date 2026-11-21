package transcode

import (
	"reflect"
	"slices"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/transcode/v4l2m2m"
)

// 一台 Amlogic S805：H.264/HEVC 硬解到 1080p，只能硬编 H.264
var s805 = v4l2m2m.Caps{
	Decode: map[string]v4l2m2m.Size{"H264": {Width: 1920, Height: 1080}, "HEVC": {Width: 1920, Height: 1088}},
	Encode: map[string]v4l2m2m.Size{"H264": {Width: 1920, Height: 1088}, "JPEG": {Width: 1920, Height: 1088}},
}

func withV4L2M2M(t *testing.T, caps v4l2m2m.Caps) {
	t.Helper()
	prev := detectV4L2M2M
	detectV4L2M2M = func(string) v4l2m2m.Caps { return caps }
	t.Cleanup(func() { detectV4L2M2M = prev })
}

func chunkArgs(hw, codec, pixFmt string, height int, startTime float64) []string {
	job := &Job{
		SourceURL: "http://127.0.0.1:5244/d/movie.mkv",
		Probe:     SourceProbe{VideoCodec: codec, PixFmt: pixFmt, Width: height * 16 / 9, Height: height},
	}
	profile := Profile{Name: "1080p", VideoCodec: "h264", VideoBitrate: "4000k", Scale: "1920:-2",
		AudioCodec: "aac", AudioBitrate: "160k", HWAccel: hw}
	return buildFFmpegChunkArgs(job, profile, 6, startTime, 60, int(startTime)/6, "/tmp/seg-%d.ts", "/tmp/chunk.m3u8", "ffmpeg")
}

// option 返回 name 后面的值，没有该选项时返回空串
func option(args []string, name string) string {
	if i := slices.Index(args, name); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

// inputOption 返回 -i 之前 name 的值（解码器等输入选项）
func inputOption(args []string, name string) string {
	return option(args[:slices.Index(args, "-i")], name)
}

func TestV4L2M2MChunkArgs(t *testing.T) {
	withV4L2M2M(t, s805)
	args := chunkArgs("v4l2m2m", "hevc", "yuv420p", 1080, 120)

	if got := inputOption(args, "-c:v"); got != "hevc_v4l2m2m" {
		t.Errorf("decoder = %q, want hevc_v4l2m2m: %v", got, args)
	}
	out := args[slices.Index(args, "-i"):]
	if got := option(out, "-c:v"); got != "h264_v4l2m2m" {
		t.Errorf("encoder = %q, want h264_v4l2m2m: %v", got, args)
	}
	if got := option(out, "-pix_fmt"); got != "nv12" {
		t.Errorf("-pix_fmt = %q, want nv12", got)
	}
	// FFmpeg 的 V4L2 M2M 编码器不认这些选项，带上会直接报错退出
	for _, name := range []string{"-profile:v", "-level:v", "-preset"} {
		if slices.Contains(args, name) {
			t.Errorf("%s passed to h264_v4l2m2m: %v", name, args)
		}
	}
	// Keep decoder timestamps nonnegative, rebase CFR, then restore the seek offset.
	if !slices.Contains(args, "-copyts") || option(args, "-vf") != "scale=1920:-2,setpts=PTS-STARTPTS" {
		t.Errorf("hardware timestamps were not rebased before CFR: %v", args)
	}
	if option(args, "-output_ts_offset") != "120.000" {
		t.Errorf("hardware chunk lost its absolute position: %v", args)
	}
	if slices.Contains(args, "-to") || option(args, "-t") != "60.000" {
		t.Errorf("rebased chunk lacks its bounded duration: %v", args)
	}
	if !slices.Contains(args, "-sn") || !slices.Contains(args, "-dn") {
		t.Errorf("subtitle/data streams not dropped: %v", args)
	}
}

func TestV4L2M2MFirstChunkKeepsTimestamps(t *testing.T) {
	withV4L2M2M(t, s805)
	args := chunkArgs("v4l2m2m", "h264", "yuv420p", 1080, 0)
	if inputOption(args, "-c:v") != "h264_v4l2m2m" {
		t.Fatalf("no hardware decoder: %v", args)
	}
	if slices.Contains(args, "-copyts") || slices.Contains(args, "-ss") {
		t.Errorf("first chunk does not seek: %v", args)
	}
}

func TestV4L2M2MSoftwareDecodeFallback(t *testing.T) {
	withV4L2M2M(t, s805)
	for _, c := range []struct {
		name, codec, pixFmt string
		height              int
	}{
		{"10-bit", "hevc", "yuv420p10le", 1080},
		{"4K", "hevc", "yuv420p", 2160},
		{"codec the device lacks", "av1", "yuv420p", 1080},
	} {
		args := chunkArgs("v4l2m2m", c.codec, c.pixFmt, c.height, 120)
		if dec := inputOption(args, "-c:v"); dec != "" {
			t.Errorf("%s: decoder %q, want software decode", c.name, dec)
		}
		// 软解的 chunk 走原来的时间戳处理
		if slices.Contains(args, "-copyts") || option(args, "-t") != "60.000" ||
			option(args, "-output_ts_offset") != "120.000" {
			t.Errorf("%s: timestamps not handled like software decode: %v", c.name, args)
		}
		if option(args[slices.Index(args, "-i"):], "-c:v") != "h264_v4l2m2m" {
			t.Errorf("%s: hardware encoder not used: %v", c.name, args)
		}
	}
}

func TestV4L2M2MWithoutDeviceMatchesCPU(t *testing.T) {
	withV4L2M2M(t, v4l2m2m.Caps{})
	got := chunkArgs("v4l2m2m", "hevc", "yuv420p", 1080, 120)
	want := chunkArgs("none", "hevc", "yuv420p", 1080, 120)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("no device:\n got %v\nwant %v", got, want)
	}
}

func TestOtherHWAccelSkipsV4L2Detection(t *testing.T) {
	detected := false
	prev := detectV4L2M2M
	detectV4L2M2M = func(string) v4l2m2m.Caps { detected = true; return s805 }
	t.Cleanup(func() { detectV4L2M2M = prev })

	args := chunkArgs("none", "h264", "yuv420p", 1080, 120)
	if detected {
		t.Error("devices probed for hwaccel=none")
	}
	if option(args, "-c:v") != "libx264" || option(args, "-profile:v") != "high" {
		t.Errorf("CPU args changed: %v", args)
	}
}
