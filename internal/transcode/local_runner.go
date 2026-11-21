package transcode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/setting"
	"github.com/OpenListTeam/OpenList/v4/internal/transcode/ffmpegargs"
	"github.com/OpenListTeam/OpenList/v4/internal/transcode/ffmpegpolicy"
	"github.com/OpenListTeam/OpenList/v4/internal/transcode/v4l2m2m"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
)

// detectV4L2M2M 可在测试中替换，避免依赖本机设备
var detectV4L2M2M = v4l2m2m.DetectForFFmpeg

// hardwareSession reserves the single-device scheduler for every opted-in
// session, including one started before a device becomes available.
func hardwareSession(_ *Job, profile Profile) bool {
	return strings.EqualFold(profile.HWAccel, string(HWV4L2M2M))
}

// limitedBuffer 是一个有容量上限的 io.Writer，超出 maxSize 后静默丢弃。
// 用于捕获 ffmpeg stderr，防止异常文件导致无限内存增长。
type limitedBuffer struct {
	data    []byte
	maxSize int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remain := b.maxSize - len(b.data)
	if remain <= 0 {
		return len(p), nil // 静默丢弃
	}
	if len(p) > remain {
		p = p[:remain]
	}
	b.data = append(b.data, p...)
	return n, nil
}

func (b *limitedBuffer) String() string {
	return string(b.data)
}

// LocalRunner 本地内置 worker，直接调用本机 ffmpeg
type LocalRunner struct {
	mgr      *Manager
	workerID string

	stopCh chan struct{}
	wg     sync.WaitGroup

	// 运行中 job 的 cancel 管理：jobID -> cancelFunc
	runMu       sync.Mutex
	runningJobs map[string]context.CancelFunc
}

func NewLocalRunner(mgr *Manager) *LocalRunner {
	return &LocalRunner{
		mgr:         mgr,
		stopCh:      make(chan struct{}),
		runningJobs: make(map[string]context.CancelFunc),
	}
}

// Start 注册本地 worker 并启动消费协程
func (r *LocalRunner) Start() error {
	concurrency := setting.GetInt(conf.TranscodeLocalConcurrency, 1)
	if concurrency <= 0 {
		concurrency = 1
	}
	hwaccel := setting.GetStr(conf.TranscodeHWAccel, "none")
	hostname, _ := os.Hostname()
	w := r.mgr.Registry.Register(&RegisterRequest{
		Name:          "local-" + hostname,
		Version:       "embedded",
		Capacity:      concurrency,
		HWAccel:       []string{hwaccel},
		CodecsDecode:  []string{"h264", "hevc", "av1", "vp9", "mpeg4", "mpeg2video"},
		CodecsEncode:  []string{"h264", "hevc"},
		MaxResolution: "3840x2160",
		Tags:          []string{"local", "embedded"},
	}, true)
	r.workerID = w.ID
	utils.Log.Infof("[transcode] local worker started: id=%s capacity=%d hwaccel=%s", w.ID, concurrency, hwaccel)
	if strings.EqualFold(hwaccel, string(HWV4L2M2M)) {
		if caps := detectV4L2M2M(setting.GetStr(conf.TranscodeFFmpegPath, "ffmpeg")); caps.Empty() {
			utils.Log.Warnf("[transcode] v4l2m2m: no compatible device and FFmpeg codec pair found, using CPU codecs (check FFmpeg wrappers and container device access)")
		} else {
			utils.Log.Infof("[transcode] v4l2m2m: %s", caps)
		}
	}

	for i := 0; i < concurrency; i++ {
		r.wg.Add(1)
		go r.loop()
	}
	return nil
}

func (r *LocalRunner) Stop() {
	close(r.stopCh)
	// 取消所有运行中的 FFmpeg 进程
	r.runMu.Lock()
	for id, cancel := range r.runningJobs {
		utils.Log.Infof("[transcode] stopping ffmpeg for job %s (runner shutdown)", id)
		cancel()
	}
	r.runMu.Unlock()
	r.wg.Wait()
	r.mgr.Registry.Unregister(r.workerID)
}

// CancelJob 外部取消指定 job 的 FFmpeg 进程（由 Scheduler.Cancel / 空闲超时调用）
func (r *LocalRunner) CancelJob(jobID string) bool {
	r.runMu.Lock()
	cancel, ok := r.runningJobs[jobID]
	r.runMu.Unlock()
	if ok {
		utils.Log.Infof("[transcode] killing ffmpeg for job %s (external cancel)", jobID)
		cancel()
	}
	return ok
}

