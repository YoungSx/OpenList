# V4L2 M2M transcoding

Select `v4l2m2m` in the transcoding hardware acceleration setting, or pass
`-hwaccel v4l2m2m` to the standalone worker. The default remains `none`.
The FFmpeg executable must include the corresponding `*_v4l2m2m` codecs;
these are named codecs rather than an FFmpeg `-hwaccel` backend.

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

V4L2 sessions conservatively use one hardware process per local chunk
manager. Seeking cancels the old chunk and waits for the process to exit
before another hardware chunk starts; prefetch runs after the current chunk
finishes. Software fallback sessions using the `v4l2m2m` setting retain this
serialization so devices becoming available later cannot bypass it. The
standalone worker also advertises and runs one slot in this mode. Other
hardware backends and ordinary software concurrency keep their existing
behavior. Separate worker processes must not share an exclusive device.

The built-in runner preserves source timestamps for sought hardware-decoded
chunks and excludes subtitle/data streams. Hardware encoders receive NV12
without the software encoder's named High profile/preset options. Full
OpenList HLS playback, continuous playback and browser seeking still need
end-to-end hardware acceptance; driver-only codec tests are separate evidence.
