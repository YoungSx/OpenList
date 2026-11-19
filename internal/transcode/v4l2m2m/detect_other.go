//go:build !(linux && (386 || amd64 || arm || arm64 || loong64 || riscv64))

package v4l2m2m

import "errors"

// V4L2 只存在于 Linux；其余架构的 ioctl 编码不同，一律视为没有设备，走软件编解码
type platform struct{}

func (platform) nodes() []string { return nil }

func (platform) id(string) (uint64, bool) { return 0, false }

func (platform) scan(string) (Caps, error) { return Caps{}, errors.ErrUnsupported }
