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
	args, err := Build("https://media/movie.mkv", SourceProbe{AudioCodec: "dts"},
		Profile{VideoCodec: "h264", AudioCodec: "copy"}, 0, 0, 0, 0, "/tmp/seg-%d.ts", "/tmp/list.m3u8", v4l2m2m.Caps{})
	if err != nil {
		t.Fatal(err)
	}
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
	args, err := Build("/media/movie.mp4", SourceProbe{VideoCodec: "h264", AudioCodec: "aac", PixFmt: "yuv420p", Width: 1920, Height: 1080},
		Profile{VideoCodec: "h264", HWAccel: "v4l2m2m"}, 2, 36, 12, 18, "/tmp/seg-%d.ts", "/tmp/list.m3u8", caps)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(args, "-copyts") || value(args, "-t") != "12.000" || value(args, "-output_ts_offset") != "36.000" ||
		value(args, "-vf") != "setpts=PTS-STARTPTS" || value(args, "-af") != "asetpts=PTS-STARTPTS" ||
		slices.Contains(args, "-start_at_zero") || slices.Contains(args, "-to") {
		t.Fatalf("hardware seek uses incorrect timeline: %v", args)
	}
	if value(args, "-start_number") != "18" || slices.Contains(args, "-profile:v") || slices.Contains(args, "-preset") || slices.Contains(args, "-sc_threshold") {
		t.Fatalf("hardware output received software-only options: %v", args)
	}
	if slices.Contains(args, "-maxrate") || slices.Contains(args, "-bufsize") {
		t.Fatalf("unsupported V4L2 peak constraints were passed: %v", args)
	}
}

func TestFrameRateAndGOPShareMediaClock(t *testing.T) {
	for _, tc := range []struct {
		source, output, gop string
	}{
		{"30/1", "30/1", "60"},
		{"30000/1001", "30000/1001", "60"},
		{"24000/1001", "24000/1001", "48"},
		{"25/1", "25/1", "50"},
		{"0/0", "30/1", "60"},
		{"-30/1", "30/1", "60"},
	} {
		args, err := Build("/media/movie.mp4", SourceProbe{FrameRate: tc.source},
			Profile{VideoCodec: "h264", VideoBitrate: "4000k"}, 2, 0, 0, 0,
			"/tmp/seg-%d.ts", "/tmp/list.m3u8", v4l2m2m.Caps{})
		if err != nil {
			t.Fatal(err)
		}
		if value(args, "-r") != tc.output || value(args, "-fps_mode") != "cfr" || value(args, "-g") != tc.gop {
			t.Fatalf("rate %s: %v", tc.source, args)
		}
		if value(args, "-maxrate") != "4000k" || value(args, "-bufsize") != "4000k" || value(args, "-sc_threshold") != "0" {
			t.Fatalf("software bitrate constraints lost: %v", args)
		}
	}
}
