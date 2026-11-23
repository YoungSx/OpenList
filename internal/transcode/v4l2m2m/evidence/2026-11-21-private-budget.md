> Historical snapshot; current setup and limits are in the [guide](../README.md).

# Rate budget acceptance / 码率预算验收

Historical acceptance for the recorded image. The private FFmpeg rate-limit
experiment is retired; current development targets unmodified official FFmpeg.
See [current compatibility](../README.md).

Image: `youngsx/openlist-onecloud:v4.2.10-v4l2m2m-29c7842`, ARMv7.
Backend/worker source: `29c7842afaa3691e824c4021f3d49b4cdc361f1a`.
The companion encoder and FFmpeg patches are in
`YoungSx/onecloud-meson8b-vcodec` at `748fdc2`; the encoder SHA-256 is
`e67a83367410c859f44151c774b2290124ca1f5eb3751c1161350556a92d84e8`.
The machine retains Linux 5.15.79-meson and its 64 MiB CMA budget.

The final image passed [local acceptance](rate-budget-local-2026-11-21.json)
and [standalone-worker acceptance](rate-budget-worker-2026-11-21.json) with
disposable data. Local output was checked again after the remote-source fix.
The earlier local run at `64c0907c` remains historical evidence.

| Check | Result |
| --- | --- |
| Default `warn`, 1080p H.264/AAC, 1 Mbps target | Playback continues; actual HLS bandwidth and overshoot warning are reported. |
| `fail`, 1 Mbps peak, 8192-bit buffer | Missing segment and playlist requests return HTTP 500 repeatedly; the failed job is not reactivated. |
| `fail` with stock FFmpeg lacking the control mapping | Clear mapping error and HTTP 500. |
| Bounded 30000/1001 short sample | All 195 video frames decode, including the 15-frame final segment; AAC and actual segment durations are retained. |
| Local `downscale`, 1080p source, 800k target | Selects `854:-2` before publication; segments 0/1/2/18/19/6/7/8 retain that width and their timeline. |
| Standalone worker | Advertises one hardware slot, reads a signed HTTP source, and completes bounded encoding and explicit budget failure. |
| Standalone `downscale`, 640×360 source, 500k target | Selects `426:-2`; all 195 video frames decode and the effective profile reaches the Master. |

The tests verify H.264/AAC packets, video frame counts, independent software
decoding, configured FFmpeg codecs and the driver's active peak/buffer controls.
Every successful constrained session has zero budget violations. Both test
scripts restore the original encoder. Production OpenList's start time and
restart count remain unchanged; four HTTP service probes return 200 and
Arcane remains running. The decoder pool returns to zero slots/bytes.

最终镜像的[本地验收](rate-budget-local-2026-11-21.json)和
[独立 worker 验收](rate-budget-worker-2026-11-21.json)均使用独立数据库，
不替换生产容器。默认告警保持播放，低预算与缺少映射返回 500，同一失败
任务不会被自动重启。有预算短片完整输出 195 帧；本地降档为 `854:-2`，
前后跳转保持尺寸与时间线；worker 通过签名 HTTP 地址读取源文件、容量
为 1，降档到 `426:-2` 后完整输出短片，并向 Master 回报选定档位。
两组结束后恢复原编码模块，五项服务正常，解码帧池释放。

This is an opt-in media-time VBR envelope with allowed bursts. It does not
establish strict CBR/HRD or a per-HLS/network bandwidth cap. The short probe
does not predict future scenes; a later budget violation still fails.
HTTP segment acceptance does not establish browser rendering, arbitrary
remote-storage protocols, a quality/PSNR matrix or indefinite production load.

该预算约束视频 AU 的媒体时间码量，允许缓冲突发，不代表严格 CBR/HRD
或逐片网络限速。短时探测不能预测未来场景，后续超额仍会失败。浏览器、
任意远程存储协议、画质矩阵和无限生产负载没有在此记录中验收。

Earlier candidates exposed an unsupported FFmpeg option, publication
of a tail after encoder failure, a first-playlist timeout during probing
and a Master-only path in a remote claim. Each issue was fixed before
the relevant passing test. The current application flow keeps the existing
profiles, scheduler, cache and worker protocol; it adds validation and a
bounded preparation step rather than another scheduling framework.
