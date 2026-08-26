package h265

import "github.com/AlexxIT/go2rtc/pkg/core"

func init() {
	core.RegisterGopCodec(core.CodecH265, IsKeyframe, RTPDepay)
}