// RunningJobIDs 返回当前正在运行的所有 job ID 列表
func (r *LocalRunner) RunningJobIDs() []string {
	r.runMu.Lock()
	defer r.runMu.Unlock()
	ids := make([]string, 0, len(r.runningJobs))
	for id := range r.runningJobs {
		ids = append(ids, id)
	}
	return ids
}

func (r *LocalRunner) loop() {
	defer r.wg.Done()
	for {
		select {
		case <-r.stopCh:
			return
		default:
		}
		jobs := r.mgr.Scheduler.Claim(r.workerID, 1, 5*time.Second)
		if len(jobs) == 0 {
			continue
		}
		for _, j := range jobs {
			r.runJob(j)
		}
	}
}

// runJob 执行一个转码任务（单 profile，HLS 输出，边转边写入 cache）
func (r *LocalRunner) runJob(job *Job) {
	utils.Log.Infof("[transcode] local runner picked job %s (%s)", job.ID, job.Path)
	if len(job.Profiles) == 0 {
		_ = r.mgr.Scheduler.Finish(&FinishRequest{JobID: job.ID, Status: JobFailed, Error: "no profiles"})
		return
	}
	profile := job.Profiles[0]
	job.mu.RLock()
	prepared := job.EffectiveProfile != nil
	if prepared {
		profile = *job.EffectiveProfile
	}
	job.mu.RUnlock()
	timeoutMin := setting.GetInt(conf.TranscodeJobTimeoutMin, 120)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutMin)*time.Minute)
	defer cancel()

	// 注册到 runningJobs，支持外部取消
	r.runMu.Lock()
	r.runningJobs[job.ID] = cancel
	r.runMu.Unlock()
	defer func() {
		r.runMu.Lock()
		delete(r.runningJobs, job.ID)
		r.runMu.Unlock()
	}()

	// 输出目录
	jc, err := r.mgr.Cache.getOrCreate(job.ID, profile.Name)
	if err != nil {
		_ = r.mgr.Scheduler.Finish(&FinishRequest{JobID: job.ID, Status: JobFailed, Error: err.Error()})
		return
	}
	outDir := jc.BaseDir
	ffmpegPath := setting.GetStr(conf.TranscodeFFmpegPath, "ffmpeg")
	var caps v4l2m2m.Caps
	if hardwareSession(job, profile) {
		caps = detectV4L2M2M(ffmpegPath)
	}
	if !prepared && profile.RatePolicy == "downscale" && hardwareSession(job, profile) {
		if r.mgr.Chunks == nil {
			_ = r.mgr.Scheduler.Finish(&FinishRequest{JobID: job.ID, Status: JobFailed, Error: "rate probe requires a device scheduler"})
			return
		}
		if !r.mgr.Chunks.acquireDevice(ctx) {
			_ = r.mgr.Scheduler.Finish(&FinishRequest{JobID: job.ID, Status: JobCancelled, Error: "cancelled before rate probe"})
			return
		}
	}
	selected := profile
	var policyErr error
	if !prepared {
		selected, policyErr = ffmpegpolicy.Prepare(ctx, job.SourceURL, job.Probe, profile, ffmpegPath, caps, runProcess)
	}
	if !prepared && profile.RatePolicy == "downscale" && hardwareSession(job, profile) {
		r.mgr.Chunks.releaseDevice()
	}
	if policyErr != nil {
		status := JobFailed
		if ctx.Err() != nil {
			status = JobCancelled
		}
		_ = r.mgr.Scheduler.Finish(&FinishRequest{JobID: job.ID, Status: status, Error: policyErr.Error()})
		r.mgr.Cache.MarkFinal(job.ID, profile.Name)
		return
	}
	job.mu.Lock()
	job.EffectiveProfile = &selected
	if selected.Scale != profile.Scale {
		job.Warning = fmt.Sprintf("rate budget selected output scale %s before playback", selected.Scale)
	}
	job.mu.Unlock()
	profile = selected

	// 【智能调度】如果探测到了视频总时长，启用 chunk 模式（按需分段并发转码）
	// 否则回退到旧的"一次性整段"模式（兼容 ffprobe 失败的场景）
	if job.Probe.Duration > 0 && r.mgr.Chunks != nil {
		r.runJobByChunks(ctx, job, profile, outDir)
		return
	}

	// === 旧路径：一次性整段转码 ===
	r.runJobLegacy(ctx, job, profile, outDir)
}

