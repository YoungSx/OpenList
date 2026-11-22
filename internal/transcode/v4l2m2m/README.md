# V4L2 M2M transcoding

Select `v4l2m2m` in the transcoding hardware acceleration setting, or pass
`-hwaccel v4l2m2m` to the standalone worker. The default remains `none`.
The FFmpeg executable must include the corresponding `*_v4l2m2m` codecs;
these are named codecs rather than an FFmpeg `-hwaccel` backend.
Device formats are intersected with the configured FFmpeg's decoder and
encoder lists. Lists are cached by executable identity, size and modification
time; replacing the program or changing its path refreshes detection.

On Linux the worker queries `/dev/video*` for stateful memory-to-memory
devices. Compressed OUTPUT formats select decoders; compressed CAPTURE
formats select encoders whose raw OUTPUT accepts NV12. Frame-size limits
come from `ENUM_FRAMESIZES`, with `TRY_FMT` as the encoder fallback.
Unknown source pixel formats and 10-bit/4:2:2 sources use software decoding.
Encoding can still use hardware when the scaled output fits the device.
Unsupported codec/geometry and absent devices select the existing software
codecs. An FFmpeg runtime or device failure is reported normally; this does
not retry a partially written HLS chunk with software.

Known device nodes are cached by inode so discovery does not reopen an
exclusive codec during a transcode. Busy nodes are retried on a later
discovery and removed/recreated nodes are refreshed. Stateless request-API
codecs are not implemented by this integration.

In Docker, explicitly map the codec devices into the container, for example
`--device /dev/video0 --device /dev/video1`, and grant the worker permission
to open them. Startup logs show the detected compressed formats and limits.
Device discovery does not prove every profile or stream is supported.

V4L2 sessions conservatively use one hardware process per local manager,
including the legacy whole-file path when duration is unknown. Waiting for
the device can be cancelled without waiting for the current process.
Seeking cancels the old chunk and waits for the process to exit
before another hardware chunk starts; prefetch runs after the current chunk
finishes. Software fallback sessions using the `v4l2m2m` setting retain this
serialization so devices becoming available later cannot bypass it. The
standalone worker also advertises and runs one slot in this mode. Other
hardware backends and ordinary software concurrency keep their existing
behavior. Separate worker processes must not share an exclusive device.

Local and standalone workers share the HLS argument builder. Both exclude
subtitle/data streams, convert the `copy` audio setting to AAC for browser
compatibility, and align key frames with segment boundaries. The local runner
preserves source timestamps for sought hardware-decoded chunks. Hardware
encoders receive NV12 without the software encoder's named High profile/preset
options. V4L2 master playlists omit the optional `CODECS` attribute because
the profile cannot be predicted from the requested codec alone.

Source probing reads the average frame rate, falling back to the stream rate.
Both workers produce CFR at that rational rate (1–240 fps; unknown rates use
30/1) and set the GOP to `ceil(fps × segment duration)`. Fractional-rate segment
durations and a short final segment come from FFmpeg's playlist rather than a
fixed duration. The frame-rate range describes argument validation, not a
device performance guarantee.

Sought hardware input keeps nonnegative source timestamps into V4L2. Video and
audio filters then rebase the chunk before CFR encoding, its duration is bounded
relative to that start, and the muxer restores the seek offset. This keeps later
chunks on the same playback timeline as the first chunk.

`VideoBitrate` is a target for the device's rate controller. The default
`transcode_rate_policy=warn` sends `-b:v` to V4L2; upstream FFmpeg does not map
`-maxrate`/`-bufsize` to its peak/CPB controls. Other encoder backends keep their
existing arguments. This integration does not provide a strict network
bandwidth cap.
After the first segment is ready, the master playlist advertises the greater
of the requested video-plus-audio rate and the highest measured segment rate.
`AVERAGE-BANDWIDTH` uses total segment bytes divided by their total media
duration, including audio and TS overhead. Measurements update as chunks finish,
and the master response has a two-second cache lifetime. Past segment rates
cannot guarantee a future scene's peak rate.

For local V4L2 jobs, an observed average above 125% of the requested
video-plus-audio rate after at least three segments produces a `warning` in
`GET /api/admin/transcode/jobs` and one log message. Playback continues. Select
a smaller output resolution or software encoding when the connection cannot
carry the measured rate.

The opt-in policies use the existing profile, worker and chunk paths:

| Setting | Behavior |
| --- | --- |
| `transcode_rate_policy=warn` | Default target tracking and overshoot warning; playback continues. |
| `transcode_rate_policy=fail` | Require native peak/buffer support; unsupported V4L2 paths fail explicitly. |
| `transcode_rate_policy=downscale` | Probe measured HLS output before publication, then keep one selected size. |
| `transcode_peak_bitrate` | Video bits per second; blank uses the requested target. |
| `transcode_buffer_size` | Video buffer in **bits**, like FFmpeg `-bufsize`; blank allows two seconds of peak rate. |

Official unmodified FFmpeg is the compatibility target. The private
`v4l2_rate_limit` experiment is retired. The configured executable is checked
only for standard decoder/encoder wrappers; no private capability probe runs.
Standard `-b:v`, `-qmin/-qmax`, `-g` and output frame rate are supported by the
upstream V4L2 wrapper. Encoder-specific help lists private options only.

For V4L2, `warn` uses target tracking. `downscale` with peak/buffer fields left
blank probes actual HLS output and selects a stable geometry before publication.
This does not claim a CPB/peak envelope. `fail` or explicit peak/buffer limits
are rejected on this path because the checked official wrappers do not forward
those controls; use an encoder with native support when those are required.
The direct V4L2 driver controls remain available to other clients. Other native
encoder backends retain maxrate/bufsize handling.

Downscale probes up to six media seconds with all candidates sharing a
90-second wall-time budget. First-playlist waits include that budget. Candidates
preserve aspect ratio, never upscale, and try the requested size then smaller
widths among 1280/854/640/426/320. Complete HLS output must average within 125%
of target video plus audio, allowing muxing overhead. Temporary probe files are
removed on every exit. The selected profile keeps its cache identity and is
reused across chunks and reactivation. Future scenes can exceed the measured
rate; this policy is not a network cap or a prediction of future output.

Use stock FFmpeg from the existing image. A shared-buffer raw NV12 crash,
drain truncation or incorrectly propagated terminal error requires an independent
reproducer before any optional bug patch is used. Do not deploy the earlier
private-extension test image as the official compatibility solution.

官方原版 FFmpeg 是兼容目标，不使用私有码控扩展。V4L2 `warn` 跟踪目标，
`downscale` 在峰值/缓冲留空时按实测 HLS 平均码率试编码，选择尺寸并在
任务中保持稳定；它不保证 CPB 或未来场景峰值。显式峰值/缓冲预算和
`fail` 在该路径清晰报错，原生支持预算的其他编码器仍可使用。只有明确
复现的 bug 才保留可选最小修复，不以 FFmpeg fork 弥补功能。

The play endpoint first filters size/extension, then checks source codec and
bitrate again after ffprobe before submitting a job. An unknown probe field
retains the existing permissive behavior; device selection remains conservative.

Hardware acceptance belongs to a specific image and sample set. HTTP HLS
segment validation does not establish browser rendering, all stream profiles
or long-running production stability; driver tests are separate evidence.

Playback clients may GET the sibling `status` URL next to `master.m3u8` using
its read token. The response is uncached and contains state, a redacted error,
measured warning and a copy of the selected profile; worker tokens and signed
source URLs are omitted. The player includes bounded preparation in its HLS
manifest wait and reads this status after preparation or fatal HLS errors.
