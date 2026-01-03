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

		if cons.IsClosed() {
			log.Trace().Msgf("[streams] consumer closed during dial cons=%d prod=%d", consN, prodN)
			break
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
				switch prodMedia.Kind {
				case core.KindAudio:
					prevHasAudio = true
				case core.KindVideo:
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
		// Fallback mode: Collect all producers, then assign using BestFit
		// This ensures Video+Audio come from the same producer when possible
		log.Trace().Msgf("[streams] no bestfit, using collect-then-assign matching")

		// Phase 1: Dial all producers and collect their medias
		type producerInfo struct {
			prod   *Producer
			prodN  int
			medias []*core.Media
		}
		var availableProducers []producerInfo

		// Track which consMedias have been pre-negotiated (by index)
		preNegotiatedConsMedias := make(map[int]bool)

		for prodN, prod := range s.producers {
			// check for loop request
			if info, ok := cons.(core.Info); ok && prod.url == info.GetSource() {
				log.Trace().Msgf("[streams] skip prod=%d (loop)", prodN)
				continue
			}

			if prodErrors[prodN] != nil {
				log.Trace().Msgf("[streams] skip prod=%d (error)", prodN)
				continue
			}

			// Check #requirePrevAudio - skip if previous producers don't have audio
			if prod.requirePrevAudio && !prevHasAudio && prodN > 0 {
				log.Trace().Msgf("[streams] skip prod=%d (requires previous audio but none available)", prodN)
				continue
			}

			// Check #requirePrevVideo - skip if previous producers don't have video
			if prod.requirePrevVideo && !prevHasVideo && prodN > 0 {
				log.Trace().Msgf("[streams] skip prod=%d (requires previous video but none available)", prodN)
				continue
			}

			// Skip producers with backchannel disabled when consumer requests backchannel
			if consumerRequestsBackchannel && !prod.backchannelEnabled && !prod.videoExplicitlySet && !prod.audioExplicitlySet {
				log.Trace().Msgf("[streams] skip prod=%d (consumer requests backchannel, producer has #noBackchannel)", prodN)
				continue
			}

			// CRITICAL: Before dialing a producer with requirePrevAudio/Video,
			// we must SETUP all tracks from previous producers FIRST.
			// This is because dialing a dependent producer may trigger an internal consumer that
			// will PLAY the camera. If backchannel isn't SETUPed yet, it causes reconnect.
			if (prod.requirePrevAudio || prod.requirePrevVideo) && len(availableProducers) > 0 {
				log.Trace().Msgf("[streams] prod=%d has requirePrev*, pre-negotiating all tracks from previous producers", prodN)

				for _, pInfo := range availableProducers {
					for consMediaIdx, consMedia := range consMedias {
						// Check producer flags
						if consMedia.Direction == core.DirectionSendonly {
							if consMedia.Kind == core.KindVideo && !pInfo.prod.videoEnabled {
								continue
							}
							if consMedia.Kind == core.KindAudio && !pInfo.prod.audioEnabled {
								continue
							}
						}
						if consMedia.Direction == core.DirectionRecvonly && !pInfo.prod.backchannelEnabled {
							continue
						}

						for _, prodMedia := range pInfo.medias {
							prodCodec, consCodec := prodMedia.MatchMedia(consMedia)

							// Handle backchannel with mixer transcoding
							if prodCodec == nil &&
								prodMedia.Direction == core.DirectionSendonly &&
								consMedia.Direction == core.DirectionRecvonly &&
								prodMedia.Kind == consMedia.Kind &&
								pInfo.prod.mixingEnabled {
								if len(prodMedia.Codecs) > 0 && len(consMedia.Codecs) > 0 {
									prodCodec = prodMedia.Codecs[0]
									consCodec = consMedia.Codecs[0]
								}
							}

							if prodCodec == nil {
								continue
							}

							switch prodMedia.Direction {
							case core.DirectionRecvonly:
								// SETUP receive track (video/audio from camera)
								if track, err := pInfo.prod.GetTrack(prodMedia, prodCodec); err == nil {
									log.Trace().Msgf("[streams] pre-negotiate %s from prod=%d", prodMedia.Kind, pInfo.prodN)
									// Don't add to consumer yet - just SETUP on producer
									_ = track
									prodStarts = append(prodStarts, pInfo.prod)
								}

							case core.DirectionSendonly:
								// SETUP send track (backchannel to camera)
								if track, err := cons.(core.Producer).GetTrack(consMedia, consCodec); err == nil {
									if err := pInfo.prod.AddTrack(prodMedia, prodCodec, track); err == nil {
										log.Trace().Msgf("[streams] pre-negotiate backchannel to prod=%d", pInfo.prodN)
										prodStarts = append(prodStarts, pInfo.prod)
										preNegotiatedConsMedias[consMediaIdx] = true
									}
								}
							}
							break // Next consMedia
						}
					}
				}
			}

			if err = prod.Dial(); err != nil {
				log.Trace().Err(err).Msgf("[streams] dial prod=%d", prodN)
				prodErrors[prodN] = err
				continue
			}

			if cons.IsClosed() {
				log.Trace().Msgf("[streams] consumer closed during dial cons=%d prod=%d", consN, prodN)
				break
			}

			medias := prod.GetMedias()
			availableProducers = append(availableProducers, producerInfo{
				prod:   prod,
				prodN:  prodN,
				medias: medias,
			})

			// Track prevHasAudio/Video for dependent producers
			for _, media := range medias {
				prodMedias = append(prodMedias, media)
				if media.Direction == core.DirectionRecvonly {
					switch media.Kind {
					case core.KindAudio:
						prevHasAudio = true
					case core.KindVideo:
						prevHasVideo = true
					}
				}
			}

			log.Trace().Msgf("[streams] collected prod=%d medias=%d", prodN, len(medias))
		}

		// Phase 2: Find BestFit producer for Video+Audio (the one that can satisfy most)
		var bestFitIdx int = -1
		bestFitCount := 0

		for i, pInfo := range availableProducers {
			count := 0

			for _, consMedia := range consMedias {
				// Only count video and audio (sendonly = consumer wants to receive)
				if consMedia.Direction != core.DirectionSendonly {
					continue
				}

				// Check producer flags
				if consMedia.Kind == core.KindVideo && !pInfo.prod.videoEnabled {
					continue
				}
				if consMedia.Kind == core.KindAudio && !pInfo.prod.audioEnabled {
					continue
				}

				for _, prodMedia := range pInfo.medias {
					prodCodec, _ := prodMedia.MatchMedia(consMedia)
					if prodCodec != nil {
						count++
						break
					}
				}
			}

			log.Trace().Msgf("[streams] prod=%d can satisfy %d video+audio medias", pInfo.prodN, count)

			if count > bestFitCount {
				bestFitCount = count
				bestFitIdx = i
			}
		}

		// Phase 3: Assign tracks
		matchedConsMedias := make(map[int]bool)

		// Step 1: If BestFit producer can satisfy multiple medias (Video+Audio), use it
		if bestFitIdx >= 0 && bestFitCount >= 2 {
			pInfo := &availableProducers[bestFitIdx]
			log.Trace().Msgf("[streams] using bestfit prod=%d for %d medias (video+audio)", pInfo.prodN, bestFitCount)

			for consMediaIdx, consMedia := range consMedias {
				// Only handle video and audio here
				if consMedia.Direction != core.DirectionSendonly {
					continue
				}

				// Check producer flags
				if consMedia.Kind == core.KindVideo && !pInfo.prod.videoEnabled {
					continue
				}
				if consMedia.Kind == core.KindAudio && !pInfo.prod.audioEnabled {
					continue
				}

				for _, prodMedia := range pInfo.medias {
					prodCodec, consCodec := prodMedia.MatchMedia(consMedia)
					if prodCodec == nil {
						continue
					}

					log.Trace().Msgf("[streams] bestfit match cons=%d <= prod=%d media=%s", consN, pInfo.prodN, prodMedia.Kind)

					track, err := pInfo.prod.GetTrack(prodMedia, prodCodec)
					if err != nil {
						log.Info().Err(err).Msg("[streams] can't get track")
						continue
					}
					if err = cons.AddTrack(consMedia, consCodec, track); err != nil {
						log.Info().Err(err).Msg("[streams] can't add track")
						continue
					}

					prodStarts = append(prodStarts, pInfo.prod)
					matchedConsMedias[consMediaIdx] = true
					break
				}
			}
		}

		// Step 2: Negotiate tracks for producers that need them as source (for dependent producers)
		for _, pInfo := range availableProducers {
			// Check if this producer needs to provide source for a later producer
			needsToProvideAudio := false
			needsToProvideVideo := false

			for _, laterPInfo := range availableProducers {
				if laterPInfo.prodN <= pInfo.prodN {
					continue
				}
				if laterPInfo.prod.requirePrevAudio {
					needsToProvideAudio = true
				}
				if laterPInfo.prod.requirePrevVideo {
					needsToProvideVideo = true
				}
			}

			if !needsToProvideAudio && !needsToProvideVideo {
				continue
			}

			for _, prodMedia := range pInfo.medias {
				if prodMedia.Direction != core.DirectionRecvonly {
					continue
				}
				if len(prodMedia.Codecs) == 0 {
					continue
				}

				// Negotiate audio as source for dependent producer
				if prodMedia.Kind == core.KindAudio && needsToProvideAudio {
					codec := prodMedia.Codecs[0]
					if _, err = pInfo.prod.GetTrack(prodMedia, codec); err == nil {
						log.Trace().Msgf("[streams] negotiate audio prod=%d codec=%s", pInfo.prodN, codec.Name)
						prodStarts = append(prodStarts, pInfo.prod)
					}
				}

				// Negotiate video as source for dependent producer
				if prodMedia.Kind == core.KindVideo && needsToProvideVideo {
					codec := prodMedia.Codecs[0]
					if _, err = pInfo.prod.GetTrack(prodMedia, codec); err == nil {
						log.Trace().Msgf("[streams] negotiate video prod=%d codec=%s", pInfo.prodN, codec.Name)
						prodStarts = append(prodStarts, pInfo.prod)
					}
				}
			}
		}

		// Step 3: Assign remaining medias (including backchannel) from any producer
		for consMediaIdx, consMedia := range consMedias {
			if matchedConsMedias[consMediaIdx] {
				continue
			}
			// Skip if already pre-negotiated (e.g., backchannel was SETUPed before dependent producer dialed)
			if preNegotiatedConsMedias[consMediaIdx] {
				log.Trace().Msgf("[streams] skip consMedia=%d (already pre-negotiated)", consMediaIdx)
				continue
			}

			log.Trace().Msgf("[streams] assign remaining media=%s", consMedia)

			for _, pInfo := range availableProducers {
				// Check producer flags
				if consMedia.Direction == core.DirectionSendonly && consMedia.Kind == core.KindVideo && !pInfo.prod.videoEnabled {
					continue
				}
				if consMedia.Direction == core.DirectionSendonly && consMedia.Kind == core.KindAudio && !pInfo.prod.audioEnabled {
					continue
				}
				if consMedia.Direction == core.DirectionRecvonly && !pInfo.prod.backchannelEnabled {
					continue
				}

				for _, prodMedia := range pInfo.medias {
					prodCodec, consCodec := prodMedia.MatchMedia(consMedia)

					// Handle backchannel with mixer transcoding
					if prodCodec == nil &&
						prodMedia.Direction == core.DirectionSendonly &&
						consMedia.Direction == core.DirectionRecvonly &&
						prodMedia.Kind == consMedia.Kind &&
						pInfo.prod.mixingEnabled {
						if len(prodMedia.Codecs) > 0 && len(consMedia.Codecs) > 0 {
							prodCodec = prodMedia.Codecs[0]
							consCodec = consMedia.Codecs[0]
							log.Trace().Msgf("[streams] backchannel mixer transcode %s -> %s", consCodec.Name, prodCodec.Name)
						}
					}

					if prodCodec == nil {
						continue
					}

					var track *core.Receiver

					switch prodMedia.Direction {
					case core.DirectionRecvonly:
						log.Trace().Msgf("[streams] match cons=%d <= prod=%d media=%s", consN, pInfo.prodN, prodMedia.Kind)

						track, err = pInfo.prod.GetTrack(prodMedia, prodCodec)
						if err != nil {
							log.Info().Err(err).Msg("[streams] can't get track")
							continue
						}
						if err = cons.AddTrack(consMedia, consCodec, track); err != nil {
							log.Info().Err(err).Msg("[streams] can't add track")
							continue
						}

					case core.DirectionSendonly:
						log.Trace().Msgf("[streams] match cons=%d => prod=%d media=%s (backchannel)", consN, pInfo.prodN, prodMedia.Kind)

						track, err = cons.(core.Producer).GetTrack(consMedia, consCodec)
						if err != nil {
							log.Info().Err(err).Msg("[streams] can't get track")
							continue
						}
						if err = pInfo.prod.AddTrack(prodMedia, prodCodec, track); err != nil {
							log.Info().Err(err).Msg("[streams] can't add track")
							continue
						}
					}

					prodStarts = append(prodStarts, pInfo.prod)
					matchedConsMedias[consMediaIdx] = true

					if !consMedia.MatchAll() {
						break
					}
				}

				if matchedConsMedias[consMediaIdx] {
					break
				}
			}
		}
	}

	// stop producers if they don't have readers
	if s.pending.Add(-1) == 0 {
		s.stopProducers()
	}

	// Check if consumer was closed during dial
	if cons.IsClosed() {
		return errors.New("streams: consumer closed during dial")
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
