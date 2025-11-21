package core

func init() {
	// Register generic audio handler for all audio codecs
	RegisterCodecHandler(CodecPCM, CreateAudioHandler)
	RegisterCodecHandler(CodecPCML, CreateAudioHandler)
	RegisterCodecHandler(CodecPCMA, CreateAudioHandler)
	RegisterCodecHandler(CodecPCMU, CreateAudioHandler)
	RegisterCodecHandler(CodecOpus, CreateAudioHandler)
	RegisterCodecHandler(CodecAAC, CreateAudioHandler)
	RegisterCodecHandler(CodecPCM, CreateAudioHandler)
	RegisterCodecHandler(CodecG722, CreateAudioHandler)
	RegisterCodecHandler(CodecFLAC, CreateAudioHandler)
	RegisterCodecHandler(CodecELD, CreateAudioHandler)
	RegisterCodecHandler(CodecMP3, CreateAudioHandler)
}

func CreateAudioHandler(codec *Codec) CodecHandler {
	// For audio, every packet is treated as a "keyframe"
	isKeyframeFunc := func(payload []byte) bool {
		return true
	}

	// Simple pass-through for RTP depay
	createRTPDepayFunc := func(c *Codec, handler HandlerFunc) HandlerFunc {
		return handler
	}

	// No AVCC repair needed for audio
	createAVCCRepairFunc := func(c *Codec, handler HandlerFunc) HandlerFunc {
		return handler
	}

	return NewCodecHandler(
		codec,
		isKeyframeFunc,
		createRTPDepayFunc,
		createAVCCRepairFunc,
		nil, // No payloader needed
		nil, // No fmtp update needed
	)
}
