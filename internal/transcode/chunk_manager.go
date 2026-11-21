// Package transcode chunk_manager.go：智能分段转码调度器
//
// 设计目标：
//  1. 把视频按时间切成多个 chunk（默认每 60 秒一段），每个 chunk 独立用 ffmpeg 转码
//  2. 用户拖动到任意位置时，按需启动对应 chunk 的 ffmpeg，不再线性等待
//  3. 控制单 Job 最多并发 N 个 chunk ffmpeg，超出时 LRU 淘汰最久未访问的 chunk
//  4. 当前 chunk 接近末尾时自动预启动下一个 chunk，保证顺序播放无缝衔接
//  5. chunk 空闲超过阈值时 kill ffmpeg 释放 CPU，但保留已转出的切片文件
//
// 与 LocalRunner 的关系：
//   - LocalRunner.runJob 不再直接启动一个完整的 ffmpeg，而是创建 ChunkSession 并阻塞等待
//   - ChunkManager 是 ChunkSession 的"大脑"，负责调度
//   - TCSegment 收到切片请求时，调用 ChunkManager.EnsureChunkRunningForSeg(seq) 触发按需调度
package transcode

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/setting"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
)

// ChunkState chunk 当前状态
type ChunkState int

const (
	ChunkIdle    ChunkState = iota // 未启动
	ChunkRunning                   // ffmpeg 正在转码
	ChunkDone                      // ffmpeg 自然退出（chunk 内所有切片已转完）
	ChunkFailed                    // ffmpeg 异常失败
	ChunkKilled                    // 被 LRU 淘汰或空闲超时 kill
)

// Chunk 表示视频中的一个时间段，对应一段连续的切片序号
type Chunk struct {
	Index      int     // chunk 序号（0 开始）
	StartTime  float64 // 起始秒
	Duration   float64 // 持续时长（最后一个 chunk 可能不足 chunkDur）
	StartSeq   int     // 起始切片序号（包含）
	EndSeq     int     // 结束切片序号（不包含）
	State      ChunkState
	StartedAt  time.Time
	LastAccess time.Time
	Error      string

	cancel   context.CancelFunc // ffmpeg 进程的 cancel
	doneCh   chan struct{}      // ffmpeg 进程结束时关闭
	doneOnce sync.Once
	// gen 每次启动自增。被 kill 的 chunk 可以立刻被重新启动，旧进程
	// 退出得晚，只有和当前 gen 相同的进程才允许回写状态。
	gen int
}

// chunkDoneClose 关闭 doneCh（幂等）
func (c *Chunk) chunkDoneClose() {
	c.doneOnce.Do(func() {
		if c.doneCh != nil {
			close(c.doneCh)
		}
	})
}

// ChunkSession 单个 (jobID, profile) 下的 chunk 调度上下文
type ChunkSession struct {
	JobID   string
	Profile string
	Job     *Job
	ProfDef Profile

	// 静态参数
	TotalDuration float64 // 视频总时长
	SegDur        int     // 每片时长（秒）
	ChunkDur      int     // 每个 chunk 时长（秒）
	MaxConcurrent int     // 最大并发 chunk 数（硬件设备会话固定为 1）
	IdleSec       int     // chunk 空闲多久就 kill
	Prefetch      bool    // 是否启用预读

	// HW 为 true 时该会话选择 V4L2 M2M（设备不可用时可能回退软件）。为兼容独占设备，
	// FFmpeg 通过 ChunkManager.deviceSlot 串行，且
	// 预读只能是"上一段转完立即启动下一段"。
	HW         bool
	FFmpegPath string
	Timeout    time.Duration

	mu     sync.Mutex
	chunks map[int]*Chunk // chunkIndex -> Chunk

	// 给 LocalRunner 用：所有切片转完时关闭，让 runJob 能退出
	allDoneCh   chan struct{}
	allDoneOnce sync.Once

	// outDir / segPattern (供 ffmpeg 使用)
	OutDir string
}

