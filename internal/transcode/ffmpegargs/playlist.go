package ffmpegargs

import (
	"math"
	"strconv"
	"strings"
)

// SegmentDurations parses FFmpeg HLS output without assuming fixed segment lengths.
func SegmentDurations(data string) map[int]float64 {
	out := make(map[int]float64)
	lines := strings.Split(data, "\n")
	var pendingDur float64
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#EXTINF:") {
			pendingDur = 0
			// #EXTINF:6.000000,
			body := strings.TrimPrefix(line, "#EXTINF:")
			body = strings.TrimSuffix(body, ",")
			d, err := strconv.ParseFloat(strings.TrimSpace(body), 64)
			if err == nil && d > 0 && !math.IsNaN(d) && !math.IsInf(d, 0) {
				pendingDur = d
			}
			continue
		}
		if strings.HasSuffix(line, ".ts") && strings.HasPrefix(line, "seg-") {
			numStr := strings.TrimSuffix(strings.TrimPrefix(line, "seg-"), ".ts")
			if seq, err := strconv.Atoi(numStr); err == nil && pendingDur > 0 {
				out[seq] = pendingDur
			}
			pendingDur = 0
		} else if line != "" && !strings.HasPrefix(line, "#") {
			pendingDur = 0
		}
	}
	return out
}
