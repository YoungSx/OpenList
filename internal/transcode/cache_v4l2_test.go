package transcode

import (
	"strings"
	"testing"
)

func TestV4L2MasterPlaylistDoesNotClaimUnknownProfile(t *testing.T) {
	cache := &Cache{}
	profiles := []Profile{{Name: "source", VideoCodec: "h264", VideoBitrate: "1000k", AudioBitrate: "96k", HWAccel: "v4l2m2m"}}
	playlist := cache.MasterPlaylist(profiles, "/tc/")
	if strings.Contains(playlist, "CODECS=") || !strings.Contains(playlist, "BANDWIDTH=1096000") {
		t.Fatalf("unexpected V4L2 master playlist: %s", playlist)
	}
	profiles[0].HWAccel = "none"
	if playlist := cache.MasterPlaylist(profiles, "/tc/"); !strings.Contains(playlist, "CODECS=") {
		t.Fatalf("software playlist lost CODECS: %s", playlist)
	}
}