// allDoneClose 关闭 allDoneCh（幂等）
func (s *ChunkSession) allDoneClose() {
	s.allDoneOnce.Do(func() {
		close(s.allDoneCh)
	})
}

// ChunkManager 全局 chunk 管理器（一个 Manager 一个）
type ChunkManager struct {
	mgr *Manager

	mu       sync.Mutex
	sessions map[string]*ChunkSession // key = jobID + ":" + profile

	// 独占编解码设备由整个 FFmpeg 生命周期持有；整段和分块路径
	// 共用此许可，排队中的请求可随 context 取消。
	deviceSlot chan struct{}

	stopCh chan struct{}
}

// NewChunkManager 创建实例
func NewChunkManager(mgr *Manager) *ChunkManager {
	return &ChunkManager{
		mgr:        mgr,
		sessions:   make(map[string]*ChunkSession),
		deviceSlot: make(chan struct{}, 1),
		stopCh:     make(chan struct{}),
	}
}

func (cm *ChunkManager) acquireDevice(ctx context.Context) bool {
	select {
	case cm.deviceSlot <- struct{}{}:
		if ctx.Err() == nil {
			return true
		}
		<-cm.deviceSlot
		return false
	case <-ctx.Done():
		return false
	}
}

func (cm *ChunkManager) releaseDevice() { <-cm.deviceSlot }

func sessionKey(jobID, profile string) string { return jobID + ":" + profile }

// StartSession 创建并启动一个 chunk session（由 LocalRunner.runJob 调用）
// 返回的 ChunkSession.allDoneCh 在所有切片都转完时关闭
func (cm *ChunkManager) StartSession(job *Job, profile Profile, outDir string) (*ChunkSession, error) {
	totalDur := job.Probe.Duration
	if totalDur <= 0 || math.IsNaN(totalDur) || math.IsInf(totalDur, 0) {
		return nil, fmt.Errorf("chunk session requires finite positive probe.duration")
	}
	segDur := setting.GetInt(conf.TranscodeSegmentDuration, 6)
	if segDur <= 0 {
		segDur = 6
	}
	chunkDur := setting.GetInt(conf.TranscodeChunkDurationSec, 60)
	if chunkDur < segDur {
		chunkDur = segDur * 10
	}
	// 让 chunkDur 是 segDur 的整数倍，保证切片对齐
	if chunkDur%segDur != 0 {
		chunkDur = (chunkDur / segDur) * segDur
		if chunkDur < segDur {
			chunkDur = segDur
		}
	}
	maxConc := setting.GetInt(conf.TranscodeMaxChunkConcurrency, 2)
	if maxConc <= 0 {
		maxConc = 2
	}
	idleSec := setting.GetInt(conf.TranscodeChunkIdleSec, 60)
	if idleSec <= 0 {
		idleSec = 60
	}
	prefetch := setting.GetBool(conf.TranscodeChunkPrefetch)
	hw := hardwareSession(job, profile)
	if hw {
		// 设备只有一路，多个 chunk 并发只会互相抢设备。
		maxConc = 1
	}
	timeoutMin := setting.GetInt(conf.TranscodeJobTimeoutMin, 120)
	if timeoutMin <= 0 {
		timeoutMin = 120
	}

	s := &ChunkSession{
		JobID:         job.ID,
		Profile:       profile.Name,
		Job:           job,
		ProfDef:       profile,
		TotalDuration: totalDur,
		SegDur:        segDur,
		ChunkDur:      chunkDur,
		MaxConcurrent: maxConc,
		IdleSec:       idleSec,
		Prefetch:      prefetch,
		HW:            hw,
		FFmpegPath:    setting.GetStr(conf.TranscodeFFmpegPath, "ffmpeg"),
		Timeout:       time.Duration(timeoutMin) * time.Minute,
		chunks:        make(map[int]*Chunk),
		allDoneCh:     make(chan struct{}),
		OutDir:        outDir,
	}

	// 预先按总时长生成所有 chunk 元数据（不启动）
	segsPerChunk := chunkDur / segDur
	segments := math.Ceil(totalDur / float64(segDur))
	if segments > maxSegmentSeq+1 {
		return nil, fmt.Errorf("chunk session exceeds segment limit")
	}
	totalSegs := int(segments)
	chunkCount := totalSegs / segsPerChunk
	if totalSegs%segsPerChunk > 0 {
		chunkCount++
	}
	for i := 0; i < chunkCount; i++ {
		startSeq := i * segsPerChunk
		endSeq := startSeq + segsPerChunk
		if endSeq > totalSegs {
			endSeq = totalSegs
		}
		startT := float64(startSeq * segDur)
		dur := float64((endSeq - startSeq) * segDur)
		// 最后一个 chunk 可能不足
		if i == chunkCount-1 {
			realDur := totalDur - startT
			if realDur > 0 && realDur < dur {
				dur = realDur
			}
		}
		s.chunks[i] = &Chunk{
			Index:     i,
			StartTime: startT,
			Duration:  dur,
			StartSeq:  startSeq,
			EndSeq:    endSeq,
			State:     ChunkIdle,
		}
	}

	cm.mu.Lock()
	cm.sessions[sessionKey(job.ID, profile.Name)] = s
	cm.mu.Unlock()

	// 立刻启动第一个 chunk（保证 playlist 首切片可用）
	cm.ensureChunk(s, 0)

	// 启动后台维护协程：空闲清理 + 完成检测
	go cm.sessionLoop(s)

	utils.Log.Infof("[transcode][chunk] session started job=%s profile=%s totalDur=%.1fs chunkDur=%ds chunks=%d maxConc=%d hw=%v",
		job.ID, profile.Name, totalDur, chunkDur, chunkCount, maxConc, hw)
	return s, nil
}

