package transcode

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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
