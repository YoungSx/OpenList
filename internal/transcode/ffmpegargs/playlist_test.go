package ffmpegargs

import (
	"reflect"
	"testing"
)

func TestSegmentDurationsPreserveShortLastSegment(t *testing.T) {
	data := "#EXTM3U\n#EXTINF:2.002000,\nseg-0.ts\n#EXTINF:0.500000,\nseg-1.ts\n#EXT-X-ENDLIST\n"
	if got := SegmentDurations(data); !reflect.DeepEqual(got, map[int]float64{0: 2.002, 1: .5}) {
		t.Fatalf("durations: %v", got)
	}
	for _, invalid := range []string{"NaN", "+Inf", "-1", "bad"} {
		data := "#EXTINF:2,\n#EXTINF:" + invalid + ",\nseg-3.ts\n"
		if got := SegmentDurations(data); len(got) != 0 {
			t.Fatalf("invalid %s inherited an older duration: %v", invalid, got)
		}
	}
}
