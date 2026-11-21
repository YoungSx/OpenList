package ffmpegpolicy

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/transcode/ffmpegargs"
	"github.com/OpenListTeam/OpenList/v4/internal/transcode/v4l2m2m"
)

func TestDownscaleBeforePublication(t *testing.T) {
	p := ffmpegargs.Profile{Name: "source", VideoCodec: "h264", VideoBitrate: "1000k", AudioBitrate: "96k", RatePolicy: "downscale"}
	probe := ffmpegargs.SourceProbe{Width: 1920, Height: 1080, Duration: 84}
	caps := v4l2m2m.Caps{Encode: map[string]v4l2m2m.Size{"H264": {Width: 1920, Height: 1080}}, RateLimits: true}
	var scales, directories []string
	run := func(_ context.Context, _ string, args []string, _ io.Writer) error {
		scale := ""
		for i, arg := range args {
			if arg == "-vf" {
				scale = args[i+1]
			}
		}
		scales = append(scales, scale)
		playlist := args[len(args)-1]
		directories = append(directories, filepath.Dir(playlist))
		if scale != "scale=640:-2" {
			return errors.New("rate budget exceeded")
		}
		if err := os.WriteFile(playlist, []byte("#EXTM3U\n#EXTINF:6,\nseg-0.ts\n#EXT-X-ENDLIST\n"), 0o600); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(filepath.Dir(playlist), "seg-0.ts"), make([]byte, 200000), 0o600)
	}
	selected, err := Prepare(context.Background(), "source.mp4", probe, p, "ffmpeg", caps, run)
	if err != nil || selected.Scale != "640:-2" || selected.Name != p.Name || len(scales) != 4 {
		t.Fatalf("selection: %+v %v %v", selected, err, scales)
	}
	for _, dir := range directories {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("probe output leaked: %s", dir)
		}
	}
}

func TestDownscaleDoesNotIgnoreMappingAtSmallerSizes(t *testing.T) {
	p := ffmpegargs.Profile{VideoCodec: "h264", VideoBitrate: "1000k", RatePolicy: "downscale"}
	probe := ffmpegargs.SourceProbe{Width: 3840, Height: 2160, Duration: 6}
	caps := v4l2m2m.Caps{Encode: map[string]v4l2m2m.Size{"H264": {Width: 1920, Height: 1080}}}
	calls := 0
	run := func(_ context.Context, _ string, args []string, _ io.Writer) error {
		calls++
		if strings.Contains(strings.Join(args, " "), "h264_v4l2m2m") {
			t.Fatal("hardware candidate ran without required mapping")
		}
		return errors.New("software encoding failed")
	}
	if _, err := Prepare(context.Background(), "source", probe, p, "ffmpeg", caps, run); err == nil || !strings.Contains(err.Error(), "mapping") || calls != 1 {
		t.Fatalf("candidate mapping: calls=%d error=%v", calls, err)
	}
}

func TestProbeRejectsTruncatedAndOverBudgetHLS(t *testing.T) {
	dir := t.TempDir()
	playlist := filepath.Join(dir, "playlist.m3u8")
	p := ffmpegargs.Profile{VideoBitrate: "1000k", AudioBitrate: "96k"}
	if err := os.WriteFile(filepath.Join(dir, "seg-0.ts"), make([]byte, 200000), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ body, want string }{
		{"#EXTINF:2,\nseg-0.ts\n", "complete"},
		{"#EXTINF:2,\nseg-0.ts\n#EXT-X-ENDLIST\n", "ended early"},
		{"#EXTINF:0.5,\nseg-0.ts\n#EXT-X-ENDLIST\n", "exceeds target"},
	} {
		if err := os.WriteFile(playlist, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		expected := 6.0
		if tc.want == "exceeds target" {
			expected = 0.5
		}
		if err := checkOutput(playlist, p, expected); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: %v", tc.want, err)
		}
	}
}

func TestRequiredMappingAndCancellation(t *testing.T) {
	p := ffmpegargs.Profile{VideoCodec: "h264", VideoBitrate: "1000k", RatePolicy: "fail"}
	probe := ffmpegargs.SourceProbe{Width: 640, Height: 360}
	caps := v4l2m2m.Caps{Encode: map[string]v4l2m2m.Size{"H264": {Width: 1920, Height: 1080}}}
	if _, err := Prepare(context.Background(), "source", probe, p, "ffmpeg", caps, nil); err == nil || !strings.Contains(err.Error(), "mapping") {
		t.Fatalf("missing mapping was ignored: %v", err)
	}
	p.RatePolicy = "downscale"
	caps.RateLimits = true
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Prepare(ctx, "source", probe, p, "ffmpeg", caps, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled probe ran: %v", err)
	}
}

func TestCandidatesShareOneProbeDeadline(t *testing.T) {
	p := ffmpegargs.Profile{VideoCodec: "h264", VideoBitrate: "1000k", RatePolicy: "downscale"}
	probe := ffmpegargs.SourceProbe{Width: 1920, Height: 1080, Duration: 10}
	var deadline time.Time
	calls := 0
	run := func(ctx context.Context, _ string, _ []string, _ io.Writer) error {
		value, ok := ctx.Deadline()
		if !ok {
			t.Fatal("probe has no wall-time limit")
		}
		if calls == 0 {
			deadline = value
		} else if !value.Equal(deadline) {
			t.Fatal("another candidate restarted the probe budget")
		}
		calls++
		return errors.New("unreachable rate")
	}
	if _, err := Prepare(context.Background(), "source", probe, p, "ffmpeg", v4l2m2m.Caps{}, run); err == nil || calls != 6 {
		t.Fatalf("candidate deadline: calls=%d error=%v", calls, err)
	}
}
