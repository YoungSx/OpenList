package transcode

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
)

// testSession 构造一个不依赖 setting/db 的 session，每个 chunk 10 片
func testSession(t *testing.T, chunkCount int) *ChunkSession {
	t.Helper()
	s := &ChunkSession{
		JobID:         "job",
		Profile:       "p",
		Job:           &Job{ID: "job", SourceURL: "http://127.0.0.1:5244/d/movie.mkv", Probe: SourceProbe{VideoCodec: "h264", PixFmt: "yuv420p", Width: 1920, Height: 1080, Duration: 600}},
		ProfDef:       Profile{Name: "p", VideoCodec: "h264", VideoBitrate: "2000k", AudioCodec: "aac", AudioBitrate: "160k"},
		SegDur:        6,
		ChunkDur:      60,
		MaxConcurrent: 1,
		IdleSec:       60,
		HW:            true,
		FFmpegPath:    "ffmpeg",
		Timeout:       time.Minute,
		chunks:        make(map[int]*Chunk),
		allDoneCh:     make(chan struct{}),
		OutDir:        t.TempDir(),
	}
	for i := 0; i < chunkCount; i++ {
		s.chunks[i] = &Chunk{Index: i, StartSeq: i * 10, EndSeq: (i + 1) * 10, Duration: 60, State: ChunkIdle}
	}
	return s
}

func chunkState(s *ChunkSession, idx int) ChunkState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.chunks[idx].State
}

func setChunk(s *ChunkSession, idx int, state ChunkState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chunks[idx].State = state
	s.chunks[idx].LastAccess = time.Now()
}

// chunkFromArgs 从 ffmpeg 参数里的临时 playlist 名解析 chunk 序号
func chunkFromArgs(t *testing.T, args []string) int {
	t.Helper()
	for _, a := range args {
		base := filepath.Base(a)
		if strings.HasPrefix(base, "_chunk_") {
			var n int
			if _, err := fmt.Sscanf(base, "_chunk_%d.m3u8", &n); err == nil {
				return n
			}
		}
	}
	t.Fatalf("no chunk playlist in %v", args)
	return -1
}

func TestChunkSessionIncludesSubsecondTail(t *testing.T) {
	for key, value := range map[string]string{
		conf.TranscodeSegmentDuration: "2", conf.TranscodeChunkDurationSec: "12",
		conf.TranscodeFFmpegPath: "ffmpeg", conf.TranscodeChunkPrefetch: "false",
		conf.TranscodeMaxChunkConcurrency: "1", conf.TranscodeChunkIdleSec: "60",
		conf.TranscodeJobTimeoutMin: "120",
	} {
		op.Cache.SetSetting(key, &model.SettingItem{Key: key, Value: value})
	}
	t.Cleanup(op.Cache.ClearAll)
	previous := runProcess
	runProcess = func(context.Context, string, []string, io.Writer) error { return nil }
	t.Cleanup(func() { runProcess = previous })
	for _, tc := range []struct {
		duration       float64
		lastSeq, chunk int
		tailDuration   float64
	}{
		{.5, 0, 0, .5},
		{6.5, 3, 0, 6.5},
		{12.5, 6, 1, .5},
	} {
		cm := NewChunkManager(nil)
		job := NewJob()
		job.ID = "tail"
		job.Probe.Duration = tc.duration
		session, err := cm.StartSession(job, Profile{Name: "source"}, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if got := cm.findChunkBySeq(session, tc.lastSeq); got != tc.chunk {
			t.Fatalf("duration %v lost segment %d: chunk=%d", tc.duration, tc.lastSeq, got)
		}
		if got := session.chunks[tc.chunk]; got.EndSeq != tc.lastSeq+1 || got.Duration != tc.tailDuration {
			t.Fatalf("wrong tail metadata: %+v", got)
		}
		waitFor(t, "initial chunk done", func() bool { return chunkState(session, 0) == ChunkDone })
		close(cm.stopCh)
	}
	for _, duration := range []float64{math.NaN(), math.Inf(1), -1, float64(maxSegmentSeq+2) * 2} {
		job := NewJob()
		job.Probe.Duration = duration
		if _, err := NewChunkManager(nil).StartSession(job, Profile{}, t.TempDir()); err == nil {
			t.Fatalf("invalid duration %v accepted", duration)
		}
	}
}

func TestFailedChunkEndsSessionWithoutAnotherLaunch(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(fmt.Sprintf("timeout=%t", timeout), func(t *testing.T) {
			s := testSession(t, 2)
			if timeout {
				s.Timeout = 10 * time.Millisecond
			}
			cm := NewChunkManager(nil)
			launches := 0
			previous := runProcess
			runProcess = func(ctx context.Context, _ string, _ []string, _ io.Writer) error {
				launches++
				if timeout {
					<-ctx.Done()
					return ctx.Err()
				}
				return fmt.Errorf("rate budget exceeded")
			}
			t.Cleanup(func() { runProcess = previous })
			cm.ensureChunk(s, 0)
			select {
			case <-s.WaitDone():
			case <-time.After(time.Second):
				t.Fatal("failed chunk left the session waiting")
			}
			if s.Err() == nil || chunkState(s, 0) != ChunkFailed {
				t.Fatal("session did not preserve its failure")
			}
			cm.ensureChunk(s, 1)
			if launches != 1 {
				t.Fatalf("failed session launched another encoder: %d", launches)
			}
		})
	}
}

