// Package ffmpegargs constructs HLS commands shared by local and remote workers.
package ffmpegargs

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/internal/transcode/v4l2m2m"
)

// SourceProbe 源文件探测信息（由 ffprobe 得出）
type SourceProbe struct {
	Duration     float64 `json:"duration"`      // 秒
	Size         int64   `json:"size"`          // 字节
	VideoCodec   string  `json:"video_codec"`   // h264 / hevc / av1 / vp9 ...
	VideoBitrate int64   `json:"video_bitrate"` // bps
	Width        int     `json:"width"`
	Height       int     `json:"height"`
	PixFmt       string  `json:"pix_fmt"` // yuv420p / yuv420p10le ...
	AudioCodec   string  `json:"audio_codec"`
	AudioBitrate int64   `json:"audio_bitrate"`
}

// Profile 输出转码档位
type Profile struct {
	Name         string `json:"name"`          // 例如 1080p
	VideoCodec   string `json:"video_codec"`   // h264 / hevc / av1
	VideoBitrate string `json:"video_bitrate"` // 例如 4000k
	Scale        string `json:"scale"`         // 例如 1920:-2
	AudioCodec   string `json:"audio_codec"`   // aac / mp3 / opus / copy
	AudioBitrate string `json:"audio_bitrate"` // 例如 160k
	HWAccel      string `json:"hwaccel"`       // worker 端如何选择硬件加速
}

// Build constructs one HLS process without reading settings or probing devices.
func Build(source string, probe SourceProbe, profile Profile, segDur int, startTime, duration float64, startSeq int, segPattern, playlistPath string, v4l2 v4l2m2m.Caps) []string {
	hw := strings.ToLower(profile.HWAccel)
	args := []string{"-y", "-loglevel", "warning"}

	// 输入侧硬件加速
	switch hw {
	case "nvenc":
		args = append(args, "-hwaccel", "cuda")
	case "qsv":
		args = append(args, "-hwaccel", "qsv")
	case "vaapi":
		args = append(args, "-hwaccel", "vaapi", "-hwaccel_output_format", "vaapi", "-vaapi_device", "/dev/dri/renderD128")
	case "amf":
		args = append(args, "-hwaccel", "d3d11va")
	case "videotoolbox":
		args = append(args, "-hwaccel", "videotoolbox")
	case "auto":
		args = append(args, "-hwaccel", "auto")
	}

	// V4L2 M2M 解码器不属于 -hwaccel，要在 -i 之前按源编码点名；设备不支持该源时软解
	decoder := v4l2.Decoder(probe.VideoCodec, probe.PixFmt, probe.Width, probe.Height)
	// 硬解的 chunk 保留源时间戳，原因见下
	copyTS := decoder != "" && startTime > 0.001

	// 【chunk 模式】input seek：放在 -i 之前，速度快，会按关键帧对齐
	// 注意 -ss 在 -i 之前是 input seek（用容器索引快速跳转），但落点是关键帧；
	// 我们通过 force_key_frames 保证每个 chunk 边界都有关键帧，所以这里精度足够。
	if startTime > 0.001 {
		args = append(args, "-ss", fmt.Sprintf("%.3f", startTime))
	}
	// 默认 input seek 把落点记为 0，落点前那几帧的 pts 是负数。FFmpeg 把负 pts
	// 交给 V4L2 时拆出负的微秒，内核按无符号换算成纳秒，这些帧的时间戳因此
	// 往后错 2^32 纳秒（32 位内核）或 2^32 微秒（64 位内核），trim 丢不掉，
	// 输出时间戳倒退，muxer 等音频时扣住编码器的全部输出缓冲，转码卡死。
	// -copyts 让解码器只收到非负的源时间戳；-start_at_zero 像默认路径一样把
	// 源起点归零，于是输出时间戳直接从 startTime 开始，不再需要 -output_ts_offset。
	if copyTS {
		args = append(args, "-copyts", "-start_at_zero")
	}

	// 输入：先限定协议白名单（输入选项，必须在 -i 之前），再指定输入
	args = append(args, "-protocol_whitelist", ProtocolWhitelistFor(source))
	if decoder != "" {
		args = append(args, "-c:v", decoder)
	}
	args = append(args, "-i", source)

	// 【chunk 模式】限定时长（output side 更精确）
	if duration > 0.001 {
		if copyTS {
			// -copyts 下输出端的 -t 也按源时间戳计，会在 duration 处而不是
			// startTime+duration 处截止；改用 -to 给出结束位置
			args = append(args, "-to", fmt.Sprintf("%.3f", startTime+duration))
		} else {
			args = append(args, "-t", fmt.Sprintf("%.3f", duration))
		}
	}

	// 视频编码器
	vcodec := pickEncoder(profile.VideoCodec, hw)
	// V4L2 M2M 编码器同样按设备声明来选，设备编不了该格式时回退软件编码
	if enc := v4l2.Encoder(profile.VideoCodec, probe.Width, probe.Height, profile.Scale); enc != "" {
		vcodec = enc
	}
	args = append(args, "-c:v", vcodec)
	switch {
	case strings.HasSuffix(vcodec, "_v4l2m2m"):
		// V4L2 M2M 编码器只收设备支持的原始格式，NV12 最通用，也正是硬解输出的格式。
		// V4L2 negotiates the profile with the driver.
		args = append(args, "-pix_fmt", "nv12")
	case strings.Contains(vcodec, "264"):
		// Keep software H.264 output compatible with browser decoders.
		args = append(args, "-profile:v", "high", "-level:v", "4.0", "-pix_fmt", "yuv420p")
	}
	if profile.VideoBitrate != "" {
		args = append(args, "-b:v", profile.VideoBitrate)
		args = append(args, "-maxrate", profile.VideoBitrate, "-bufsize", profile.VideoBitrate)
	}
	if profile.Scale != "" {
		// 软件 / 硬件缩放
		switch hw {
		case "vaapi":
			args = append(args, "-vf", "scale_vaapi="+profile.Scale)
		case "qsv":
			args = append(args, "-vf", "scale_qsv="+profile.Scale)
		default:
			args = append(args, "-vf", "scale="+profile.Scale)
		}
	}
	if preset := defaultPresetFor(vcodec); preset != "" {
		args = append(args, "-preset", preset)
	}

	// 音频
	// 【关键修复】不能使用 -c:a copy，因为浏览器 MSE 不支持 AC3/DTS/EAC3/TrueHD 等
	// 常见于 mkv 的音频编码。直接 copy 会导致 SourceBuffer.appendBuffer 失败
	// 触发 bufferAppendError 死循环。统一强制转 AAC 兼容所有浏览器。
	ac := profile.AudioCodec
	if ac == "" || ac == "copy" {
		ac = "aac"
	}
	args = append(args, "-c:a", ac)
	ab := profile.AudioBitrate
	if ab == "" {
		ab = "160k"
	}
	args = append(args, "-b:a", ab)
	// 强制立体声 + 48kHz，避免 5.1 声道 / 高采样率导致的兼容问题
	args = append(args, "-ac", "2", "-ar", "48000")
	// 只输出音视频。源里有字幕时默认映射会带上一路字幕：播放端用不到，muxer
	// 还要为交错攒约 10 秒的包等字幕，首切片明显变慢；V4L2 M2M 编码器的输出包
	// 引用设备缓冲，被攒住时转码直接卡死。
	args = append(args, "-sn", "-dn")

	// HLS 输出
	if segDur <= 0 {
		segDur = 6
	}
	// 【关键】强制关键帧对齐切片边界，确保每个切片以 IDR 帧开始可独立解码。
	// 没有这些参数时，第一个切片可能不含 IDR 帧，导致 HLS.js bufferAppendError。
	// fps 假设 25 (帧率未知时的保守值)，gop = segDur * fps；force_key_frames 让
	// ffmpeg 在每个切片边界强制插入关键帧。
	gop := segDur * 25
	args = append(args,
		"-g", strconv.Itoa(gop),
		"-keyint_min", strconv.Itoa(gop),
		"-sc_threshold", "0",
		"-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", segDur),
	)
	// 【chunk 模式 - 关键修复】input seek 后 ffmpeg 会把第一帧 PTS 重置为 0，
	// 这会导致 chunk-30 的切片在播放器看来是 PTS=0~60s（而不是 1800~1860s），
	// 进而触发 HLS.js bufferAppendError 或时间轴错位。
	// 解决方法：用 -output_ts_offset 让生成的切片时间戳偏移到原始位置。
	if startTime > 0.001 && !copyTS {
		args = append(args, "-output_ts_offset", fmt.Sprintf("%.3f", startTime))
	}
	args = append(args,
		"-f", "hls",
		"-hls_time", strconv.Itoa(segDur),
		"-hls_playlist_type", "vod",
		"-hls_segment_filename", segPattern,
		"-hls_flags", "independent_segments+temp_file",
	)
	// 【chunk 模式】指定起始切片编号，让全局切片编号连续
	if startSeq > 0 {
		args = append(args, "-start_number", strconv.Itoa(startSeq))
	}
	args = append(args, playlistPath)
	return args
}