// runJobByChunks 智能 chunk 调度模式：按需启动 ffmpeg 进程，支持随机访问
func (r *LocalRunner) runJobByChunks(ctx context.Context, job *Job, profile Profile, outDir string) {
	startedAt := time.Now()

	// 启动 watchSegments：周期扫描目录把切片登记到 cache
	stopWatcher := make(chan struct{})
	var watcherWG sync.WaitGroup
	watcherWG.Add(1)
	go func() {
		defer watcherWG.Done()
		r.watchSegments(job, profile, outDir, stopWatcher)
	}()

	// 创建 chunk session（会立刻启动 chunk-0 + 后台维护协程）
	session, err := r.mgr.Chunks.StartSession(job, profile, outDir)
	if err != nil {
		close(stopWatcher)
		watcherWG.Wait()
		_ = r.mgr.Scheduler.Finish(&FinishRequest{JobID: job.ID, Status: JobFailed, Error: err.Error()})
		r.mgr.Cache.MarkFinal(job.ID, profile.Name)
		return
	}

	// 阻塞等待全部 chunk 完成 / 任务被取消
	select {
	case <-session.WaitDone():
		// 所有 chunk 都 Done，正常完成
	case <-ctx.Done():
		// 外部取消：通知 chunk session 杀掉所有 ffmpeg
		r.mgr.Chunks.CancelSession(job.ID, profile.Name)
	}

	close(stopWatcher)
	watcherWG.Wait()
	if sessionErr := session.Err(); sessionErr != nil {
		r.mgr.Chunks.CancelSession(job.ID, profile.Name)
		_ = r.mgr.Scheduler.Finish(&FinishRequest{JobID: job.ID, Status: JobFailed, Error: sessionErr.Error()})
		r.mgr.Cache.MarkFinal(job.ID, profile.Name)
		return
	}
	r.scanAndPublish(job, profile, outDir, true)

	if ctx.Err() != nil {
		utils.Log.Infof("[transcode] job %s chunk session cancelled", job.ID)
		_ = r.mgr.Scheduler.Finish(&FinishRequest{
			JobID:  job.ID,
			Status: JobCancelled,
			Error:  "cancelled: " + ctx.Err().Error(),
		})
		return
	}
	utils.Log.Infof("[transcode] job %s completed in %.1fs (chunk mode)", job.ID, time.Since(startedAt).Seconds())
	_ = r.mgr.Scheduler.Finish(&FinishRequest{
		JobID:  job.ID,
		Status: JobFinished,
		Stats:  JobStats{Elapsed: time.Since(startedAt).Seconds()},
	})
}

// runJobLegacy 旧路径：一次性整段转码（不知道总时长时回退使用）
func (r *LocalRunner) runJobLegacy(ctx context.Context, job *Job, profile Profile, outDir string) {
	if hardwareSession(job, profile) {
		if !r.mgr.Chunks.acquireDevice(ctx) {
			_ = r.mgr.Scheduler.Finish(&FinishRequest{JobID: job.ID, Status: JobCancelled, Error: "cancelled while waiting for codec device"})
			return
		}
		defer r.mgr.Chunks.releaseDevice()
	}
	playlistPath := filepath.Join(outDir, "playlist.m3u8")
	segPattern := filepath.Join(outDir, "seg-%d.ts")

	ffmpegPath := setting.GetStr(conf.TranscodeFFmpegPath, "ffmpeg")
	args, err := buildFFmpegArgs(job, profile, segPattern, playlistPath, ffmpegPath)
	if err != nil {
		_ = r.mgr.Scheduler.Finish(&FinishRequest{JobID: job.ID, Status: JobFailed, Error: err.Error()})
		r.mgr.Cache.MarkFinal(job.ID, profile.Name)
		return
	}

	stderr := &limitedBuffer{maxSize: 64 * 1024}

	startedAt := time.Now()

	stopWatcher := make(chan struct{})
	var watcherWG sync.WaitGroup
	watcherWG.Add(1)
	go func() {
		defer watcherWG.Done()
		r.watchSegments(job, profile, outDir, stopWatcher)
	}()

	err = runProcess(ctx, ffmpegPath, args, stderr)
	close(stopWatcher)
	watcherWG.Wait()
	if err == nil || ctx.Err() != nil {
		r.scanAndPublish(job, profile, outDir, true)
	}

	if err != nil {
		if ctx.Err() != nil {
			utils.Log.Infof("[transcode] job %s ffmpeg killed (context cancelled)", job.ID)
			_ = r.mgr.Scheduler.Finish(&FinishRequest{
				JobID:  job.ID,
				Status: JobCancelled,
				Error:  "cancelled: " + ctx.Err().Error(),
			})
		} else {
			_ = r.mgr.Scheduler.Finish(&FinishRequest{
				JobID:  job.ID,
				Status: JobFailed,
				Error:  fmt.Sprintf("ffmpeg failed: %v, stderr=%s", err, truncate(stderr.String(), 1024)),
			})
		}
		r.mgr.Cache.MarkFinal(job.ID, profile.Name)
		return
	}
	utils.Log.Infof("[transcode] job %s completed in %.1fs (legacy mode)", job.ID, time.Since(startedAt).Seconds())
	_ = r.mgr.Scheduler.Finish(&FinishRequest{
		JobID:  job.ID,
		Status: JobFinished,
		Stats:  JobStats{Elapsed: time.Since(startedAt).Seconds()},
	})
}

