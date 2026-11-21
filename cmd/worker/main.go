// Command openlist-transcode-worker 是独立的远程转码 Worker 节点。
// 它通过 OpenList 自研协议（HTTP/JSON）注册到 Master，长轮询领任务，调用
// 本机 ffmpeg 转码，并把生成的 HLS 切片实时上传回 Master。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/transcode/ffmpegargs"
	"github.com/OpenListTeam/OpenList/v4/internal/transcode/v4l2m2m"
)

var (
	masterURL = flag.String("master", "http://127.0.0.1:5244", "OpenList Master URL")
	secret    = flag.String("secret", "", "Worker 共享密钥（与 Master 配置一致）")
	name      = flag.String("name", hostname(), "Worker 名称")
	capacity  = flag.Int("capacity", 2, "并发任务数")
	hwaccel   = flag.String("hwaccel", "none", "硬件加速：none/auto/nvenc/qsv/vaapi/amf/videotoolbox/v4l2m2m")
	workdir   = flag.String("workdir", "./worker_tmp", "临时工作目录")
	ffmpeg    = flag.String("ffmpeg", "ffmpeg", "ffmpeg 路径")
)

func hostname() string {
	h, _ := os.Hostname()
	if h == "" {
		h = "worker"
	}
	return h
}

// ---- 协议结构 ----（与 Master 端 internal/transcode/protocol.go 一致）

type RegisterReq struct {
	Name          string   `json:"name"`
	Version       string   `json:"version"`
	Capacity      int      `json:"capacity"`
	HWAccel       []string `json:"hwaccel"`
	CodecsDecode  []string `json:"codecs_decode"`
	CodecsEncode  []string `json:"codecs_encode"`
	MaxResolution string   `json:"max_resolution"`
	Tags          []string `json:"tags"`
}
type RegisterResp struct {
	Data struct {
		WorkerID          string `json:"worker_id"`
		HeartbeatInterval int    `json:"heartbeat_interval"`
	} `json:"data"`
}

type HeartbeatReq struct {
	WorkerID  string   `json:"worker_id"`
	Load      float64  `json:"load"`
	Running   []string `json:"running"`
	FreeSlots int      `json:"free_slots"`
}
type HeartbeatResp struct {
	Data struct {
		OK     bool     `json:"ok"`
		Kick   bool     `json:"kick"`
		Cancel []string `json:"cancel"`
	} `json:"data"`
}

type ClaimReq struct {
	WorkerID string `json:"worker_id"`
	Slots    int    `json:"slots"`
	Wait     int    `json:"wait"`
}

type Profile = ffmpegargs.Profile
type OutputSpec struct {
	Format          string `json:"format"`
	SegmentDuration int    `json:"segment_duration"`
}
type Probe = ffmpegargs.SourceProbe
type Job struct {
	ID            string     `json:"id"`
	Path          string     `json:"path"`
	SourceURL     string     `json:"source_url"`
	Probe         Probe      `json:"probe"`
	Profiles      []Profile  `json:"profiles"`
	Output        OutputSpec `json:"output"`
	CallbackToken string     `json:"callback_token"`
	Deadline      int64      `json:"deadline"`
}
type ClaimResp struct {
	Data struct {
		Jobs []Job `json:"jobs"`
	} `json:"data"`
}

