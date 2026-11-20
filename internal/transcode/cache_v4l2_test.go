package transcode

import (
	"bytes"
	"strings"
	"testing"
	"time"
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

func TestSeekingDoesNotWakeUnrelatedSegmentWaiter(t *testing.T) {
	cache := NewCache(t.TempDir(), 1)
	jc, err := cache.getOrCreate("job_test", "source")
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		_, err := cache.WaitForSegment("job_test", "source", 6, time.Second)
		finished <- err
	}()
	waitFor(t, "segment waiter registered", func() bool {
		jc.mu.RLock()
		defer jc.mu.RUnlock()
		return len(jc.waiters[6]) == 1
	})
	if _, err := cache.PutSegment("job_test", "source", 18, 2, bytes.NewBufferString("later chunk"), false); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		t.Fatalf("unrelated later segment woke request: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if _, err := cache.PutSegment("job_test", "source", 6, 2, bytes.NewBufferString("requested chunk"), false); err != nil {
		t.Fatal(err)
	}
	if err := <-finished; err != nil {
		t.Fatalf("requested segment was not delivered: %v", err)
	}
}