// watchSegments 周期性扫描输出目录，把新切片登记到 Cache
// 【内存优化】把扫描频率从 500ms 降到 2s，减少每次 os.ReadDir 和 parseFFmpegPlaylistDurations
// 触发的字符串/字节切片分配。chunk 模式下每个 chunk 完成时还会主动调用 publishChunkSegments，
// 所以这里只需要兜底登记中间切片，2s 频率完全够用。
func (r *LocalRunner) watchSegments(job *Job, profile Profile, dir string, stop chan struct{}) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			r.scanAndPublish(job, profile, dir, false)
		}
	}
}

func (r *LocalRunner) scanAndPublish(job *Job, profile Profile, dir string, finalScan bool) {
	jc, err := r.mgr.Cache.getOrCreate(job.ID, profile.Name)
	if err != nil {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type segFile struct {
		seq  int
		path string
		size int64
	}
	var segs []segFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "seg-") || !strings.HasSuffix(name, ".ts") {
			continue
		}
		numStr := strings.TrimSuffix(strings.TrimPrefix(name, "seg-"), ".ts")
		seq, err := strconv.Atoi(numStr)
		if err != nil {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		segs = append(segs, segFile{seq: seq, path: filepath.Join(dir, name), size: fi.Size()})
	}
	sort.Slice(segs, func(i, j int) bool { return segs[i].seq < segs[j].seq })

	// 【优化】解析 ffmpeg 写出的 playlist.m3u8 获取每个切片的真实时长
	// 避免硬编码 segDur 导致 HLS.js 显示总时长不准确
	realDurations := make(map[int]float64)
	for seq, duration := range parseFFmpegPlaylistDurations(filepath.Join(dir, "playlist.m3u8")) {
		realDurations[seq] = duration
	}
	// Chunk playlists are atomically written too; preserve their real durations.
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "_chunk_") && strings.HasSuffix(entry.Name(), ".m3u8") {
			for seq, duration := range parseFFmpegPlaylistDurations(filepath.Join(dir, entry.Name())) {
				realDurations[seq] = duration
			}
		}
	}

	// ffmpeg 在写入 .ts 时是边写边写,稳妥起见仅登记除最后一片以外的切片(最后一片可能未写完)
	limit := len(segs)
	if !finalScan && limit > 0 {
		limit--
	}
	defaultDur := float64(job.Output.SegmentDuration)
	if defaultDur <= 0 {
		defaultDur = 6
	}
	for i := 0; i < limit; i++ {
		s := segs[i]
		jc.mu.RLock()
		_, exists := jc.segments[s.seq]
		jc.mu.RUnlock()
		if exists {
			continue
		}
		// 优先用 ffmpeg playlist 中的真实时长
		dur := defaultDur
		if d, ok := realDurations[s.seq]; ok && d > 0 {
			dur = d
		}
		// 直接登记，不复制（已经在 cache 目录里了）
		jc.publishSegment(&SegmentInfo{Seq: s.seq, Duration: dur, Size: s.size, Path: s.path}, false)
		r.reportBitrate(job, profile)
		// 第一片就绪 → 通知 job ready
		if s.seq == 0 {
			job.MarkReady()
		}
	}
	if finalScan {
		jc.mu.Lock()
		jc.final = true
		jc.mu.Unlock()
	}
}