type FinishReq struct {
	JobID  string `json:"job_id"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
	Stats  struct {
		Elapsed  float64 `json:"elapsed"`
		BytesOut int64   `json:"bytes_out"`
	} `json:"stats"`
}

// ---- HTTP helper ----

var httpClient = &http.Client{Timeout: 60 * time.Second}

func postJSON(path string, body any, out any, bearer string) error {
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, *masterURL+path, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(raw))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// ---- main ----

func main() {
	flag.Parse()
	if strings.EqualFold(*hwaccel, "v4l2m2m") {
		*capacity = workerCapacity(*hwaccel, *capacity)
	}
	if *secret == "" {
		fmt.Fprintln(os.Stderr, "missing -secret")
		os.Exit(1)
	}
	if err := os.MkdirAll(*workdir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	workerID, hbInterval, err := registerOnce()
	if err != nil {
		fmt.Fprintln(os.Stderr, "register failed:", err)
		os.Exit(1)
	}
	fmt.Printf("[worker] registered: id=%s heartbeat=%ds\n", workerID, hbInterval)
	if strings.EqualFold(*hwaccel, "v4l2m2m") {
		if caps := v4l2m2m.DetectForFFmpeg(*ffmpeg); caps.Empty() {
			fmt.Println("[worker] v4l2m2m: no compatible device and FFmpeg codec pair found, using CPU codecs (check FFmpeg wrappers and container device access)")
		} else {
			fmt.Printf("[worker] v4l2m2m: %s\n", caps)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("[worker] shutting down")
		cancel()
	}()

	state := &workerState{
		id:        workerID,
		runningMu: sync.Mutex{},
		running:   map[string]context.CancelFunc{},
	}

	// 心跳协程
	go func() {
		t := time.NewTicker(time.Duration(hbInterval) * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				state.heartbeat()
			}
		}
	}()
	// 工作协程
	for i := 0; i < *capacity; i++ {
		go state.workLoop(ctx)
	}
	<-ctx.Done()
	time.Sleep(500 * time.Millisecond)
}

func registerOnce() (string, int, error) {
	req := RegisterReq{
		Name:          *name,
		Version:       "1.0.0",
		Capacity:      *capacity,
		HWAccel:       []string{*hwaccel},
		CodecsDecode:  []string{"h264", "hevc", "av1", "vp9", "mpeg4", "mpeg2video"},
		CodecsEncode:  []string{"h264", "hevc"},
		MaxResolution: "3840x2160",
		Tags:          []string{runtime.GOOS, runtime.GOARCH},
	}
	var resp RegisterResp
	if err := postJSON("/api/transcode/worker/register", req, &resp, *secret); err != nil {
		return "", 0, err
	}
	if resp.Data.WorkerID == "" {
		return "", 0, fmt.Errorf("master returned empty worker_id")
	}
	if resp.Data.HeartbeatInterval == 0 {
		resp.Data.HeartbeatInterval = 10
	}
	return resp.Data.WorkerID, resp.Data.HeartbeatInterval, nil
}

// ---- 状态机 ----

type workerState struct {
	id        string
	runningMu sync.Mutex
	running   map[string]context.CancelFunc
}

func (s *workerState) heartbeat() {
	s.runningMu.Lock()
	ids := make([]string, 0, len(s.running))
	for id := range s.running {
		ids = append(ids, id)
	}
	free := *capacity - len(ids)
	s.runningMu.Unlock()
	var resp HeartbeatResp
	if err := postJSON("/api/transcode/worker/heartbeat", HeartbeatReq{
		WorkerID:  s.id,
		Running:   ids,
		FreeSlots: free,
	}, &resp, *secret); err != nil {
		fmt.Println("[worker] heartbeat error:", err)
		return
	}
	for _, jid := range resp.Data.Cancel {
		s.runningMu.Lock()
		if cancel, ok := s.running[jid]; ok {
			cancel()
		}
		s.runningMu.Unlock()
	}
}

func (s *workerState) workLoop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		var resp ClaimResp
		err := postJSON("/api/transcode/worker/claim", ClaimReq{
			WorkerID: s.id, Slots: 1, Wait: 20,
		}, &resp, *secret)
		if err != nil {
			fmt.Println("[worker] claim error:", err)
			time.Sleep(5 * time.Second)
			continue
		}
		if len(resp.Data.Jobs) == 0 {
			continue
		}
		for _, j := range resp.Data.Jobs {
			s.runJob(ctx, j)
		}
	}
}

func (s *workerState) runJob(parent context.Context, job Job) {
	jobCtx, cancel := context.WithCancel(parent)
	s.runningMu.Lock()
	s.running[job.ID] = cancel
	s.runningMu.Unlock()
	defer func() {
		s.runningMu.Lock()
		delete(s.running, job.ID)
		s.runningMu.Unlock()
		cancel()
	}()

	startedAt := time.Now()
	tmpDir := filepath.Join(*workdir, job.ID)
	_ = os.MkdirAll(tmpDir, 0o755)
	defer os.RemoveAll(tmpDir)

	if len(job.Profiles) == 0 {
		s.finish(job, "failed", "no profiles", 0, 0)
		return
	}
	profile := job.Profiles[0]
	playlist := filepath.Join(tmpDir, "playlist.m3u8")
	segPattern := filepath.Join(tmpDir, "seg-%d.ts")
	args := buildFFmpegArgs(job, profile, segPattern, playlist)

	cmd := exec.CommandContext(jobCtx, *ffmpeg, args...)
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	fmt.Printf("[worker] running job %s, ffmpeg %s\n", job.ID, strings.Join(args, " "))
	if err := cmd.Start(); err != nil {
		s.finish(job, "failed", err.Error(), 0, 0)
		return
	}

	// 上传协程
	stopUpload := make(chan struct{})
	uploadDone := make(chan struct{})
	go func() {
		defer close(uploadDone)
		s.watchAndUpload(jobCtx, job, profile.Name, tmpDir, stopUpload)
	}()

	err := cmd.Wait()
	close(stopUpload)
	<-uploadDone
	// 最后一次扫描：把还未上传的尾片传上去
	s.uploadOnce(job, profile.Name, tmpDir, true)

	if err != nil {
		if jobCtx.Err() == context.Canceled {
			s.finish(job, "cancelled", "cancelled by master", 0, 0)
			return
		}
		s.finish(job, "failed", fmt.Sprintf("%v: %s", err, truncate(stderr.String(), 1024)), 0, 0)
		return
	}
	s.finish(job, "finished", "", time.Since(startedAt).Seconds(), 0)
}

func (s *workerState) finish(job Job, status, errStr string, elapsed float64, bytesOut int64) {
	req := FinishReq{
		JobID: job.ID, Status: status, Error: errStr,
	}
	req.Stats.Elapsed = elapsed
	req.Stats.BytesOut = bytesOut
	if err := postJSON("/api/transcode/worker/job/finish", req, nil, job.CallbackToken); err != nil {
		fmt.Printf("[worker] finish report failed: %v\n", err)
	}
}

func (s *workerState) watchAndUpload(_ context.Context, job Job, profile, dir string, stop chan struct{}) {
	t := time.NewTicker(800 * time.Millisecond)
	defer t.Stop()
	uploaded := map[int]bool{}
	flush := func(final bool) {
		s.uploadDelta(job, profile, dir, uploaded, final)
	}
	for {
		select {
		case <-stop:
			flush(false)
			return
		case <-t.C:
			flush(false)
		}
	}
}

func (s *workerState) uploadOnce(job Job, profile, dir string, final bool) {
	uploaded := map[int]bool{}
	s.uploadDelta(job, profile, dir, uploaded, final)
}

// uploadDelta 扫描目录，上传新切片（uploaded 跟踪已传过的 seq；最后一片到 final 时才上传）
func (s *workerState) uploadDelta(job Job, profile, dir string, uploaded map[int]bool, final bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type segFile struct {
		seq  int
		path string
	}
	var segs []segFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if !strings.HasPrefix(n, "seg-") || !strings.HasSuffix(n, ".ts") {
			continue
		}
		num, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(n, "seg-"), ".ts"))
		if err != nil {
			continue
		}
		segs = append(segs, segFile{num, filepath.Join(dir, n)})
	}
	// 排序
	for i := 0; i < len(segs); i++ {
		for j := i + 1; j < len(segs); j++ {
			if segs[j].seq < segs[i].seq {
				segs[i], segs[j] = segs[j], segs[i]
			}
		}
	}
	limit := len(segs)
	if !final && limit > 0 {
		limit-- // 最后一片可能还在写，留到下一轮
	}
	segDur := job.Output.SegmentDuration
	if segDur == 0 {
		segDur = 6
	}
	durations := map[int]float64{}
	if data, err := os.ReadFile(filepath.Join(dir, "playlist.m3u8")); err == nil {
		durations = ffmpegargs.SegmentDurations(string(data))
	}
	for i := 0; i < limit; i++ {
		seg := segs[i]
		if uploaded[seg.seq] {
			continue
		}
		isLast := final && i == limit-1
		duration := durations[seg.seq]
		if duration <= 0 {
			duration = float64(segDur)
		}
		if err := s.uploadSegment(job, profile, seg.seq, duration, seg.path, isLast); err != nil {
			fmt.Printf("[worker] upload seg-%d failed: %v\n", seg.seq, err)
			return
		}
		uploaded[seg.seq] = true
	}
}

func (s *workerState) uploadSegment(job Job, profile string, seq int, dur float64, file string, final bool) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	q := fmt.Sprintf("?job=%s&profile=%s&seq=%d&duration=%f&final=%t",
		job.ID, profile, seq, dur, final)
	req, _ := http.NewRequest(http.MethodPut, *masterURL+"/api/transcode/worker/segment"+q, f)
	req.Header.Set("Content-Type", "video/mp2t")
	req.Header.Set("Authorization", "Bearer "+job.CallbackToken)
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(raw))
	}
	return nil
}

// ---- ffmpeg args（与 master 端 local_runner 保持一致）----

// detectV4L2M2M 可在测试中替换，避免依赖本机设备
var detectV4L2M2M = func() v4l2m2m.Caps { return v4l2m2m.DetectForFFmpeg(*ffmpeg) }

func buildFFmpegArgs(job Job, profile Profile, segPattern, playlist string) []string {
	var caps v4l2m2m.Caps
	if strings.EqualFold(profile.HWAccel, "v4l2m2m") {
		caps = detectV4L2M2M()
	}
	return ffmpegargs.Build(job.SourceURL, job.Probe, profile, job.Output.SegmentDuration, 0, 0, 0, segPattern, playlist, caps)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func workerCapacity(hw string, requested int) int {
	if strings.EqualFold(hw, "v4l2m2m") {
		return 1
	}
	return requested
}
