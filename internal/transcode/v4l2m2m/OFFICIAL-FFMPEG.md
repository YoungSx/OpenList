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

Frontend unit tests and its production build cover preparation timeout and
status handling. Browser rendering is a separate acceptance. Current full
frontend type-check still reports pre-existing Monaco/archive/storage errors;
the touched player files no longer report errors.

The old private-extension rate-budget image is superseded. Dated historical
results remain available but must not be used as official compatibility proof.
