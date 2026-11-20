# Compatibility changes and hardware evidence

The follow-up to `c52a889` fixes three behavior problems and removes duplicated
FFmpeg argument construction. Source codec and bitrate filters run again after
ffprobe, whole-file and chunked V4L2 jobs share a cancellable device slot, and
V4L2 master playlists omit the optional `CODECS` attribute until the actual
profile is known. A configured FFmpeg's decoder/encoder wrappers are intersected
with device formats and refreshed when the executable changes.

The local and standalone workers now share a pure HLS builder and source/profile
types. AAC conversion for the `copy` setting, keyframe alignment, input protocol
limits and temporary-file publication follow the same policy. This does not add
a board-specific application branch or a new backend framework.

Validation performed before the candidate image was built:

- The repository's unit-test package set and the standalone worker pass.
- Affected packages pass the race detector and `go vet`.
- Regression checks cover rejection before job submission, whole-file/chunk
  device ownership, cancelled waiters, unknown playlist profiles, separate
  FFmpeg decode/encode lists, executable replacement and common HLS options.
- Backend and standalone worker cross-build for ARMv7 without target compilation.

The original `c52a889` image passed bounded HTTP HLS checks on the OneCloud
running the installed v0.4.0 driver: 320×240 and 1080p H.264 plus audio, eight
segments each covering initial/continuous access and forward/backward seeking.
Video and AAC streams, requested timestamps and independent software decoding
passed. Its actual Baseline output disagreed with its Main profile declaration;
the candidate fixes that declaration without guessing another profile.

During that earlier check, containerd's shim disconnected twice while Docker
exec commands were validating a 1080p segment. A temporary container and then
the production container exited 137; production restarted automatically. There
was no kernel/cgroup OOM evidence and no application panic evidence. The exact
cause remains undetermined. This follow-up repeated version/probe/decode with
and without a single-thread limit, plus six pairs of concurrent probe/decode
calls in temporary containers without codec devices. All completed, with no
new production restart and no fatal signal in the bounded trace. Failure to
reproduce does not establish a fix or a root cause.

Candidate hardware results and image/source hashes are delivered alongside the
candidate archive. Production deployment requires its own acceptance; HTTP
segment checks do not establish browser rendering, remote-storage playback,
every stream profile or sustained multi-client load.

Driver generalization is separate: keep the v0.4 frame pool and standard V4L2
interface, then synchronize the research port before validating static DT,
shared DOS syscon/regmap and genpd/runtime PM on another kernel/board. The live
runtime overlay remains an intentional compatibility path for the old kernel.