// pickEncoder 根据视频编码 + HW 类型选择 ffmpeg 编码器
func pickEncoder(codec, hw string) string {
	codec = strings.ToLower(codec)
	switch codec {
	case "h264", "":
		switch hw {
		case "nvenc":
			return "h264_nvenc"
		case "qsv":
			return "h264_qsv"
		case "vaapi":
			return "h264_vaapi"
		case "amf":
			return "h264_amf"
		case "videotoolbox":
			return "h264_videotoolbox"
		default:
			return "libx264"
		}
	case "hevc", "h265":
		switch hw {
		case "nvenc":
			return "hevc_nvenc"
		case "qsv":
			return "hevc_qsv"
		case "vaapi":
			return "hevc_vaapi"
		case "amf":
			return "hevc_amf"
		case "videotoolbox":
			return "hevc_videotoolbox"
		default:
			return "libx265"
		}
	case "av1":
		switch hw {
		case "nvenc":
			return "av1_nvenc"
		case "qsv":
			return "av1_qsv"
		default:
			return "libsvtav1"
		}
	}
	return "libx264"
}

func defaultPresetFor(encoder string) string {
	if strings.Contains(encoder, "nvenc") {
		return "p5"
	}
	if strings.Contains(encoder, "qsv") {
		return "medium"
	}
	if strings.Contains(encoder, "amf") {
		return "balanced"
	}
	if strings.Contains(encoder, "videotoolbox") {
		return "medium"
	}
	if strings.HasSuffix(encoder, "_v4l2m2m") {
		return "" // V4L2 M2M 编码器没有 preset 选项
	}
	return "veryfast"
}
