// Package ffmpegpolicy selects a stable output profile before publishing HLS.
package ffmpegpolicy

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/transcode/ffmpegargs"
	"github.com/OpenListTeam/OpenList/v4/internal/transcode/v4l2m2m"
)

type Run func(context.Context, string, []string, io.Writer) error

const ProbeTimeout = 90 * time.Second

type diagnostic struct{ data []byte }

func (b *diagnostic) Write(p []byte) (int, error) {
	n := len(p)
	remaining := 32768 - len(b.data)
	if len(p) > remaining {
		p = p[:remaining]
	}
	b.data = append(b.data, p...)
	return n, nil
}

// Prepare validates required limits; downscale probes isolated output before use.
func Prepare(ctx context.Context, source string, probe ffmpegargs.SourceProbe,
	profile ffmpegargs.Profile, executable string, caps v4l2m2m.Caps, run Run) (ffmpegargs.Profile, error) {
	_, _, err := ffmpegargs.RateLimits(profile)
	if err != nil {
		return profile, err
	}
	if profile.RatePolicy == "" || profile.RatePolicy == "warn" {
		return profile, nil
	}
	if caps.Encoder(profile.VideoCodec, probe.Width, probe.Height, profile.Scale) != "" &&
		(profile.RatePolicy == "fail" || profile.PeakBitrate != "" || profile.BufferSize != "") {
		return profile, fmt.Errorf("official FFmpeg's V4L2 encoder does not map peak/buffer limits; use warn or an encoder with native rate-limit support")
	}
	if profile.RatePolicy == "fail" {
		return profile, nil
	}
	candidates, err := Candidates(profile, probe)
	if err != nil {
		return profile, err
	}
	dir, err := os.MkdirTemp("", "openlist-rate-probe-")
	if err != nil {
		return profile, err
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	var last error
	for index, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return profile, err
		}
		output := filepath.Join(dir, strconv.Itoa(index))
		if err := os.Mkdir(output, 0o700); err != nil {
			return profile, err
		}
		playlist := filepath.Join(output, "playlist.m3u8")
		duration := 6.0
		if probe.Duration > 0 && probe.Duration < duration {
			duration = probe.Duration
		}
		args, err := ffmpegargs.Build(source, probe, candidate, 2, 0, duration, 0,
			filepath.Join(output, "seg-%d.ts"), playlist, caps)
		if err != nil {
			last = err
			continue
		}
		stderr := &diagnostic{}
		err = run(ctx, executable, args, stderr)
		if err != nil {
			last = fmt.Errorf("rate probe %s failed: %v: %s", candidate.Scale, err, string(stderr.data))
			continue
		}
		if err := checkOutput(playlist, candidate, duration); err != nil {
			last = err
			continue
		}
		return candidate, nil
	}
	return profile, fmt.Errorf("no output size met the requested rate budget: %v", last)
}

func checkOutput(playlist string, profile ffmpegargs.Profile, expectedDuration float64) error {
	data, err := os.ReadFile(playlist)
	if err != nil {
		return err
	}
	durations := ffmpegargs.SegmentDurations(string(data))
	if len(durations) == 0 || !strings.Contains(string(data), "#EXT-X-ENDLIST") {
		return fmt.Errorf("rate probe did not produce complete HLS output")
	}
	var bytes int64
	var seconds float64
	for seq, duration := range durations {
		info, err := os.Stat(filepath.Join(filepath.Dir(playlist), fmt.Sprintf("seg-%d.ts", seq)))
		if err != nil || info.Size() == 0 {
			return fmt.Errorf("rate probe segment %d is missing", seq)
		}
		bytes += info.Size()
		seconds += duration
	}
	if seconds < expectedDuration-0.25 {
		return fmt.Errorf("rate probe ended early: %.3fs of %.3fs", seconds, expectedDuration)
	}
	target, _ := ffmpegargs.ParseBitrate(profile.VideoBitrate)
	audio := int64(160000)
	if profile.AudioBitrate != "" {
		audio, _ = ffmpegargs.ParseBitrate(profile.AudioBitrate)
	}
	if float64(bytes)*8/seconds > float64(target+audio)*1.25 {
		return fmt.Errorf("rate probe exceeds target: %.0f bps", float64(bytes)*8/seconds)
	}
	return nil
}

func Candidates(profile ffmpegargs.Profile, probe ffmpegargs.SourceProbe) ([]ffmpegargs.Profile, error) {
	width := probe.Width
	if width <= 0 || probe.Height <= 0 {
		return nil, fmt.Errorf("downscale policy requires known source dimensions")
	}
	if profile.Scale != "" {
		parts := strings.Split(profile.Scale, ":")
		if len(parts) != 2 || parts[1] != "-2" {
			return nil, fmt.Errorf("downscale policy requires an aspect-preserving width scale")
		}
		var err error
		width, err = strconv.Atoi(parts[0])
		if err != nil || width <= 0 {
			return nil, fmt.Errorf("invalid downscale width")
		}
		if width > probe.Width {
			width = probe.Width
			profile.Scale = fmt.Sprintf("%d:-2", width)
		}
	}
	result := []ffmpegargs.Profile{profile}
	for _, candidate := range []int{1280, 854, 640, 426, 320} {
		if candidate >= width {
			continue
		}
		p := profile
		p.Scale = fmt.Sprintf("%d:-2", candidate)
		result = append(result, p)
	}
	return result, nil
}