// EnsureChunkRunningForSeg 确保覆盖 seq 的 chunk 正在转码（由 TCSegment 触发）
// 返回所属 chunk index；如果 seq 越界返回 -1
func (cm *ChunkManager) EnsureChunkRunningForSeg(jobID, profile string, seq int) int {
	cm.mu.Lock()
	s, ok := cm.sessions[sessionKey(jobID, profile)]
	cm.mu.Unlock()
	if !ok {
		utils.Log.Warnf("[transcode][chunk] EnsureChunk: session not found job=%s profile=%s seq=%d", jobID, profile, seq)
		return -1
	}
	idx := cm.findChunkBySeq(s, seq)
	if idx < 0 {
		utils.Log.Warnf("[transcode][chunk] EnsureChunk: seq=%d out of range job=%s", seq, jobID)
		return -1
	}
	cm.ensureChunk(s, idx)
	return idx
}

// findChunkBySeq 按切片序号找到所属 chunk
func (cm *ChunkManager) findChunkBySeq(s *ChunkSession, seq int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	for idx, c := range s.chunks {
		if seq >= c.StartSeq && seq < c.EndSeq {
			return idx
		}
	}
	return -1
}

// ensureChunk 确保某个 chunk 处于 Running 状态（已运行/已完成则跳过）
// 同时受 MaxConcurrent 限制，超出时 LRU 淘汰最久未访问的 chunk
func (cm *ChunkManager) ensureChunk(s *ChunkSession, idx int) {
	s.mu.Lock()
	c, ok := s.chunks[idx]
	if !ok {
		s.mu.Unlock()
		utils.Log.Warnf("[transcode][chunk] ensureChunk: chunk %d not in session (job=%s)", idx, s.JobID)
		return
	}
	now := time.Now()
	c.LastAccess = now
	prevState := c.State
	switch c.State {
	case ChunkRunning:
		s.mu.Unlock()
		utils.Log.Debugf("[transcode][chunk] ensureChunk: chunk %d already running (job=%s)", idx, s.JobID)
		return
	case ChunkDone:
		s.mu.Unlock()
		utils.Log.Debugf("[transcode][chunk] ensureChunk: chunk %d already done (job=%s)", idx, s.JobID)
		return
	}
	// 【关键修复】ChunkFailed 也允许重启：因为可能是网络抖动、source URL 临时失效等
	// 而不是永久性故障；如果连续失败，runChunk 内部不会陷入死循环（每次都会重新尝试源）
	// ChunkIdle / ChunkKilled / ChunkFailed → 需要启动
	running := 0
	var lruIdx = -1
	var lruTime = now
	for i, ck := range s.chunks {
		if ck.State == ChunkRunning {
			running++
			if i != idx && ck.LastAccess.Before(lruTime) {
				lruTime = ck.LastAccess
				lruIdx = i
			}
		}
	}
	if running >= s.MaxConcurrent && lruIdx >= 0 {
		victim := s.chunks[lruIdx]
		utils.Log.Infof("[transcode][chunk] LRU evict chunk %d (job=%s) to make room for chunk %d",
			lruIdx, s.JobID, idx)
		if victim.cancel != nil {
			victim.cancel()
		}
		victim.State = ChunkKilled
	}
	// 启动 chunk ffmpeg。ctx 在这里创建而不是在 runChunk 里，这样
	// 状态变为 Running 的同时 cancel 已就绪，LRU/空闲 kill 不会扑空。
	ctx, cancel := context.WithTimeout(context.Background(), s.Timeout)
	c.gen++
	gen := c.gen
	c.State = ChunkRunning
	c.StartedAt = now
	c.cancel = cancel
	c.doneCh = make(chan struct{})
	c.doneOnce = sync.Once{}
	s.mu.Unlock()

	utils.Log.Infof("[transcode][chunk] ensureChunk: launching chunk %d (job=%s prevState=%v running=%d)",
		idx, s.JobID, prevState, running)
	go cm.runChunk(ctx, cancel, s, idx, gen)
}

