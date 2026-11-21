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
| `transcode_rate_policy=fail` | Require the video peak/buffer envelope and report an encode failure when the driver rejects output. |
| `transcode_rate_policy=downscale` | Probe isolated HLS output before publication, then keep one selected size for the job. |
| `transcode_peak_bitrate` | Video bits per second; blank uses the requested target. |
| `transcode_buffer_size` | Video buffer in **bits**, like FFmpeg `-bufsize`; blank allows two seconds of peak rate. |

Peak must be at least the positive target, and buffer must hold at least 8192
bits. For V4L2 H.264, `fail` and `downscale` require the configured FFmpeg to
expose `-v4l2_rate_limit`. The builder passes that option, maxrate and bufsize;
the wrapper transactionally sets BITRATE, BITRATE_PEAK and H264_CPB_SIZE and
fails if the device rejects them. The independent
[FFmpeg patch](https://github.com/YoungSx/onecloud-meson8b-vcodec/blob/main/patches/ffmpeg-7.0.2-v4l2-rate-limits.patch)
is for 7.0.2 after its drain patch. Other V4L2 encoder formats do not have this
mapping. Validation errors never produce an unconstrained command.

The Meson8b driver uses KiB (1024 bytes) for H264_CPB_SIZE. Buffer bits are
rounded down by division by 8192, so the effective capacity does not exceed
the request: `2M` bits becomes 244 KiB (249856 bytes). New sessions start with
full credits, then refill on the negotiated media clock. Oversized access units
fail before publication. This is a VBR envelope with allowed bursts, not
strict CBR, HRD, a per-segment limit or an audio/TS/network cap.

Downscale probes up to six media seconds (less for a short source), with each
attempt limited to 90 wall seconds. It tries the requested size, then smaller
aspect-preserving widths among 1280/854/640/426/320. It requires known geometry,
complete output and a measured HLS average within 125% of video plus audio
target, allowing muxing overhead. Probe files are private temporary output and
are removed on success, failure or cancellation. A selected profile preserves
its name/cache URLs; `effective_profile` reports the chosen size locally and
in the standalone worker's finish callback. Chunks and reactivated jobs reuse
it. A later complex scene can still exceed the budget and fail; the sample
does not predict future peaks. Failed jobs return errors for missing segments
and playlists without automatic reactivation; completed cached segments remain
readable. A partially encoded chunk is not retried with another encoder.

The candidate image keeps stock FFmpeg and adds the independent test tool at
`/opt/openlist/ffmpeg-v4l2`. To use these V4L2 limits, explicitly configure
`transcode_ffmpeg_path` to that path (or provide a compatible full FFmpeg),
plus the corresponding driver. The tool supports files/HTTP, H.264/HEVC/AAC
and HLS/MPEG-TS. It lacks HTTPS/TLS, libx264/libx265 and full format coverage;
it is not a general replacement for stock FFmpeg. The standalone worker uses
the same choice through `-ffmpeg`.

The play endpoint first filters size/extension, then checks source codec and
bitrate again after ffprobe before submitting a job. An unknown probe field
retains the existing permissive behavior; device selection remains conservative.

Hardware acceptance belongs to a specific image and sample set. HTTP HLS
segment validation does not establish browser rendering, all stream profiles
or long-running production stability; driver tests are separate evidence.
