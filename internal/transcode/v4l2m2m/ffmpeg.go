package v4l2m2m

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type ffmpegCodecs struct {
	decode, encode map[string]bool
}

var ffmpegCache struct {
	sync.Mutex
	path   string
	info   os.FileInfo
	codecs ffmpegCodecs
}

// DetectForFFmpeg intersects device formats with the configured executable's wrappers.
func DetectForFFmpeg(executable string) Caps {
	caps := Detect()
	if caps.Empty() {
		return caps
	}
	codecs := executableCodecs(executable)
	usable := Caps{Decode: map[string]Size{}, Encode: map[string]Size{}}
	for _, decoder := range decoders {
		if limit, ok := caps.Decode[decoder.fourcc]; ok && codecs.decode[decoder.name] {
			usable.Decode[decoder.fourcc] = limit
		}
	}
	for fourcc, name := range map[string]string{"H264": "h264_v4l2m2m", "HEVC": "hevc_v4l2m2m"} {
		if limit, ok := caps.Encode[fourcc]; ok && codecs.encode[name] {
			usable.Encode[fourcc] = limit
		}
	}
	return usable
}

func executableCodecs(executable string) ffmpegCodecs {
	path, err := exec.LookPath(executable)
	if err != nil {
		return ffmpegCodecs{}
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return ffmpegCodecs{}
	}
	ffmpegCache.Lock()
	defer ffmpegCache.Unlock()
	if ffmpegCache.path == path && ffmpegCache.info != nil && os.SameFile(info, ffmpegCache.info) &&
		info.Size() == ffmpegCache.info.Size() && info.ModTime().Equal(ffmpegCache.info.ModTime()) {
		return ffmpegCache.codecs
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var output strings.Builder
	for _, option := range []string{"-decoders", "-encoders"} {
		out, err := exec.CommandContext(ctx, path, "-hide_banner", option).Output()
		if err != nil {
			// Retry on the next discovery; a transient failure is not a capability.
			return ffmpegCodecs{}
		}
		output.Write(out)
		output.WriteByte('\n')
	}
	codecs := parseFFmpegCodecs(output.String())
	ffmpegCache.path, ffmpegCache.info, ffmpegCache.codecs = path, info, codecs
	return codecs
}

func parseFFmpegCodecs(output string) ffmpegCodecs {
	codecs := ffmpegCodecs{decode: map[string]bool{}, encode: map[string]bool{}}
	var current map[string]bool
	for _, line := range strings.Split(output, "\n") {
		switch strings.TrimSpace(line) {
		case "Decoders:":
			current = codecs.decode
		case "Encoders:":
			current = codecs.encode
		default:
			fields := strings.Fields(line)
			if current != nil && len(fields) >= 2 && len(fields[0]) == 6 && fields[0][0] == 'V' &&
				strings.HasSuffix(fields[1], "_v4l2m2m") {
				current[fields[1]] = true
			}
		}
	}
	return codecs
}