// runProcess 执行一次 ffmpeg。测试替换它来观察调度顺序。
var runProcess = func(ctx context.Context, path string, args []string, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stderr = stderr
	return cmd.Run()
}

// runChunk 实际启动 ffmpeg 转码该 chunk
func (cm *ChunkManager) runChunk(ctx context.Context, cancel context.CancelFunc, s *ChunkSession, idx, gen int) {
	defer cancel()

	if s.HW {
		// 设备同一时刻只给一个进程。这里阻塞到前一个硬件 ffmpeg 退出，
		// 拖动抢占时新 chunk 就是靠这行等旧进程让出 /dev/video*。
		if cm.acquireDevice(ctx) {
			defer cm.releaseDevice()
		}
	}

	s.mu.Lock()
	c := s.chunks[idx]
	s.mu.Unlock()
	if c == nil {
		return
	}

	if ctx.Err() != nil {
		// 排队等设备期间被抢占，不必再启动 ffmpeg
		s.mu.Lock()
		if c.gen == gen {
			c.State = ChunkKilled
			c.chunkDoneClose()
		}
		s.mu.Unlock()
		return
	}

	segPattern := filepath.Join(s.OutDir, "seg-%d.ts")
	// 每个 chunk 写自己的 playlist 文件，防止互相覆盖；后端不读这个 playlist 用于响应，
	// 真正的总 playlist 由 cache.BuildPlaylist 根据总时长生成。
	chunkPlaylist := filepath.Join(s.OutDir, fmt.Sprintf("_chunk_%d.m3u8", idx))

	args := buildFFmpegChunkArgs(s.Job, s.ProfDef, s.SegDur, c.StartTime, c.Duration, c.StartSeq, segPattern, chunkPlaylist, s.FFmpegPath)
	stderr := &limitedBuffer{maxSize: 32 * 1024}

	utils.Log.Infof("[transcode][chunk] start ffmpeg job=%s chunk=%d ss=%.1f t=%.1f startSeq=%d",
		s.JobID, idx, c.StartTime, c.Duration, c.StartSeq)

	startedAt := time.Now()
	err := runProcess(ctx, s.FFmpegPath, args, stderr)
	elapsed := time.Since(startedAt)

	s.mu.Lock()
	if c.gen != gen {
		// 该 chunk 已被重新启动，状态属于新进程，这里只收尾
		s.mu.Unlock()
		utils.Log.Infof("[transcode][chunk] chunk %d superseded (job=%s elapsed=%.1fs)", idx, s.JobID, elapsed.Seconds())
		return
	}
	switch {
	case ctx.Err() != nil && c.State == ChunkKilled:
		// 被 LRU/空闲/拖动主动 kill，状态保持 Killed
		utils.Log.Infof("[transcode][chunk] chunk %d killed (job=%s elapsed=%.1fs)", idx, s.JobID, elapsed.Seconds())
	case err != nil:
		c.State = ChunkFailed
		c.Error = truncate(stderr.String(), 512)
		utils.Log.Errorf("[transcode][chunk] chunk %d failed (job=%s elapsed=%.1fs): %v stderr=%s",
			idx, s.JobID, elapsed.Seconds(), err, c.Error)
	default:
		c.State = ChunkDone
		utils.Log.Infof("[transcode][chunk] chunk %d done (job=%s elapsed=%.1fs)", idx, s.JobID, elapsed.Seconds())
	}
	c.chunkDoneClose()
	s.mu.Unlock()

	// 【关键修复】chunk 自然完成时，必须用 finalScan-style 扫描，否则边界切片（最大 seq）
	// 永远不会被登记到 cache，导致下次播放/拖动到该位置时 60 秒超时
	if cm.mgr != nil && cm.mgr.localRunner != nil {
		cm.mgr.localRunner.publishChunkSegments(s.Job, s.ProfDef, s.OutDir, c.StartSeq, c.EndSeq, chunkPlaylist)
	}
	_ = os.Remove(chunkPlaylist)
}