// publishChunkSegments 把 [startSeq, endSeq) 范围内已经写完的切片登记到 cache。
// 与 scanAndPublish 不同，这个方法专门用于"某个 chunk 的 ffmpeg 已经退出"的场景，
// 此时该范围的所有 .ts 文件都是完整的，不能再丢掉最大序号的那片（避免边界切片永远丢失）。
func (r *LocalRunner) publishChunkSegments(job *Job, profile Profile, dir string, startSeq, endSeq int, playlistPath string) {
	jc, err := r.mgr.Cache.getOrCreate(job.ID, profile.Name)
	if err != nil {
		return
	}
	defaultDur := float64(job.Output.SegmentDuration)
	if defaultDur <= 0 {
		defaultDur = 6
	}
	durations := parseFFmpegPlaylistDurations(playlistPath)
	registered := 0
	for seq := startSeq; seq < endSeq; seq++ {
		jc.mu.RLock()
		_, exists := jc.segments[seq]
		jc.mu.RUnlock()
		if exists && durations[seq] == 0 {
			continue
		}
		fpath := filepath.Join(dir, fmt.Sprintf("seg-%d.ts", seq))
		fi, err := os.Stat(fpath)
		if err != nil || fi.IsDir() || fi.Size() == 0 {
			continue
		}
		duration := defaultDur
		if durations[seq] > 0 {
			duration = durations[seq]
		}
		jc.publishSegment(&SegmentInfo{Seq: seq, Duration: duration, Size: fi.Size(), Path: fpath}, false)
		r.reportBitrate(job, profile)
		if seq == 0 {
			job.MarkReady()
		}
		registered++
	}
	if registered > 0 {
		utils.Log.Infof("[transcode][chunk] published %d segments [%d, %d) for job=%s",
			registered, startSeq, endSeq, job.ID)
	}
}

func (r *LocalRunner) reportBitrate(job *Job, profile Profile) {
	target, err := parseBitrate(profile.VideoBitrate)
	if err != nil || target <= 0 || !strings.EqualFold(profile.HWAccel, string(HWV4L2M2M)) ||
		r.mgr.Cache.SegmentCount(job.ID, profile.Name) < 3 {
		return
	}
	audio, _ := parseBitrate(profile.AudioBitrate)
	_, actual := r.mgr.Cache.profileBandwidth(job.ID, profile.Name)
	// The measurement includes audio and TS overhead; avoid startup-only warnings.
	if actual > (target+audio)*5/4 {
		warning := fmt.Sprintf("requested video bitrate %d bps; measured HLS bandwidth %d bps, reduce output size/frame rate or use software encoding if bandwidth is limited", target, actual)
		job.mu.Lock()
		first := job.Warning == ""
		job.Warning = warning
		job.mu.Unlock()
		if first {
			utils.Log.Warnf("[transcode] %s", warning)
		}
	}
}

// playlistDurCache 缓存 m3u8 解析结果，按文件 mtime+size 失效。
// 【内存优化】scanAndPublish 每 2 秒一次，每次都会解析同一个 m3u8（长视频几百 KB），
// strings.Split 会产生大量临时字符串切片造成 GC 压力。用缓存减少 99% 的重复解析。
type playlistDurCacheEntry struct {
	mtime time.Time
	size  int64
	durs  map[int]float64
}

var (
	playlistDurCache   = make(map[string]*playlistDurCacheEntry)
	playlistDurCacheMu sync.Mutex
)

// parseFFmpegPlaylistDurations 解析 ffmpeg 写出的 m3u8，返回 seq -> duration 的映射
// 用于获取每个切片的真实时长，避免后端用硬编码 segDur 导致总时长显示偏差
func parseFFmpegPlaylistDurations(path string) map[int]float64 {
	fi, err := os.Stat(path)
	if err != nil {
		return map[int]float64{}
	}
	// 命中缓存：mtime + size 都没变就直接复用
	playlistDurCacheMu.Lock()
	if e, ok := playlistDurCache[path]; ok && e.mtime.Equal(fi.ModTime()) && e.size == fi.Size() {
		playlistDurCacheMu.Unlock()
		return e.durs
	}
	playlistDurCacheMu.Unlock()

	out := make(map[int]float64)
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	out = ffmpegargs.SegmentDurations(string(data))
	// 写入缓存
	playlistDurCacheMu.Lock()
	playlistDurCache[path] = &playlistDurCacheEntry{
		mtime: fi.ModTime(),
		size:  fi.Size(),
		durs:  out,
	}
	playlistDurCacheMu.Unlock()
	return out
}

