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

Image `youngsx/openlist-onecloud:v4.2.10-official-ffmpeg-a845b34-clean`
contains backend code `6c77764f` and frontend `a845b34`,with residual generated
assets removed. FFmpeg/ffprobe match the pinned official base byte-for-byte.
It now runs on production with the explicitly authorized development driver
source `1a1d1f7`; original v0.4.0 codec installation is retained for rollback.
The superseded image without the `-clean` suffix is not used for acceptance.

The clean image passed bounded actual browser HLS playback,uncached forward
and backward seeks,end seek and cancellation in disposable data. No MediaError
occurred; exact cached seek events hit12 and50 seconds,and device release took
0.242s with poolzero. Buffering occurred,so this is not a realtime guarantee.
See the separate
[dated driver/application evidence](https://github.com/YoungSx/onecloud-meson8b-vcodec/tree/main/evidence/development-2026-11-22).

The new driver includes contiguous NV12,IRQ/EOS completion ordering,async
conversion,SPS/VUI and firmware DMA guards. Normal and fractional-rate MP4
count,duration,color-tag/crop and independent decode checks pass with stock
FFmpeg. Unknown color metadata is not invented; stock `h264_metadata` can
supply explicit output tags after an actual colorspace conversion.

Prior device outages remain unexplained. Sustained production load tests are
paused; bounded playback and CI do not establish long-term stability. Legacy
codec tails and the1.3× realtime target remain open. Source and tests remain
compatible with official FFmpeg without private control extensions.

中文：新版干净 OpenList 镜像已运行，新驱动按授权部署，原安装保留可回滚。
最新前端通过有界实际浏览器播放、前后跳转、取消与设备释放；仍有缓冲，
不宣称实时保证。码流元数据与分数帧率检查通过，FFmpeg 保持原版；历史
整机离线原因、旧格式尾帧和长期稳定性仍是未完成项。

The old private-extension rate-budget image is superseded. Dated historical
results remain available but must not be used as official compatibility proof.
