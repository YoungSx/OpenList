package ffmpegargs

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

func ParseBitrate(value string) (int64, error) {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return 0, nil
	}
	multiplier := int64(1)
	if strings.HasSuffix(value, "k") {
		multiplier, value = 1000, strings.TrimSuffix(value, "k")
	} else if strings.HasSuffix(value, "m") {
		multiplier, value = 1000000, strings.TrimSuffix(value, "m")
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < 0 || n > math.MaxInt64/multiplier {
		return 0, fmt.Errorf("invalid bitrate %q", value)
	}
	return n * multiplier, nil
}

// RateLimits returns a video peak and buffer envelope, not an HRD/CBR promise.
func RateLimits(profile Profile) (int64, int64, error) {
	if profile.RatePolicy != "" && profile.RatePolicy != "warn" && profile.RatePolicy != "fail" && profile.RatePolicy != "downscale" {
		return 0, 0, fmt.Errorf("unknown rate policy %q", profile.RatePolicy)
	}
	if profile.RatePolicy == "" || profile.RatePolicy == "warn" {
		return 0, 0, nil
	}
	target, err := ParseBitrate(profile.VideoBitrate)
	if err != nil || target <= 0 || target > math.MaxInt32 {
		return 0, 0, fmt.Errorf("rate policy requires a positive video bitrate")
	}
	peak := target
	if profile.PeakBitrate != "" {
		peak, err = ParseBitrate(profile.PeakBitrate)
	}
	if err != nil || peak < target || peak > math.MaxInt32 {
		return 0, 0, fmt.Errorf("peak bitrate must be at least the target")
	}
	buffer := peak * 2
	if profile.BufferSize != "" {
		buffer, err = ParseBitrate(profile.BufferSize)
	}
	if err != nil || buffer < 8192 || buffer > math.MaxInt32 {
		return 0, 0, fmt.Errorf("buffer must hold 8192..2147483647 bits")
	}
	return peak, buffer, nil
}