// sessionLoop 每个 session 的后台维护：空闲清理 + 全部完成检测 + 预读
func (cm *ChunkManager) sessionLoop(s *ChunkSession) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-cm.stopCh:
			cm.killAllChunks(s)
			return
		case <-s.allDoneCh:
			return
		case <-t.C:
			cm.maintainSession(s)
		}
	}
}

// maintainSession 一次维护：检查空闲、预读下一 chunk、判断是否全部完成
func (cm *ChunkManager) maintainSession(s *ChunkSession) {
	now := time.Now()
	idleThreshold := time.Duration(s.IdleSec) * time.Second

	s.mu.Lock()
	// 1. 空闲清理：Running 状态但长时间未访问 → kill
	for _, c := range s.chunks {
		if c.State == ChunkRunning && now.Sub(c.LastAccess) > idleThreshold {
			utils.Log.Infof("[transcode][chunk] chunk %d idle for %.0fs, killing (job=%s)",
				c.Index, now.Sub(c.LastAccess).Seconds(), s.JobID)
			if c.cancel != nil {
				c.cancel()
			}
			c.State = ChunkKilled
		}
	}
	// 2. 预读
	//    硬件会话：设备只有一路，并发没有意义，改成"当前段转完立即启动下一段"。
	//    软件会话：最近被访问的 Running chunk 进度过半时提前启动下一个。
	var prefetchTarget = -1
	if s.Prefetch {
		if s.HW {
			// 最近访问的 chunk 就是播放位置；它还没转完（或正被拖动抢占）时不预读
			var latest *Chunk
			for _, c := range s.chunks {
				if c.State == ChunkIdle {
					continue
				}
				if latest == nil || c.LastAccess.After(latest.LastAccess) {
					latest = c
				}
			}
			if latest != nil && latest.State == ChunkDone {
				if nc, ok := s.chunks[latest.Index+1]; ok && nc.State == ChunkIdle {
					prefetchTarget = nc.Index
				}
			}
		} else {
			var hottestIdx = -1
			var hottestAccess time.Time
			for i, c := range s.chunks {
				if c.State == ChunkRunning && c.LastAccess.After(hottestAccess) {
					hottestAccess = c.LastAccess
					hottestIdx = i
				}
			}
			if hottestIdx >= 0 {
				hot := s.chunks[hottestIdx]
				// 估算进度：用 ffmpeg 已经写出的切片数 / 该 chunk 总切片数
				// 由于 cache.segments 是全局的，我们检查范围 [StartSeq, EndSeq) 中已就绪数量
				done := cm.countReadySegments(s, hot.StartSeq, hot.EndSeq)
				expected := hot.EndSeq - hot.StartSeq
				if expected > 0 && done*2 >= expected { // 进度 >= 50%
					next := hottestIdx + 1
					if nc, ok := s.chunks[next]; ok && nc.State == ChunkIdle {
						prefetchTarget = next
					}
				}
			}
		}
	}
	// 3. 全部完成检测：所有 chunk 都 Done
	allDone := true
	for _, c := range s.chunks {
		if c.State != ChunkDone {
			allDone = false
			break
		}
	}
	s.mu.Unlock()

	if prefetchTarget >= 0 {
		utils.Log.Infof("[transcode][chunk] prefetch chunk %d (job=%s)", prefetchTarget, s.JobID)
		cm.ensureChunk(s, prefetchTarget)
	}

	if allDone {
		s.allDoneClose()
	}
}

