# Rate-control and frame-clock acceptance

Historical acceptance for the recorded image. The private FFmpeg rate-limit
experiment is retired; current development targets unmodified official FFmpeg.
See [current compatibility](README.md).

Runtime source: `7da9ab4e8d7089285467cae4a3442178b92256fd`.
Image: `youngsx/openlist-onecloud:v4.2.10-v4l2m2m-7da9ab4`, ARMv7.
The OneCloud ran its existing Linux 5.15.79-meson and 64 MiB CMA. Tests used
disposable application data, a 256 MiB container limit, the installed decoder
and runtime, and the candidate encoder
`366d0653cb3f34a4d7f3c5d876463a28fab857e012ba9e8a991e5ac96bd916fe`.

The [sanitized results](rate-control-2026-11-21.json) record 16 independently
software-decoded HTTP HLS segments with AAC and observed hardware decode/encode.

| Case | Result |
| --- | --- |
| 640×360, 30/1, 6.5 seconds, target video 1.6 Mbps | 195/195 pictures in four segments; 0.5-second tail; BANDWIDTH 1,846,912 and AVERAGE-BANDWIDTH 1,765,696 bps |
| 640×360, 30000/1001 | 195/195 pictures; fractional timing and short tail preserved; BANDWIDTH 1,851,077 and AVERAGE-BANDWIDTH 1,764,164 bps |
| 1080p, video target 1 Mbps plus 96 kbps AAC | Sequences 0/1/2/18/19/6/7/8 decode with 60 pictures each and the correct seek timeline; BANDWIDTH 7,450,064 and AVERAGE-BANDWIDTH 6,982,488 bps; job warning present |

The average was checked against segment bytes divided by their actual playlist
durations. CFR and GOP60 were observed for both tested frame rates, without
unsupported V4L2 maxrate/bufsize arguments. This measurement includes audio
and TS overhead and cannot guarantee a future scene's peak rate.

Hardware testing exposed additional lifecycle bugs. Subsecond source durations
were truncated before chunk indexing, so the 0.5-second tail returned 404 even
though FFmpeg produced it. That range now uses the full duration. Cached reads
also reactivated completed jobs and overwrote the same URLs with another encode,
so fetched bytes and current cache bandwidth could describe different runs.
Playlist/segment cache hits now keep the completed job and its output.

CFR also reset sought hardware output to zero. Hardware input keeps nonnegative
timestamps, both output streams are rebased before CFR, duration is bounded from
that start, and the muxer restores the seek offset. A standalone four-second
hardware check at 36 seconds produced segment18/19 video at 37.4/39.4 seconds,
60 pictures each. The final HTTP forward/backward seek passed the same timeline.

The readiness/completion lock cycle was independently reproduced with a reader
holding the job lock. Ready and finish now take locks in the same order. Unit
tests, image-tag race checks, vet, ARMv7 builds and the fork's CI passed. The
driver's host/sanitizer, full kernel build and silicon results are separate
evidence in its repository.

At completion all four production service HTTP probes returned 200, Arcane
remained running, OpenList's start time stayed unchanged with restart count
zero, and the decoder pool returned to zero slots and bytes. The original
encoder was restored and the temporary container stopped. Boot kernel, device
tree, firmware, CMA and production settings/database were preserved.

The full Docker archive was exported and verified externally. WanOS imported an
incremental archive reusing the exact base layers already installed there; its
configuration, new layer and source revision were verified. This round reused
the existing frontend artifact and did not publish an image to a registry or
replace production. Browser rendering, remote-storage, standalone-worker hardware,
quality/PSNR and a new long soak were not tested. Strict CBR/CPB enforcement and
automatic downscaling remain unimplemented.
