package v4l2m2m

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestFFmpegListsKeepDecodeAndEncodeSeparate(t *testing.T) {
	codecs := parseFFmpegCodecs(`Decoders:
 V..... h264_v4l2m2m V4L2 H.264 decoder
 V..... hevc_v4l2m2m V4L2 HEVC decoder
Encoders:
 V..... h264_v4l2m2m V4L2 H.264 encoder
 A..... hevc_v4l2m2m not a video wrapper
`)
	if !codecs.decode["hevc_v4l2m2m"] || codecs.encode["hevc_v4l2m2m"] || !codecs.encode["h264_v4l2m2m"] {
		t.Fatalf("incorrect decoder/encoder lists: %+v", codecs)
	}
}

func TestDetectForFFmpegRefreshesChangedExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake executable uses a shell script")
	}
	h := newFakeHost()
	h.add("/dev/video0", 1, s805)
	previous := system
	system = &registry{host: h, devices: map[string]device{}}
	t.Cleanup(func() { system = previous })
	dir := t.TempDir()
	path, calls := filepath.Join(dir, "ffmpeg"), filepath.Join(dir, "calls")
	script := "#!/bin/sh\necho called >> '" + calls + "'\ncase \"$2\" in\n" +
		"-decoders) printf 'Decoders:\\n V..... hevc_v4l2m2m decoder\\n';;\n" +
		"-encoders) printf 'Encoders:\\n V..... h264_v4l2m2m encoder\\n';;\nesac\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		caps := DetectForFFmpeg(path)
		if caps.Decoder("hevc", "yuv420p", 1920, 1080) != "hevc_v4l2m2m" ||
			caps.Decoder("h264", "yuv420p", 1920, 1080) != "" ||
			caps.Encoder("h264", 1920, 1080, "") != "h264_v4l2m2m" {
			t.Fatalf("device and executable capabilities not intersected: %s", caps)
		}
	}
	if contents, err := os.ReadFile(calls); err != nil || string(contents) != "called\ncalled\n" {
		t.Fatalf("FFmpeg lists were not cached: %q (%v)", contents, err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf 'Decoders:\\nEncoders:\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(time.Second)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if caps := DetectForFFmpeg(path); !caps.Empty() {
		t.Fatalf("changed FFmpeg retained old wrappers: %s", caps)
	}
	if caps := DetectForFFmpeg(filepath.Join(dir, "missing")); !caps.Empty() {
		t.Fatalf("missing FFmpeg advertised codecs: %s", caps)
	}
}
