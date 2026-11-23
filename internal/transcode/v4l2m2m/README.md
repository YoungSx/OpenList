# Stateful V4L2 transcoding / V4L2 转码

Use official unmodified FFmpeg. This fork selects standard `*_v4l2m2m` codecs;
`v4l2m2m` is an OpenList setting, not a value for FFmpeg's `-hwaccel` option.
It is tested on OneCloud S805 but contains no board-specific register logic.

## Quick start / 快速开始

1. Install a working stateful V4L2 driver on the host. Confirm the relevant
   FFmpeg decoder/encoder wrappers with `ffmpeg -decoders` and `-encoders`.
2. In Docker, map the actual codec nodes, for example
   `--device /dev/video0 --device /dev/video1`, and give the process access.
   Mapping grants access even when the application already supports V4L2.
3. Select `v4l2m2m` under hardware acceleration, or use `-hwaccel v4l2m2m` in
   the standalone worker. Default remains `none`.
4. Start with `transcode_rate_policy=warn` and peak/buffer fields blank.
   Device discovery and codec presence alone do not prove stream compatibility.

宿主先有可用驱动，容器映射实际设备，再选择V4L2。以官方FFmpeg为目标，
不依赖私有扩展；默认平均码率告警不限制网络带宽。

## Selection and scheduling

Discovery intersects stateful M2M compressed OUTPUT/CAPTURE formats with the
configured executable's decoder/encoder lists. Hardware encoding requires NV12
input; size limits use ENUM_FRAMESIZES or encoder TRY_FMT. Unknown source pixel
formats and 10-bit/4:2:2 input use software decoding; a suitable scaled output
may still use hardware encoding. Absent/unsupported devices select existing
software codecs. A running FFmpeg/device failure is reported; partially written
chunks are not silently retried with software. Stateless request API is absent.

Device cache identity includes inode; FFmpeg detection refreshes after executable
replacement. One hardware process runs per local manager or standalone worker,
including unknown-duration/whole-file paths. Seek cancels and waits for the old
process before the next begins; prefetch follows current-chunk completion.
Separate managers/workers must not share an exclusive hardware engine.

## HLS and rate policy

Local and standalone workers share arguments and source/profile types. They keep
rational source rates as CFR (valid range1–240; unknown uses30/1), align GOP/keyframes
to segments, use actual playlist durations and preserve sought timestamps.
Audio `copy` becomes AAC; subtitle/data streams are excluded. Hardware encoders
receive NV12 without software preset/High-profile options. The master omits
CODECS rather than guessing a hardware profile.

| Setting | V4L2 behavior |
| --- | --- |
| `transcode_rate_policy=warn` | Default: send target `-b:v`; report measured average overshoot and continue playback. |
| `transcode_rate_policy=downscale` | With peak/buffer blank, probe output and keep one chosen size for the job. |
| `transcode_rate_policy=fail` | Reject: checked official wrappers do not forward native peak/CPB controls. |
| `transcode_peak_bitrate` / `transcode_buffer_size` | Explicit values are rejected on V4L2; other native backends keep maxrate/bufsize behavior. Units are bps / bits. |

The checked official wrappers forward target bitrate, QP, GOP and frame period;
no private capability probe runs. They do not forward peak/CPB controls.
Direct V4L2 clients can still use a driver's standard controls.

Downscale probes up to six media seconds within a shared90-second wall-time
budget. It preserves aspect ratio, never upscales, and tries requested geometry
then smaller widths1280/854/640/426/320. Complete HLS output must average within
125% of target video+audio, allowing mux overhead. Probe files are cleaned and
the selected geometry survives chunking/reactivation. Future scenes may exceed
that rate: this is measured average selection, not a CPB or network cap.

Master bandwidth uses the greater of requested audio+video and highest measured
segment rate; average bandwidth uses total bytes/media duration, including mux
overhead. Responses cache for two seconds. After three segments, local average
overshoot above125% produces an admin warning and one log message.
The sibling read-token `status` endpoint reports preparation/profile/warnings and
redacted errors without source URLs or worker secrets. The player waits through
bounded preparation and disposes replaced HLS instances.

## Current image and limits

The recorded production image is
`youngsx/openlist-onecloud:v4.2.10-official-ffmpeg-a845b34-clean`:
backend code`6c77764f`, frontend`a845b34`, original base FFmpeg8.1.3 unchanged.
Latest branch heads may include later documentation. The OneCloud driver is
`5fedae9`; driver source/build/rollback live in a separate repository.

Bounded actual browser playback, forward/backward seeks and cancellation pass;
buffering occurs. Neither realtime1080p nor long-running production stability
is guaranteed. Legacy codec tails/content and the driver's second LAN outage
remain open. [Dated evidence](evidence/README.md) preserves exact versions,
including retired experiments which are not compatibility instructions.

中文：自动发现、单任务调度、分数帧率、跳转时间线和实测HLS带宽已实现；
`downscale`只按短探测平均码率选尺寸，不能保证未来峰值。最新干净镜像通过
有界浏览器播放/跳转/取消，仍有缓冲；驱动实机与应用测试分别记录。

## Rebuild after cloning

Use Go from `go.mod` and the matching frontend branch, rather than the upstream
frontend release selected by default in `build.sh`. In a fresh backend checkout:

```sh
# Build the companion frontend first: pnpm install --frozen-lockfile && pnpm build
# Use an empty public/dist; stale assets must not survive frontend replacement.
find public/dist -mindepth 1 ! -name README.md -delete
cp -a /path/to/OpenList-Frontend/dist/. public/dist/
mkdir -p build/local
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath \
  -tags=jsoniter,netgo,osusergo -o build/local/openlist .
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath \
  -tags=jsoniter,netgo,osusergo -o build/local/worker ./cmd/worker
```

The production image recipe only copies the embedded-frontend backend into
`/opt/openlist/openlist` of official base
`docker.io/openlistteam/openlist@sha256:a33e7616190fd95e575d9c3b5d3214027887b2a18886615eb9eebdd7830d75f8`
with owner1001:1001 and mode755. It does not replace FFmpeg/ffprobe.
For an ARMv7 Docker build, use that pinned FROM and
`COPY --chown=1001:1001 --chmod=755 build/local/openlist /opt/openlist/openlist`.
Set provenance/version labels for the actual new source; do not reuse an old
acceptance tag. An existing image archive is the exact-rollback artifact,
not a promise of bit-identical output from a new toolchain.
