package signal

import (
	"fmt"
	"log"
	"strings"

	"github.com/gameplay-clipper/pkg/profile"
)

// TextBannerDetector OCRs a declared region and fires an event when
// recognized text matches one of the signal's target strings — e.g.
// Valorant's "ACE"/"CLUTCH" callout, or Mortal Kombat's "FINISH HIM" /
// "FATALITY" prompt. One reusable detector kind, parameterized entirely by
// profile config (region + match strings), per game.
type TextBannerDetector struct{}

func (TextBannerDetector) Name() string { return "text_banner" }

func (TextBannerDetector) Detect(proxyFile string, prof *profile.Profile) ([]Event, error) {
	var events []Event
	_, tesseractOK := tesseractPath()

	for _, sig := range prof.Signals {
		if sig.Kind != profile.KindTextBanner {
			continue
		}
		if !tesseractOK {
			log.Printf("Warning: tesseract not found on PATH, skipping text_banner signal %q for %s", sig.Name, prof.Game)
			continue
		}
		if sig.Region == nil {
			return events, fmt.Errorf("text_banner signal %q has no region", sig.Name)
		}
		samples, err := sampleRegionText(proxyFile, *sig.Region, sig.SampleFPS)
		if err != nil {
			return events, fmt.Errorf("text_banner %q: %w", sig.Name, err)
		}
		events = append(events, matchTextBannerEvents(sig, samples)...)
	}
	return events, nil
}

// matchTextBannerEvents is pure: given a time-ordered sequence of OCR
// readings, emit one event per rising edge (banner appearing), not one per
// sampled frame the banner happens to still be on screen for.
func matchTextBannerEvents(sig profile.Signal, samples []ocrSample) []Event {
	var events []Event
	wasMatching := false
	for _, s := range samples {
		matched := containsAnyFold(s.Text, sig.Match)
		if matched && !wasMatching {
			events = append(events, Event{
				Kind: sig.Name, T: s.T, Confidence: 1.0,
				Payload: s.Text, Source: "text_banner",
			})
		}
		wasMatching = matched
	}
	return events
}

func containsAnyFold(text string, candidates []string) bool {
	upper := strings.ToUpper(text)
	for _, c := range candidates {
		if c != "" && strings.Contains(upper, strings.ToUpper(c)) {
			return true
		}
	}
	return false
}
