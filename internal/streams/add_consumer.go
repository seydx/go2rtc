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
	consumerRequestsBackchannel := false
	for _, consMedia := range consMedias {
		if consMedia.Direction == core.DirectionRecvonly {
			consumerRequestsBackchannel = true
			break
		}
	}

	// PHASE 1: Best-fit Producer Search
	// Try to find a single producer that can satisfy ALL consumer medias.
	// This avoids splitting video/audio across different producers.

	var bestFitProd *Producer
	var bestFitProdN int = -1

	for prodN, prod := range s.producers {
		// Skip-checks
		if info, ok := cons.(core.Info); ok && prod.url == info.GetSource() {
			log.Trace().Msgf("[streams] bestfit skip cons=%d prod=%d (loop)", consN, prodN)
			continue
		}

		if prodErrors[prodN] != nil {
			continue
		}

		// Check #requirePrevAudio/#requirePrevVideo - these producers can't be best-fit
		// because they depend on previous producers
		if prod.requirePrevAudio && prodN > 0 {
			continue
		}
		if prod.requirePrevVideo && prodN > 0 {
			continue
		}

		// Skip producers with backchannel disabled when consumer requests backchannel
		if consumerRequestsBackchannel && !prod.backchannelEnabled && !prod.videoExplicitlySet && !prod.audioExplicitlySet {
			continue
		}

		// Dial the producer to get its medias
		if err = prod.Dial(); err != nil {
			log.Trace().Err(err).Msgf("[streams] bestfit dial cons=%d prod=%d", consN, prodN)
			prodErrors[prodN] = err
			continue
		}

		// Check if this producer can satisfy ALL consumer medias
		if canProducerSatisfyAll(prod, consMedias) {
			bestFitProd = prod
			bestFitProdN = prodN
			log.Trace().Msgf("[streams] bestfit found cons=%d prod=%d", consN, prodN)
			break // Use first best-fit producer
		}
	}

	// PHASE 2: Track Matching
	// If best-fit found: use only that producer for all tracks
	// If not: fall back to track-by-track matching across producers

	if bestFitProd != nil {
		// Best-fit mode: Get ALL tracks from this single producer
		log.Trace().Msgf("[streams] using bestfit prod=%d for all tracks", bestFitProdN)

		for _, prodMedia := range bestFitProd.GetMedias() {
			prodMedias = append(prodMedias, prodMedia)

			if prodMedia.Direction == core.DirectionRecvonly {
				if prodMedia.Kind == core.KindAudio {
					prevHasAudio = true
				} else if prodMedia.Kind == core.KindVideo {
					prevHasVideo = true
				}
			}
		}

		for _, consMedia := range consMedias {
			// Find matching prodMedia from best-fit producer
			for _, prodMedia := range bestFitProd.GetMedias() {
				prodCodec, consCodec := prodMedia.MatchMedia(consMedia)
				if prodCodec == nil {
					// For backchannel with mixing support, allow codec mismatch
					// The mixer will handle transcoding
					// Note: prodMedia.Direction=sendonly means producer sends TO camera (backchannel)
					//       consMedia.Direction=recvonly means consumer receives FROM client (backchannel)
					if prodMedia.Direction == core.DirectionSendonly &&
						consMedia.Direction == core.DirectionRecvonly &&
						prodMedia.Kind == consMedia.Kind &&
						bestFitProd.mixingEnabled {
						if len(prodMedia.Codecs) > 0 && len(consMedia.Codecs) > 0 {
							prodCodec = prodMedia.Codecs[0]
							consCodec = consMedia.Codecs[0]
							log.Trace().Msgf("[streams] bestfit backchannel codec mismatch, mixer will transcode %s -> %s", consCodec.Name, prodCodec.Name)
						} else {
							continue
						}
					} else {
						continue
					}
				}

				var track *core.Receiver

				switch prodMedia.Direction {
				case core.DirectionRecvonly:
					log.Trace().Msgf("[streams] bestfit match cons=%d <= prod=%d", consN, bestFitProdN)

					if track, err = bestFitProd.GetTrack(prodMedia, prodCodec); err != nil {
						log.Info().Err(err).Msg("[streams] can't get track")
						prodErrors[bestFitProdN] = err
						continue
					}
					if err = cons.AddTrack(consMedia, consCodec, track); err != nil {
						log.Info().Err(err).Msg("[streams] can't add track")
						continue
					}

				case core.DirectionSendonly:
					if !bestFitProd.backchannelEnabled {
						continue
					}

					log.Trace().Msgf("[streams] bestfit match cons=%d => prod=%d", consN, bestFitProdN)

					if track, err = cons.(core.Producer).GetTrack(consMedia, consCodec); err != nil {
						log.Info().Err(err).Msg("[streams] can't get track")
						continue
					}
					if err = bestFitProd.AddTrack(prodMedia, prodCodec, track); err != nil {
						log.Info().Err(err).Msg("[streams] can't add track")
						prodErrors[bestFitProdN] = err
						continue
					}
				}

				prodStarts = append(prodStarts, bestFitProd)

				if !consMedia.MatchAll() {
					break // Next consMedia
				}
			}
		}
	} else {
		// Fallback mode: Track-by-track matching across all producers
		log.Trace().Msgf("[streams] no bestfit, using track-by-track matching")

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
						switch prodMedia.Kind {
						case core.KindAudio:
							prevHasAudio = true
						case core.KindVideo:
							prevHasVideo = true
						}
					}

					// Step 3. Match consumer/producer codecs list
					prodCodec, consCodec := prodMedia.MatchMedia(consMedia)
					if prodCodec == nil {
						// For backchannel with mixing support, allow codec mismatch
						// The mixer will handle transcoding (e.g., OPUS from consumer -> PCMA to camera)
						// Note: prodMedia.Direction=sendonly means producer sends TO camera (backchannel)
						//       consMedia.Direction=recvonly means consumer receives FROM client (backchannel)
						if prodMedia.Direction == core.DirectionSendonly &&
							consMedia.Direction == core.DirectionRecvonly &&
							prodMedia.Kind == consMedia.Kind &&
							prod.mixingEnabled {
							// Use producer's codec as output target, consumer's codec as input
							if len(prodMedia.Codecs) > 0 && len(consMedia.Codecs) > 0 {
								prodCodec = prodMedia.Codecs[0]
								consCodec = consMedia.Codecs[0]
								log.Trace().Msgf("[streams] backchannel codec mismatch, mixer will transcode %s -> %s", consCodec.Name, prodCodec.Name)
							} else {
								continue
							}
						} else {
							continue
						}
					}

					var track *core.Receiver

					switch prodMedia.Direction {
					case core.DirectionRecvonly:
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

// canProducerSatisfyAll checks if a single producer can satisfy ALL consumer medias.
// This is used for best-fit producer matching to avoid splitting tracks across producers.
func canProducerSatisfyAll(prod *Producer, consMedias []*core.Media) bool {
	prodMedias := prod.GetMedias()

	for _, consMedia := range consMedias {
		found := false

		// Check if any prodMedia can satisfy this consMedia
		for _, prodMedia := range prodMedias {
			// Check producer flags for this specific media type
			if consMedia.Direction == core.DirectionSendonly {
				if consMedia.Kind == core.KindVideo && !prod.videoEnabled {
					continue
				}
				if consMedia.Kind == core.KindAudio && !prod.audioEnabled {
					continue
				}
			}
			if consMedia.Direction == core.DirectionRecvonly && !prod.backchannelEnabled {
				continue
			}

			prodCodec, _ := prodMedia.MatchMedia(consMedia)
			if prodCodec != nil {
				found = true
				break
			}

			// For backchannel with mixing support, allow codec mismatch
			// The mixer will handle transcoding
			// Note: prodMedia.Direction=sendonly means producer sends TO camera (backchannel)
			//       consMedia.Direction=recvonly means consumer receives FROM client (backchannel)
			if prodMedia.Direction == core.DirectionSendonly &&
				consMedia.Direction == core.DirectionRecvonly &&
				prodMedia.Kind == consMedia.Kind &&
				prod.mixingEnabled {
				found = true
				break
			}
		}

		if !found {
			return false
		}
	}

	return true
}

func (s *Stream) checkAndStartPrebuffer(cons core.Consumer) {
	// Use reflection to check if consumer has UsePrebuffer field
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
