# Rate budget acceptance / 码率预算验收

On OneCloud S805 with Linux 5.15.79-meson and 64 MiB CMA, the disposable
local OpenList container at `64c0907c` passed default warning behavior,
HTTP 500 on budget or mapping failures, and complete decoding of a
195-frame 30000/1001 sample including its 15-frame tail. Downscale selected
`854:-2` before publication and preserved that width on forward and backward
seeks.

独立 worker 与 Master 使用 `29c7842a`。worker 通过签名 HTTP 地址读取
源文件，硬件并发容量为 1。有预算短片与降档短片各完整输出 195 帧，
降档选择 `426:-2`。超预算返回 HTTP 500。两组测试均恢复了原编码模块，
生产 OpenList 未重启。验收范围为 HTTP HLS、帧计数与软件解码，浏览器
渲染需单独验证。

Earlier candidates exposed an unsupported FFmpeg option, publication
of a tail after encoder failure, a first-playlist timeout during probing
and a Master-only path in a remote claim. Each issue was fixed before
the relevant passing test.
