package transcode

import (
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
)

func TestDecideFiltersAfterProbe(t *testing.T) {
	for key, value := range map[string]string{
		conf.TranscodeEnabled: "true", conf.TranscodeMinSizeGB: "5",
		conf.TranscodeSourceExtensions: "mkv", conf.TranscodeSourceCodecs: "hevc",
		conf.TranscodeMinBitrateMbps: "20",
	} {
		op.Cache.SetSetting(key, &model.SettingItem{Key: key, Value: value})
	}
	t.Cleanup(op.Cache.ClearAll)
	r := DecideRequest{FileName: "movie.mkv", FileSize: 6 << 30}
	if result := Decide(r); !result.NeedTranscode {
		t.Fatalf("pre-probe filtering failed: %+v", result)
	}
	for _, tc := range []struct {
		codec  string
		rate   int64
		need   bool
		reason string
	}{
		{"h264", 25_000_000, false, "codec not in list"},
		{"hevc", 10_000_000, false, "bitrate below threshold"},
		{"hevc", 25_000_000, true, "ok"},
	} {
		r.Probe = SourceProbe{VideoCodec: tc.codec, VideoBitrate: tc.rate}
		if result := Decide(r); result.NeedTranscode != tc.need || result.Reason != tc.reason {
			t.Errorf("codec=%s bitrate=%d: %+v", tc.codec, tc.rate, result)
		}
	}
}
