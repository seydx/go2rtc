package streams

import (
	"errors"
	"reflect"
	"strings"

	"github.com/AlexxIT/go2rtc/pkg/core"
)

func (s *Stream) AddConsumer(cons core.Consumer) (err error) {
	// support for multiple simultaneous pending from different consumers
	consN := s.pending.Add(1) - 1

	var prodErrors = make([]error, len(s.producers))
	var prodMedias []*core.Media
	var prodStarts []*Producer

	// Track what tracks previous producers have provided
	prevHasAudio := false
	prevHasVideo := false

	// Step 1. Get consumer medias
	consMedias := cons.GetMedias()

	// Check if consumer requests backchannel (any recvonly media)
	// Normal media (video/audio receive): sendonly
	// Backchannel media (audio send back): recvonly
	consumerRequestsBackchannel := false
	for _, consMedia := range consMedias {
		if consMedia.Direction == core.DirectionRecvonly {
			consumerRequestsBackchannel = true
			break
		}
	}

	for _, consMedia := range consMedias {
		log.Trace().Msgf("[streams] check cons=%d media=%s", consN, consMedia)

	producers:
		for prodN, prod := range s.producers {
			// check for loop request, ex. `camera1: ffmpeg:camera1`
			if info, ok := cons.(core.Info); ok && prod.url == info.GetSource() {
				log.Trace().Msgf("[streams] skip cons=%d prod=%d", consN, prodN)
				continue
			}

			if prodErrors[prodN] != nil {
				log.Trace().Msgf("[streams] skip cons=%d prod=%d", consN, prodN)
				continue
			}

			// Check #requirePrevAudio - skip if previous producers don't have audio
			if prod.requirePrevAudio && !prevHasAudio && prodN > 0 {
				log.Trace().Msgf("[streams] skip cons=%d prod=%d (requires previous audio but none available)", consN, prodN)
				continue
			}

			// Check #requirePrevVideo - skip if previous producers don't have video
			if prod.requirePrevVideo && !prevHasVideo && prodN > 0 {
				log.Trace().Msgf("[streams] skip cons=%d prod=%d (requires previous video but none available)", consN, prodN)
				continue
			}

			// Skip producers with backchannel disabled when consumer requests backchannel
			// Only skip completely if no explicit video/audio flags are set
			if consumerRequestsBackchannel && !prod.backchannelEnabled && !prod.videoExplicitlySet && !prod.audioExplicitlySet {
				log.Trace().Msgf("[streams] skip cons=%d prod=%d (consumer requests backchannel, producer has #noBackchannel)", consN, prodN)
				continue
			}

			// Skip producers with video disabled when consumer requests normal video
			if consMedia.Direction == core.DirectionSendonly && consMedia.Kind == core.KindVideo && !prod.videoEnabled {
				log.Trace().Msgf("[streams] skip cons=%d prod=%d (consumer requests video, producer has #noVideo)", consN, prodN)
				continue
			}

			// Skip producers with audio disabled when consumer requests normal audio
			if consMedia.Direction == core.DirectionSendonly && consMedia.Kind == core.KindAudio && !prod.audioEnabled {
				log.Trace().Msgf("[streams] skip cons=%d prod=%d (consumer requests audio, producer has #noAudio)", consN, prodN)
				continue
			}

			// Skip producers with backchannel disabled when consumer requests backchannel for this specific media
			if consMedia.Direction == core.DirectionRecvonly && !prod.backchannelEnabled {
				log.Trace().Msgf("[streams] skip cons=%d prod=%d (consumer requests backchannel for this media, producer has #noBackchannel)", consN, prodN)
				continue
			}

			if err = prod.Dial(); err != nil {
				log.Trace().Err(err).Msgf("[streams] dial cons=%d prod=%d", consN, prodN)
				prodErrors[prodN] = err
				continue
			}

			// Step 2. Get producer medias (not tracks yet)
			for _, prodMedia := range prod.GetMedias() {
				log.Trace().Msgf("[streams] check cons=%d prod=%d media=%s", consN, prodN, prodMedia)
				prodMedias = append(prodMedias, prodMedia)

				if prodMedia.Direction == core.DirectionRecvonly {
					if prodMedia.Kind == core.KindAudio {
						prevHasAudio = true
					} else if prodMedia.Kind == core.KindVideo {
						prevHasVideo = true
					}
				}

				// Step 3. Match consumer/producer codecs list
				prodCodec, consCodec := prodMedia.MatchMedia(consMedia)
				if prodCodec == nil {
					continue
				}

				var track *core.Receiver

				switch prodMedia.Direction {
				case core.DirectionRecvonly:
					// Skip producers with backchannel disabled when consumer requests backchannel
					if consMedia.Direction == core.DirectionSendonly && !prod.backchannelEnabled {
						log.Trace().Msgf("[streams] skip cons=%d <= prod=%d (consumer requests backchannel, but producer has backchannel disabled)", consN, prodN)
						continue
					}

					log.Trace().Msgf("[streams] match cons=%d <= prod=%d", consN, prodN)

					// Step 4. Get recvonly track from producer
					if track, err = prod.GetTrack(prodMedia, prodCodec); err != nil {
						log.Info().Err(err).Msg("[streams] can't get track")
						prodErrors[prodN] = err
						continue
					}
					// Step 5. Add track to consumer
					if err = cons.AddTrack(consMedia, consCodec, track); err != nil {
						log.Info().Err(err).Msg("[streams] can't add track")
						continue
					}

				case core.DirectionSendonly:
					// Skip producers with backchannel explicitly disabled
					if !prod.backchannelEnabled {
						log.Trace().Msgf("[streams] skip cons=%d => prod=%d (backchannel disabled)", consN, prodN)
						continue
					}

					log.Trace().Msgf("[streams] match cons=%d => prod=%d", consN, prodN)

					// Step 4. Get recvonly track from consumer (backchannel)
					if track, err = cons.(core.Producer).GetTrack(consMedia, consCodec); err != nil {
						log.Info().Err(err).Msg("[streams] can't get track")
						continue
					}
					// Step 5. Add track to producer
					if err = prod.AddTrack(prodMedia, prodCodec, track); err != nil {
						log.Info().Err(err).Msg("[streams] can't add track")
						prodErrors[prodN] = err
						continue
					}
				}

				prodStarts = append(prodStarts, prod)

				if !consMedia.MatchAll() {
					break producers
				}
			}
		}
	}

	// stop producers if they don't have readers
	if s.pending.Add(-1) == 0 {
		s.stopProducers()
	}

	if len(prodStarts) == 0 {
		return formatError(consMedias, prodMedias, prodErrors)
	}

	s.mu.Lock()
	s.consumers = append(s.consumers, cons)
	s.mu.Unlock()

	// there may be duplicates, but that's not a problem
	for _, prod := range prodStarts {
		prod.start()
	}

	// Check if consumer requested prebuffer and start replay if needed
	s.checkAndStartPrebuffer(cons)

	return nil
}