func TestResumedJobKeepsSelectedProfile(t *testing.T) {
	for key, value := range map[string]string{conf.TranscodeFFmpegPath: "ffmpeg", conf.TranscodeJobTimeoutMin: "1"} {
		op.Cache.SetSetting(key, &model.SettingItem{Key: key, Value: value})
	}
	t.Cleanup(op.Cache.ClearAll)
	caps := s805
	caps.RateLimits = true
	withV4L2M2M(t, caps)
	manager := &Manager{Scheduler: NewScheduler(NewWorkerRegistry()), Cache: NewCache(t.TempDir(), 1)}
	manager.Chunks = NewChunkManager(manager)
	runner := NewLocalRunner(manager)
	job := NewJob()
	job.SourceURL = "/media/movie.mp4"
	job.Probe = SourceProbe{Width: 1920, Height: 1080}
	requested := Profile{Name: "source", VideoCodec: "h264", VideoBitrate: "1000k", HWAccel: "v4l2m2m", RatePolicy: "downscale", Scale: "1920:-2"}
	selected := requested
	selected.Scale = "640:-2"
	job.Profiles = []Profile{requested}
	job.EffectiveProfile = &selected
	job = manager.Scheduler.Submit(job)
	previous := runProcess
	calls := 0
	runProcess = func(_ context.Context, _ string, args []string, _ io.Writer) error {
		calls++
		if option(args, "-vf") != "scale=640:-2" || filepath.Base(args[len(args)-1]) != "playlist.m3u8" {
			t.Fatalf("resume probed again or changed output size: %v", args)
		}
		return nil
	}
	t.Cleanup(func() { runProcess = previous })
	runner.runJob(job)
	if _, err := manager.Scheduler.Reactivate(job.ID); err != nil {
		t.Fatal(err)
	}
	runner.runJob(job)
	if calls != 2 || job.Profiles[0] != requested || *job.EffectiveProfile != selected {
		t.Fatalf("selected profile changed across resume: calls=%d job=%+v", calls, job)
	}
}

func TestWorkerFinishKeepsEffectiveProfileOnReactivation(t *testing.T) {
	scheduler := NewScheduler(NewWorkerRegistry())
	job := NewJob()
	requested := Profile{Name: "source", Scale: "1920:-2"}
	selected := requested
	selected.Scale = "640:-2"
	job.Profiles = []Profile{requested}
	job = scheduler.Submit(job)
	if err := scheduler.Finish(&FinishRequest{JobID: job.ID, Status: JobCancelled, EffectiveProfile: &selected}); err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.Reactivate(job.ID); err != nil {
		t.Fatal(err)
	}
	if *job.EffectiveProfile != selected || job.Profiles[0] != requested {
		t.Fatal("worker selection was lost or replaced the requested profile")
	}
	selected.Name = "another-profile"
	if err := scheduler.Finish(&FinishRequest{JobID: job.ID, Status: JobFinished, EffectiveProfile: &selected}); err == nil || job.GetStatus() != JobPending {
		t.Fatal("worker changed the job's profile identity")
	}
}

