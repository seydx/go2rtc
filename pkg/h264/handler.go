package h264

import "github.com/AlexxIT/go2rtc/pkg/core"

func init() {
	core.RegisterGopCodec(core.CodecH264, IsKeyframe, RTPDepay)
}