// buildFFmpegArgs 构造 ffmpeg 命令行（一次性整段转码，兼容旧路径）
// 当 Probe.Duration 不可用时，LocalRunner.runJob 会回退到这条路径。
func buildFFmpegArgs(job *Job, profile Profile, segPattern, playlistPath, ffmpegPath string) ([]string, error) {
	segDur := job.Output.SegmentDuration
	// startTime=0, duration=0(=不限) 表示整段转码；startSeq=0
	return buildFFmpegArgsCommon(job, profile, segDur, 0, 0, 0, segPattern, playlistPath, ffmpegPath)
}

// buildFFmpegChunkArgs 构造 chunk 模式的 ffmpeg 命令行
// startTime: 起始时间（秒）；duration: 持续时长（秒，0=不限）；startSeq: 切片起始编号
// chunk 模式与一次性整段共用底层参数构造，差异仅在 -ss/-t/-start_number。
func buildFFmpegChunkArgs(job *Job, profile Profile, segDur int, startTime, duration float64, startSeq int, segPattern, playlistPath, ffmpegPath string) ([]string, error) {
	return buildFFmpegArgsCommon(job, profile, segDur, startTime, duration, startSeq, segPattern, playlistPath, ffmpegPath)
}

// buildFFmpegArgsCommon supplies local settings and capabilities to the shared builder.
func buildFFmpegArgsCommon(job *Job, profile Profile, segDur int, startTime, duration float64, startSeq int, segPattern, playlistPath, ffmpegPath string) ([]string, error) {
	var caps v4l2m2m.Caps
	if strings.EqualFold(profile.HWAccel, string(HWV4L2M2M)) {
		caps = detectV4L2M2M(ffmpegPath)
	}
	return ffmpegargs.Build(job.SourceURL, job.Probe, profile, segDur, startTime, duration, startSeq, segPattern, playlistPath, caps)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}

// ProbeSource 调用 ffprobe 探测源文件
func ProbeSource(url string) (SourceProbe, error) {
	if !ValidSourceInput(url) {
		return SourceProbe{}, fmt.Errorf("unsupported probe source")
	}
	probePath := setting.GetStr(conf.TranscodeFFprobePath, "ffprobe")
	args := []string{
		"-v", "error",
		"-protocol_whitelist", ProtocolWhitelistFor(url),
		"-show_entries", "format=duration,size,bit_rate:stream=codec_type,codec_name,bit_rate,width,height,pix_fmt,avg_frame_rate,r_frame_rate",
		"-of", "json",
		url,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, probePath, args...)
	if runtime.GOOS == "windows" {
		// no special handling
	}
	out, err := cmd.Output()
	if err != nil {
		return SourceProbe{}, err
	}
	var raw struct {
		Format struct {
			Duration string `json:"duration"`
			Size     string `json:"size"`
			BitRate  string `json:"bit_rate"`
		} `json:"format"`
		Streams []struct {
			CodecType    string `json:"codec_type"`
			CodecName    string `json:"codec_name"`
			BitRate      string `json:"bit_rate"`
			Width        int    `json:"width"`
			Height       int    `json:"height"`
			PixFmt       string `json:"pix_fmt"`
			AvgFrameRate string `json:"avg_frame_rate"`
			FrameRate    string `json:"r_frame_rate"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return SourceProbe{}, err
	}
	p := SourceProbe{}
	p.Duration, _ = strconv.ParseFloat(raw.Format.Duration, 64)
	p.Size, _ = strconv.ParseInt(raw.Format.Size, 10, 64)
	for _, s := range raw.Streams {
		if s.CodecType == "video" && p.VideoCodec == "" {
			p.VideoCodec = s.CodecName
			p.Width = s.Width
			p.Height = s.Height
			p.PixFmt = s.PixFmt
			p.FrameRate = s.AvgFrameRate
			if p.FrameRate == "" || p.FrameRate == "0/0" {
				p.FrameRate = s.FrameRate
			}
			p.VideoBitrate, _ = strconv.ParseInt(s.BitRate, 10, 64)
		}
		if s.CodecType == "audio" && p.AudioCodec == "" {
			p.AudioCodec = s.CodecName
			p.AudioBitrate, _ = strconv.ParseInt(s.BitRate, 10, 64)
		}
	}
	return p, nil
}
