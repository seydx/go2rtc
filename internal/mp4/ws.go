package mp4

import (
	"errors"

	"github.com/AlexxIT/go2rtc/internal/api"
	"github.com/AlexxIT/go2rtc/internal/api/ws"
	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/mp4"
)

type FormatConfig struct {
	Type         string `json:"type"`
	Value        string `json:"value"`
	FragmentMode string `json:"fragmentMode"`
}

func handlerWSMSE(tr *ws.Transport, msg *ws.Message) error {
	stream, _ := streams.GetOrPatch(tr.Request.URL.Query())
	if stream == nil {
		return errors.New(api.StreamNotFound)
	}

	var medias []*core.Media
	var format FormatConfig
	if msg.Value != nil {
		switch v := msg.Value.(type) {
		case string:
			format = FormatConfig{
				Value:        v,
				FragmentMode: "frame",
			}
		case map[string]interface{}:
			if codecsVal, ok := v["value"].(string); ok {
				format.Value = codecsVal
			}
			if modeVal, ok := v["fragmentMode"].(string); ok {
				format.FragmentMode = modeVal
			} else {
				format.FragmentMode = "frame"
			}
		}
	}

	if format.Value != "" {
		log.Trace().
			Str("codecs", format.Value).
			Str("mode", format.FragmentMode).
			Msgf("[mp4] new WS/MSE consumer")

		medias = mp4.ParseCodecs(format.Value, true)
	}

	cons := mp4.NewConsumer(medias)
	cons.FormatName = "mse/fmp4"
	cons.FragmentMode = format.FragmentMode
	cons.WithRequest(tr.Request)

	// Parse query parameters for GOP and prebuffer control
	query := tr.Request.URL.Query()
	if s := query.Get("gop"); s != "" {
		cons.UseGOP = core.Atoi(s) != 0
	} else {
		cons.UseGOP = true // Default: GOP enabled
	}
	if s := query.Get("prebuffer"); s != "" {
		cons.PrebufferOffset = core.Atoi(s)
	}

	if err := stream.AddConsumer(cons); err != nil {
		log.Debug().Err(err).Msg("[mp4] add consumer")
		return err
	}

	tr.Write(&ws.Message{Type: "mse", Value: mp4.ContentType(cons.Codecs())})

	go cons.WriteTo(tr.Writer())

	tr.OnClose(func() {
		stream.RemoveConsumer(cons)
	})

	return nil
}

func handlerWSMP4(tr *ws.Transport, msg *ws.Message) error {
	stream, _ := streams.GetOrPatch(tr.Request.URL.Query())
	if stream == nil {
		return errors.New(api.StreamNotFound)
	}

	var medias []*core.Media
	if codecs := msg.String(); codecs != "" {
		log.Trace().Str("codecs", codecs).Msgf("[mp4] new WS/MP4 consumer")
		medias = mp4.ParseCodecs(codecs, false)
	}

	cons := mp4.NewKeyframe(medias)
	cons.WithRequest(tr.Request)

	if err := stream.AddConsumer(cons); err != nil {
		log.Error().Err(err).Caller().Send()
		return err
	}

	tr.Write(&ws.Message{Type: "mse", Value: mp4.ContentType(cons.Codecs())})

	go cons.WriteTo(tr.Writer())

	tr.OnClose(func() {
		stream.RemoveConsumer(cons)
	})

	return nil
}
