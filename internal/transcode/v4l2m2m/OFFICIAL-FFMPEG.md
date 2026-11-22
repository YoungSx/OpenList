# Official FFmpeg acceptance / 官方 FFmpeg 验收

Image `v4.2.10-official-ffmpeg-a137737` uses the pinned official AIO base's
FFmpeg unchanged; no custom binary or private control option is installed.
Backend source `a137737a`, frontend source `2926dbd5`, encoder source `3aed869`.
Encoder SHA-256: `e72e3a6697b5f7296520f8b339b7c7e6af9147b299d0639e92c5e42128f4747b`.
The encoder supports standard contiguous NV12 so software scaling avoids
shared multi-plane upload assumptions in FFmpeg.

[Local results](official-local-2026-11-22.json) cover default warning behavior,
explicit rejection of unsupported peak/buffer constraints, the complete
195-frame 30000/1001 sample, and measured downscale to `854:-2` with segment
requests 0/1/2/18/19/6/7/8. The read-only status endpoint reports the same
selected profile as the admin job snapshot without worker/source credentials.

[Worker results](official-worker-2026-11-22.json) cover the complete short
sample and measured downscale to `320:-2`, through signed HTTP source input.
The worker advertises one hardware slot. Successful outputs contain H.264/AAC,
have the expected frame counts and pass independent software decoding.
No maxrate/bufsize/private-option hardware guarantee is claimed.

本地与独立 worker 都使用官方底座 FFmpeg 和独立数据目录。195 帧短片含
15 帧尾片完整交付；本地降到 854 宽并通过前后跳转，worker 降到 320 宽。
试编码按实测平均码率选尺寸，不是硬件 CPB 或未来峰值保证。显式不支持
的限制明确失败。测试结束恢复原编码模块，生产容器启动时间不变，四项
HTTP 探针 200、Arcane 运行、解码帧池归零。

Frontend `a845b34` passes full lint/type-check, 72 tests, production build and
[fork CI](https://github.com/YoungSx/OpenList-Frontend/actions/runs/44992424901).
This includes preparation/status handling and HLS instance replacement cleanup.
The earlier Monaco/archive/storage type errors have been fixed.

Prepared image
`youngsx/openlist-onecloud:v4.2.10-official-ffmpeg-a845b34-clean` contains backend
`6c77764f` and frontend `a845b34`, with old generated frontend assets removed.
Its FFmpeg/ffprobe match the pinned official base byte-for-byte. The image
without the `-clean` suffix is superseded. The clean image has not replaced
production and has no new browser/hardware acceptance.

[Browser observations](https://github.com/YoungSx/onecloud-meson8b-vcodec/blob/e82a3334681b686424fcc8e5eef411d2586d683f/evidence/p0-p3-2026-11-22/browser-observed.json)
apply to the earlier image: bounded playback, forward/backward seeks and
cancellation, with buffering observed. They do not establish realtime playback.
The driver candidate required two local device recoveries during later tests;
original modules/services are restored and sustained production load tests are
paused. See the separate
[driver stability record](https://github.com/YoungSx/onecloud-meson8b-vcodec/blob/e82a3334681b686424fcc8e5eef411d2586d683f/docs/next-development-status.md).

中文：最新版前端已通过完整 lint、72 项测试、构建和 CI。干净镜像保持官方
FFmpeg，尚未替换生产版本，最新版浏览器/硬件验收仍待完成。此前播放有
缓冲，不能作为实时性能保证；驱动验证期间两次离线后原服务已恢复，生产
持续负载测试暂停。

The old private-extension rate-budget image is superseded. Dated historical
results remain available but must not be used as official compatibility proof.
