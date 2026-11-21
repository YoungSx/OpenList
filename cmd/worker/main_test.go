package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/transcode/v4l2m2m"
)

func workerArgs(t *testing.T, caps v4l2m2m.Caps, hw, codec, pixFmt string) []string {
	t.Helper()
	prev := detectV4L2M2M
	detectV4L2M2M = func() v4l2m2m.Caps { return caps }
	t.Cleanup(func() { detectV4L2M2M = prev })
	job := Job{
		SourceURL: "http://master:5244/d/movie.mkv",
		Probe:     Probe{VideoCodec: codec, PixFmt: pixFmt, Width: 1920, Height: 1080},
	}
	profile := Profile{Name: "1080p", VideoCodec: "h264", VideoBitrate: "4000k", AudioCodec: "aac", HWAccel: hw}
	args, err := buildFFmpegArgs(job, profile, "/tmp/seg-%d.ts", "/tmp/playlist.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	return args
}

func TestBuildFFmpegArgsV4L2M2M(t *testing.T) {
	caps := v4l2m2m.Caps{
		Decode: map[string]v4l2m2m.Size{"HEVC": {Width: 1920, Height: 1088}},
		Encode: map[string]v4l2m2m.Size{"H264": {Width: 1920, Height: 1088}},
	}
	args := workerArgs(t, caps, "v4l2m2m", "hevc", "yuv420p")
	in := slices.Index(args, "-i")
	if d := slices.Index(args, "hevc_v4l2m2m"); d < 0 || d > in {
		t.Errorf("hevc_v4l2m2m not selected as decoder: %v", args)
	}
	out := args[in:]
	if e := slices.Index(out, "h264_v4l2m2m"); e < 1 || out[e-1] != "-c:v" {
		t.Errorf("h264_v4l2m2m not selected as encoder: %v", args)
	}
	if p := slices.Index(out, "-pix_fmt"); p < 0 || out[p+1] != "nv12" {
		t.Errorf("encoder input not nv12: %v", args)
	}
	if !slices.Contains(args, "-sn") || !slices.Contains(args, "-dn") {
		t.Errorf("subtitle/data streams not dropped: %v", args)
	}
}

func TestBuildFFmpegArgsV4L2M2MFallback(t *testing.T) {
	args := workerArgs(t, v4l2m2m.Caps{}, "v4l2m2m", "hevc", "yuv420p")
	for _, a := range args {
		if strings.HasSuffix(a, "_v4l2m2m") {
			t.Fatalf("V4L2 codec %s chosen without a device: %v", a, args)
		}
	}
	if i := slices.Index(args, "libx264"); i < 1 || args[i-1] != "-c:v" {
		t.Errorf("no CPU encoder: %v", args)
	}
}

func TestV4L2WorkerCapacity(t *testing.T) {
	if got := workerCapacity("v4l2m2m", 4); got != 1 {
		t.Fatalf("hardware worker capacity %d", got)
	}
	if got := workerCapacity("none", 4); got != 4 {
		t.Fatalf("software worker capacity %d", got)
	}
}
