package ffmpegargs

import (
	"slices"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/transcode/v4l2m2m"
)

func TestRequiredRateLimits(t *testing.T) {
	p := Profile{VideoCodec: "h264", VideoBitrate: "1000k", RatePolicy: "fail"}
	peak, buffer, err := RateLimits(p)
	if err != nil || peak != 1000000 || buffer != 2000000 {
		t.Fatalf("defaults: %d %d %v", peak, buffer, err)
	}
	caps := v4l2m2m.Caps{}
	args, err := Build("file.mp4", SourceProbe{Width: 640, Height: 360}, p, 2, 0, 0, 0, "seg-%d.ts", "playlist.m3u8", caps)
	if err != nil || value(args, "-maxrate") != "1000000" || value(args, "-bufsize") != "2000000" {
		t.Fatalf("native software limits not passed: %v %v", args, err)
	}
	caps.Encode = map[string]v4l2m2m.Size{"H264": {Width: 1920, Height: 1080}}
	p.RatePolicy = "warn"
	args, err = Build("file.mp4", SourceProbe{Width: 640, Height: 360}, p, 2, 0, 0, 0, "seg-%d.ts", "playlist.m3u8", caps)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(args, "-maxrate") || slices.Contains(args, "-bufsize") {
		t.Fatalf("default behavior changed: %v", args)
	}
}

func TestRateLimitValidation(t *testing.T) {
	for _, p := range []Profile{
		{RatePolicy: "unknown"}, {RatePolicy: "fail", VideoBitrate: "0"},
		{RatePolicy: "fail", VideoBitrate: "1000k", PeakBitrate: "500k"},
		{RatePolicy: "fail", VideoBitrate: "1000k", BufferSize: "1"},
		{RatePolicy: "fail", VideoBitrate: "9223372036854775807M"},
	} {
		if _, _, err := RateLimits(p); err == nil {
			t.Fatalf("invalid limits accepted: %+v", p)
		}
		if args, err := Build("file.mp4", SourceProbe{}, p, 2, 0, 0, 0, "seg-%d.ts", "playlist.m3u8", v4l2m2m.Caps{}); err == nil || args != nil {
			t.Fatalf("invalid limits built an unconstrained command: %v %v", args, err)
		}
	}
}

func TestRequiredHardwareMapping(t *testing.T) {
	p := Profile{VideoCodec: "h264", VideoBitrate: "1000k", RatePolicy: "fail"}
	caps := v4l2m2m.Caps{Encode: map[string]v4l2m2m.Size{"H264": {Width: 1920, Height: 1080}}}
	if args, err := Build("file.mp4", SourceProbe{Width: 640, Height: 360}, p, 2, 0, 0, 0, "seg-%d.ts", "playlist.m3u8", caps); err == nil || args != nil {
		t.Fatalf("missing hardware mapping built a command: %v %v", args, err)
	}
	args, err := Build("file.mp4", SourceProbe{Width: 640, Height: 360}, p, 2, 0, 0, 0, "seg-%d.ts", "playlist.m3u8", v4l2m2m.Caps{})
	if err != nil || value(args, "-c:v") != "libx264" || value(args, "-maxrate") != "1000000" {
		t.Fatalf("software rate limits were rejected: %v %v", args, err)
	}
}

func TestV4L2MeasuredDownscaleUsesOfficialOptions(t *testing.T) {
	p := Profile{VideoCodec: "h264", VideoBitrate: "1000k", RatePolicy: "downscale"}
	caps := v4l2m2m.Caps{Encode: map[string]v4l2m2m.Size{"H264": {Width: 1920, Height: 1080}}}
	args, err := Build("file.mp4", SourceProbe{Width: 640, Height: 360}, p, 2, 0, 6, 0, "seg-%d.ts", "playlist.m3u8", caps)
	if err != nil || value(args, "-c:v") != "h264_v4l2m2m" || value(args, "-b:v") != "1000k" {
		t.Fatalf("official command: %v %v", args, err)
	}
	for _, option := range []string{"-maxrate", "-bufsize", "-v4l2_rate_limit"} {
		if slices.Contains(args, option) {
			t.Fatalf("unsupported option: %v", args)
		}
	}
	p.BufferSize = "2M"
	if _, err = Build("file.mp4", SourceProbe{Width: 640, Height: 360}, p, 2, 0, 6, 0, "seg-%d.ts", "playlist.m3u8", caps); err == nil {
		t.Fatal("explicit unsupported buffer accepted")
	}
}
