package signal

import (
	"fmt"
	"log"
	"strings"

	"github.com/gameplay-clipper/pkg/profile"
)

// KillFeedDetector OCRs a killfeed region and fires an event each time the
// recognized text changes to a new non-empty value — i.e. a new kill feed
// entry appeared.
//
// This is a known approximation: real kill feeds distinguish entries by
// weapon icons and player-color as much as by text, which plain OCR can't
// see. It's good enough to know "something new appeared here" for scoring
// purposes, but a false-negative (OCR garbles a name) or false-positive
// (two kills close together OCR as one blob) is possible. If this proves
// too noisy against golden footage, upgrade to icon/template matching
// rather than tuning OCR further.
type KillFeedDetector struct{}

func (KillFeedDetector) Name() string { return "kill_feed" }

func (KillFeedDetector) Detect(proxyFile string, prof *profile.Profile) ([]Event, error) {
	var events []Event
	_, tesseractOK := tesseractPath()

	for _, sig := range prof.Signals {
		if sig.Kind != profile.KindKillFeed {
			continue
		}
		if !tesseractOK {
			log.Printf("Warning: tesseract not found on PATH, skipping kill_feed signal %q for %s", sig.Name, prof.Game)
			continue
		}
		if sig.Region == nil {
			return events, fmt.Errorf("kill_feed signal %q has no region", sig.Name)
		}
		samples, err := sampleRegionText(proxyFile, *sig.Region, sig.SampleFPS)
		if err != nil {
			return events, fmt.Errorf("kill_feed %q: %w", sig.Name, err)
		}
		events = append(events, matchKillFeedEvents(sig, samples)...)
	}
	return events, nil
}

// matchKillFeedEvents is pure: emits an event each time the OCR'd text
// changes to a new non-empty value, with a simple debounce so a feed entry
// sitting on screen across consecutive samples fires once, not per frame.
func matchKillFeedEvents(sig profile.Signal, samples []ocrSample) []Event {
	var events []Event
	prevText := ""
	for _, s := range samples {
		norm := strings.TrimSpace(s.Text)
		if norm != "" && norm != prevText {
			events = append(events, Event{
				Kind: sig.Name, T: s.T, Confidence: 0.8,
				Payload: norm, Source: "kill_feed",
			})
		}
		if norm != "" {
			prevText = norm
		}
	}
	return events
}
