package ffmpegargs

import (
	"slices"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/transcode/v4l2m2m"
)

func value(args []string, name string) string {
	if i := slices.Index(args, name); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

func TestHLSCopyAudioUsesBrowserCompatibleEncoding(t *testing.T) {
	args := Build("https://media/movie.mkv", SourceProbe{AudioCodec: "dts"},
		Profile{VideoCodec: "h264", AudioCodec: "copy"}, 0, 0, 0, 0, "/tmp/seg-%d.ts", "/tmp/list.m3u8", v4l2m2m.Caps{})
	if value(args, "-c:a") != "aac" || value(args, "-ac") != "2" || value(args, "-ar") != "48000" {
		t.Fatalf("DTS would be copied into browser HLS: %v", args)
	}
	if value(args, "-hls_time") != "6" || value(args, "-hls_flags") != "independent_segments+temp_file" ||
		!slices.Contains(args, "-force_key_frames") || value(args, "-protocol_whitelist") != "http,https,tcp,tls" {
		t.Fatalf("HLS keyframe, publication or input safety settings lost: %v", args)
	}
}

func TestHardwareSeekUsesCorrectOutputTimeline(t *testing.T) {
	caps := v4l2m2m.Caps{Decode: map[string]v4l2m2m.Size{"H264": {Width: 1920, Height: 1080}}, Encode: map[string]v4l2m2m.Size{"H264": {Width: 1920, Height: 1080}}}
	args := Build("/media/movie.mp4", SourceProbe{VideoCodec: "h264", PixFmt: "yuv420p", Width: 1920, Height: 1080},
		Profile{VideoCodec: "h264", HWAccel: "v4l2m2m"}, 2, 36, 12, 18, "/tmp/seg-%d.ts", "/tmp/list.m3u8", caps)
	if !slices.Contains(args, "-copyts") || value(args, "-to") != "48.000" || slices.Contains(args, "-output_ts_offset") {
		t.Fatalf("hardware seek uses incorrect timeline: %v", args)
	}
	if value(args, "-start_number") != "18" || slices.Contains(args, "-profile:v") || slices.Contains(args, "-preset") {
		t.Fatalf("hardware output received software-only options: %v", args)
	}
}
