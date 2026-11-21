package transcode

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestV4L2MasterPlaylistDoesNotClaimUnknownProfile(t *testing.T) {
	cache := &Cache{}
	profiles := []Profile{{Name: "source", VideoCodec: "h264", VideoBitrate: "1000k", AudioBitrate: "96k", HWAccel: "v4l2m2m"}}
	playlist := cache.MasterPlaylist("job", profiles, "/tc/")
	if strings.Contains(playlist, "CODECS=") || !strings.Contains(playlist, "BANDWIDTH=1096000") {
		t.Fatalf("unexpected V4L2 master playlist: %s", playlist)
	}
	profiles[0].HWAccel = "none"
	if playlist := cache.MasterPlaylist("job", profiles, "/tc/"); !strings.Contains(playlist, "CODECS=") {
		t.Fatalf("software playlist lost CODECS: %s", playlist)
	}
}

func TestMasterPlaylistUsesActualSegmentBandwidth(t *testing.T) {
	cache := NewCache(t.TempDir(), 1)
	jc, err := cache.getOrCreate("job", "source")
	if err != nil {
		t.Fatal(err)
	}
	jc.publishSegment(&SegmentInfo{Seq: 0, Duration: 2, Size: 2_000_000}, false)
	jc.publishSegment(&SegmentInfo{Seq: 1, Duration: .5, Size: 600_000}, false)
	profiles := []Profile{{Name: "source", HWAccel: "v4l2m2m", VideoBitrate: "1000k", AudioBitrate: "96k"}}
	playlist := cache.MasterPlaylist("job", profiles, "/tc/")
	if !strings.Contains(playlist, "BANDWIDTH=9600000,AVERAGE-BANDWIDTH=8320000") {
		t.Fatalf("requested bitrate replaced actual bandwidth: %s", playlist)
	}
	// Correcting a short final segment must not double-count its bytes.
	jc.publishSegment(&SegmentInfo{Seq: 1, Duration: .5, Size: 600_000}, false)
	if again := cache.MasterPlaylist("job", profiles, "/tc/"); again != playlist {
		t.Fatalf("republication changed measured average: %s", again)
	}
}

func TestChunkPublicationUsesItsFinalDuration(t *testing.T) {
	cache := NewCache(t.TempDir(), 1)
	jc, err := cache.getOrCreate("job", "source")
	if err != nil {
		t.Fatal(err)
	}
	playlist := filepath.Join(jc.BaseDir, "_chunk_0.m3u8")
	if err := os.WriteFile(playlist, []byte("#EXTINF:0.500000,\nseg-1.ts\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jc.BaseDir, "seg-1.ts"), make([]byte, 1000), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := NewLocalRunner(&Manager{Cache: cache})
	runner.publishChunkSegments(&Job{ID: "job", Output: OutputSpec{SegmentDuration: 2}}, Profile{Name: "source"}, jc.BaseDir, 1, 2, playlist)
	info, err := cache.GetSegment("job", "source", 1)
	if err != nil || info.Duration != .5 {
		t.Fatalf("short segment: %+v, %v", info, err)
	}
	if peak, average := cache.profileBandwidth("job", "source"); peak != 16000 || average != 16000 {
		t.Fatalf("wrong duration produced bandwidth %d/%d", peak, average)
	}
}

func TestMeasuredSegmentsRejectInvalidDurations(t *testing.T) {
	cache := NewCache(t.TempDir(), 1)
	for _, duration := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		if _, err := cache.PutSegment("job", "source", 0, duration, bytes.NewBufferString("data"), false); err != ErrInvalidSegmentDuration {
			t.Fatalf("duration %v: %v", duration, err)
		}
	}
}

func TestMarkFinalWakesUnfulfilledSegmentRequests(t *testing.T) {
	cache := NewCache(t.TempDir(), 1)
	jc, err := cache.getOrCreate("job", "source")
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := cache.WaitForSegment("job", "source", 1, time.Minute)
		result <- err
	}()
	waitFor(t, "segment waiter", func() bool {
		jc.mu.RLock()
		defer jc.mu.RUnlock()
		return len(jc.waiters[1]) == 1
	})
	cache.MarkFinal("job", "source")
	cache.MarkFinal("job", "source")
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("unproduced segment was reported as available")
		}
	case <-time.After(time.Second):
		t.Fatal("failed encode left the segment request waiting")
	}
}

func TestUnreachableRateIsReportedWithoutFailingPlayback(t *testing.T) {
	cache := NewCache(t.TempDir(), 1)
	jc, err := cache.getOrCreate("job", "source")
	if err != nil {
		t.Fatal(err)
	}
	job := NewJob()
	job.ID = "job"
	profile := Profile{Name: "source", HWAccel: "v4l2m2m", VideoBitrate: "1000k", AudioBitrate: "96k"}
	runner := NewLocalRunner(&Manager{Cache: cache})
	for seq := 0; seq < 3; seq++ {
		jc.publishSegment(&SegmentInfo{Seq: seq, Duration: 2, Size: 2_000_000}, false)
		runner.reportBitrate(job, profile)
		if seq < 2 && job.Warning != "" {
			t.Fatal("startup-only measurement reported unreachable rate")
		}
	}
	if !strings.Contains(job.Warning, "measured HLS bandwidth 8000000") || job.GetStatus() == JobFailed {
		t.Fatalf("rate diagnostic missing or destructive: %q status=%s", job.Warning, job.GetStatus())
	}
}

func TestRateWarningCanBeReadWhileEncoding(t *testing.T) {
	job := NewJob()
	var done sync.WaitGroup
	done.Add(1)
	go func() {
		defer done.Done()
		for i := 0; i < 100; i++ {
			job.mu.Lock()
			job.Warning = "measured rate exceeds target"
			job.mu.Unlock()
		}
	}()
	for i := 0; i < 100; i++ {
		if _, err := json.Marshal(job); err != nil {
			t.Fatal(err)
		}
	}
	done.Wait()
}

func TestReadinessAndFinishWhileJobIsBeingRead(t *testing.T) {
	job := NewJob()
	job.ID = "job"
	scheduler := NewScheduler(nil)
	scheduler.jobs[job.ID] = job
	// A JSON reader holds this lock while readiness and completion arrive.
	job.mu.RLock()
	finished := make(chan error, 1)
	go func() {
		finished <- scheduler.Finish(&FinishRequest{JobID: job.ID, Status: JobFinished})
	}()
	runtime.Gosched()
	time.Sleep(time.Millisecond)
	ready := make(chan struct{})
	go func() { job.MarkReady(); close(ready) }()
	runtime.Gosched()
	time.Sleep(time.Millisecond)
	job.mu.RUnlock()
	for _, done := range []<-chan struct{}{ready, job.WaitReady()} {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("readiness and completion deadlocked behind a job reader")
		}
	}
	select {
	case err := <-finished:
		if err != nil || job.GetStatus() != JobFinished {
			t.Fatalf("completion was lost: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("completion did not return")
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