func (s *Stream) checkAndStartPrebuffer(cons core.Consumer) {
	// Use reflection to check if consumer has UsePrebuffer field
	// This works for all consumer types: WebRTC, RTSP, HLS, MP4, MSE, etc.
	var usePrebuffer bool

	consValue := reflect.ValueOf(cons)
	if consValue.Kind() == reflect.Ptr {
		consValue = consValue.Elem()
	}

	if consValue.Kind() == reflect.Struct {
		field := consValue.FieldByName("UsePrebuffer")
		if field.IsValid() && field.Kind() == reflect.Bool {
			usePrebuffer = field.Bool()
		}
	}

	if usePrebuffer {
		// log.Debug().Msgf("[streams] Starting prebuffer replay for consumer (using producer's configured duration)")
		s.StartPrebufferReplay()
	}
}

func formatError(consMedias, prodMedias []*core.Media, prodErrors []error) error {
	// 1. Return errors if any not nil
	var text string

	for _, err := range prodErrors {
		if err != nil {
			text = appendString(text, err.Error())
		}
	}

	if len(text) != 0 {
		return errors.New("streams: " + text)
	}

	// 2. Return "codecs not matched"
	if prodMedias != nil {
		var prod, cons string

		for _, media := range prodMedias {
			if media.Direction == core.DirectionRecvonly {
				for _, codec := range media.Codecs {
					prod = appendString(prod, media.Kind+":"+codec.PrintName())
				}
			}
		}

		for _, media := range consMedias {
			if media.Direction == core.DirectionSendonly {
				for _, codec := range media.Codecs {
					cons = appendString(cons, media.Kind+":"+codec.PrintName())
				}
			}
		}

		return errors.New("streams: codecs not matched: " + prod + " => " + cons)
	}

	// 3. Return unknown error
	return errors.New("streams: unknown error")
}

func appendString(s, elem string) string {
	if strings.Contains(s, elem) {
		return s
	}
	if len(s) == 0 {
		return elem
	}
	return s + ", " + elem
}