// countReadySegments 统计 [start, end) 范围内已就绪的切片数
func (cm *ChunkManager) countReadySegments(s *ChunkSession, start, end int) int {
	if cm.mgr == nil || cm.mgr.Cache == nil {
		return 0
	}
	cnt := 0
	for seq := start; seq < end; seq++ {
		if _, err := cm.mgr.Cache.GetSegment(s.JobID, s.Profile, seq); err == nil {
			cnt++
		}
	}
	return cnt
}

// killAllChunks 关闭某 session 下所有运行中的 ffmpeg
func (cm *ChunkManager) killAllChunks(s *ChunkSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.chunks {
		if c.State == ChunkRunning && c.cancel != nil {
			c.cancel()
			c.State = ChunkKilled
		}
	}
}

// CancelSession 外部取消（job 被 cancel 或 idle 超时）
func (cm *ChunkManager) CancelSession(jobID, profile string) {
	cm.mu.Lock()
	s, ok := cm.sessions[sessionKey(jobID, profile)]
	if ok {
		delete(cm.sessions, sessionKey(jobID, profile))
	}
	cm.mu.Unlock()
	if !ok {
		return
	}
	cm.killAllChunks(s)
	s.allDoneClose()
}

// CancelAllForJob 取消该 job 下所有 profile 的 session（CancelJob 时调用）
func (cm *ChunkManager) CancelAllForJob(jobID string) {
	cm.mu.Lock()
	var targets []*ChunkSession
	for k, s := range cm.sessions {
		if s.JobID == jobID {
			targets = append(targets, s)
			delete(cm.sessions, k)
		}
	}
	cm.mu.Unlock()
	for _, s := range targets {
		cm.killAllChunks(s)
		s.allDoneClose()
	}
}

// WaitDone 阻塞等待所有 chunk 完成（LocalRunner.runJob 用）
func (s *ChunkSession) WaitDone() <-chan struct{} { return s.allDoneCh }

// Stop 停止 ChunkManager（Manager.Stop 调用）
func (cm *ChunkManager) Stop() {
	close(cm.stopCh)
	cm.mu.Lock()
	defer cm.mu.Unlock()
	for _, s := range cm.sessions {
		cm.killAllChunks(s)
		s.allDoneClose()
	}
	cm.sessions = nil
}