func TestFailedEncodeDoesNotPublishItsTail(t *testing.T) {
	for _, chunks := range []bool{false, true} {
		t.Run(fmt.Sprintf("chunks=%t", chunks), func(t *testing.T) {
			for key, value := range map[string]string{conf.TranscodeFFmpegPath: "ffmpeg", conf.TranscodeSegmentDuration: "2",
				conf.TranscodeChunkDurationSec: "12", conf.TranscodeChunkPrefetch: "false", conf.TranscodeJobTimeoutMin: "1",
				conf.TranscodeMaxChunkConcurrency: "1", conf.TranscodeChunkIdleSec: "60"} {
				op.Cache.SetSetting(key, &model.SettingItem{Key: key, Value: value})
			}
			t.Cleanup(op.Cache.ClearAll)
			manager := &Manager{Scheduler: NewScheduler(NewWorkerRegistry()), Cache: NewCache(t.TempDir(), 1)}
			manager.Chunks = NewChunkManager(manager)
			runner := NewLocalRunner(manager)
			manager.localRunner = runner
			job := manager.Scheduler.Submit(NewJob())
			job.Probe.Duration = 4
			job.Output.SegmentDuration = 2
			profile := Profile{Name: "source", VideoCodec: "h264"}
			if _, err := manager.Cache.PutSegment(job.ID, profile.Name, 0, 2, strings.NewReader("completed segment"), false); err != nil {
				t.Fatal(err)
			}
			jc, _ := manager.Cache.getOrCreate(job.ID, profile.Name)
			previous := runProcess
			runProcess = func(_ context.Context, _ string, args []string, _ io.Writer) error {
				playlist := args[len(args)-1]
				if err := os.WriteFile(filepath.Join(filepath.Dir(playlist), "seg-1.ts"), []byte("audio-only tail after video failure"), 0o600); err != nil {
					return err
				}
				if err := os.WriteFile(playlist, []byte("#EXTINF:0.5,\nseg-1.ts\n#EXT-X-ENDLIST\n"), 0o600); err != nil {
					return err
				}
				return fmt.Errorf("video rate budget exceeded")
			}
			t.Cleanup(func() { runProcess = previous })
			if chunks {
				runner.runJobByChunks(context.Background(), job, profile, jc.BaseDir)
			} else {
				runner.runJobLegacy(context.Background(), job, profile, jc.BaseDir)
			}
			if job.GetStatus() != JobFailed {
				t.Fatal("failed video encode completed the job")
			}
			if _, err := manager.Cache.GetSegment(job.ID, profile.Name, 0); err != nil {
				t.Fatal("failure discarded a completed cached segment")
			}
			if _, err := manager.Cache.GetSegment(job.ID, profile.Name, 1); err == nil {
				t.Fatal("failed encoder published its uncommitted tail")
			}
		})
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestDeviceWaitCanBeCancelled(t *testing.T) {
	cm := NewChunkManager(nil)
	if !cm.acquireDevice(context.Background()) {
		t.Fatal("first device acquisition failed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan bool, 1)
	go func() { result <- cm.acquireDevice(ctx) }()
	cancel()
	select {
	case acquired := <-result:
		if acquired {
			t.Fatal("cancelled request acquired busy device")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled request kept waiting for busy device")
	}
	cm.releaseDevice()
	if !cm.acquireDevice(context.Background()) {
		t.Fatal("device was not released")
	}
	cm.releaseDevice()
}

func TestLegacyAndChunkUseSameDeviceSlot(t *testing.T) {
	for key, value := range map[string]string{
		conf.TranscodeFFmpegPath: "ffmpeg", conf.TranscodeSegmentDuration: "6",
	} {
		op.Cache.SetSetting(key, &model.SettingItem{Key: key, Value: value})
	}
	t.Cleanup(op.Cache.ClearAll)
	withV4L2M2M(t, s805)
	manager := &Manager{Scheduler: NewScheduler(NewWorkerRegistry()), Cache: NewCache(t.TempDir(), 1)}
	manager.Chunks = NewChunkManager(nil)
	runner := NewLocalRunner(manager)
	job := manager.Scheduler.Submit(NewJob())
	job.SourceURL = "/tmp/movie.mkv"
	job.Probe = SourceProbe{VideoCodec: "h264", PixFmt: "yuv420p", Width: 1920, Height: 1080}
	profile := Profile{Name: "test", VideoCodec: "h264", HWAccel: "v4l2m2m"}
	chunk := testSession(t, 1)
	entered := make(chan string, 2)
	exitLegacy := make(chan struct{})
	previous := runProcess
	runProcess = func(_ context.Context, _ string, args []string, _ io.Writer) error {
		name := filepath.Base(args[len(args)-1])
		entered <- name
		if name == "playlist.m3u8" {
			<-exitLegacy
		}
		return nil
	}
	t.Cleanup(func() { runProcess = previous })
	legacyDone := make(chan struct{})
	dir := t.TempDir()
	go func() {
		runner.runJobLegacy(context.Background(), job, profile, dir)
		close(legacyDone)
	}()
	if got := <-entered; got != "playlist.m3u8" {
		t.Fatalf("unexpected first process: %s", got)
	}
	manager.Chunks.ensureChunk(chunk, 0)
	select {
	case <-entered:
		t.Fatal("chunk started while legacy process held the device")
	case <-time.After(30 * time.Millisecond):
	}
	close(exitLegacy)
	select {
	case name := <-entered:
		if name != "_chunk_0.m3u8" {
			t.Fatalf("unexpected next process: %s", name)
		}
	case <-time.After(time.Second):
		t.Fatal("chunk did not start after legacy process exited")
	}
	<-legacyDone
	waitFor(t, "chunk done", func() bool { return chunkState(chunk, 0) == ChunkDone })
}

// 硬件会话：拖动到下一段时旧进程被抢占，但要等它退出、设备释放后才能启动
func TestHWChunksSerialized(t *testing.T) {
	s := testSession(t, 2)
	cm := NewChunkManager(nil)

	var mu sync.Mutex
	running, peak := 0, 0
	var order []int
	entered := make(chan int, 4)
	release := make(chan struct{}, 4)

	prev := runProcess
	runProcess = func(ctx context.Context, _ string, args []string, _ io.Writer) error {
		idx := chunkFromArgs(t, args)
		mu.Lock()
		running++
		if running > peak {
			peak = running
		}
		order = append(order, idx)
		mu.Unlock()
		entered <- idx
		select {
		case <-release:
		case <-ctx.Done():
		}
		mu.Lock()
		running--
		mu.Unlock()
		return ctx.Err()
	}
	t.Cleanup(func() { runProcess = prev })

	cm.ensureChunk(s, 0)
	if got := <-entered; got != 0 {
		t.Fatalf("first process is chunk %d, want 0", got)
	}

	// 拖动到 chunk 1：chunk 0 被抢占，chunk 1 必须等它退出
	cm.ensureChunk(s, 1)
	if got := <-entered; got != 1 {
		t.Fatalf("second process is chunk %d, want 1", got)
	}
	if st := chunkState(s, 0); st != ChunkKilled {
		t.Fatalf("chunk 0 state = %v, want ChunkKilled", st)
	}
	if st := chunkState(s, 1); st != ChunkRunning {
		t.Fatalf("chunk 1 state = %v, want ChunkRunning", st)
	}

	release <- struct{}{}
	waitFor(t, "chunk 1 done", func() bool { return chunkState(s, 1) == ChunkDone })

	mu.Lock()
	defer mu.Unlock()
	if peak != 1 {
		t.Fatalf("peak concurrent ffmpeg = %d, want 1", peak)
	}
	if len(order) != 2 || order[0] != 0 || order[1] != 1 {
		t.Fatalf("launch order = %v, want [0 1]", order)
	}
}

// 硬件会话：当前段没转完时不预读；转完立即启动下一段
func TestHWPefetchAfterDone(t *testing.T) {
	s := testSession(t, 2)
	s.Prefetch = true
	cm := NewChunkManager(nil)

	var mu sync.Mutex
	var started []int
	prev := runProcess
	runProcess = func(_ context.Context, _ string, args []string, _ io.Writer) error {
		idx := chunkFromArgs(t, args)
		mu.Lock()
		started = append(started, idx)
		mu.Unlock()
		return nil
	}
	t.Cleanup(func() { runProcess = prev })

	setChunk(s, 0, ChunkRunning)
	cm.maintainSession(s)
	if st := chunkState(s, 1); st != ChunkIdle {
		t.Fatalf("chunk 1 launched while chunk 0 still runs: state=%v", st)
	}

	setChunk(s, 0, ChunkDone)
	cm.maintainSession(s)
	// 等进程真正跑完，免得测试结束后 goroutine 还在读 runProcess
	waitFor(t, "chunk 1 done", func() bool { return chunkState(s, 1) == ChunkDone })

	mu.Lock()
	defer mu.Unlock()
	if len(started) != 1 || started[0] != 1 {
		t.Fatalf("started = %v, want [1]", started)
	}
}

func TestHWDeviceWaitsForProcessExitAcrossSessions(t *testing.T) {
	first, second := testSession(t, 1), testSession(t, 1)
	second.JobID = "second"
	cm := NewChunkManager(nil)
	entered := make(chan struct{}, 2)
	cancelled := make(chan struct{})
	release := make(chan struct{})
	var calls int
	previous := runProcess
	runProcess = func(ctx context.Context, _ string, _ []string, _ io.Writer) error {
		calls++ // guarded by the device lock
		entered <- struct{}{}
		if calls == 1 {
			<-ctx.Done()
			close(cancelled)
			<-release // device is still owned until the process actually exits
			return ctx.Err()
		}
		return nil
	}
	t.Cleanup(func() { runProcess = previous })
	cm.ensureChunk(first, 0)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first process did not start")
	}
	cm.ensureChunk(second, 0)
	cm.killAllChunks(first)
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("first process did not observe cancellation")
	}
	select {
	case <-entered:
		t.Fatal("second session acquired the device before the first process exited")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("second session did not acquire the released device")
	}
	waitFor(t, "second session done", func() bool { return chunkState(second, 0) == ChunkDone })
}
