package transcode

import "github.com/OpenListTeam/OpenList/v4/internal/transcode/ffmpegargs"

func ProtocolWhitelistFor(source string) string { return ffmpegargs.ProtocolWhitelistFor(source) }
func ValidSourceInput(source string) bool       { return ffmpegargs.ValidSourceInput(source) }
