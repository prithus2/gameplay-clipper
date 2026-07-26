package signal

import (
	"fmt"
	"log"

	"github.com/gameplay-clipper/pkg/profile"
)

// SubtitleDetector finds cutscenes/dialogue by the presence of a dialogue
// caption in a bottom-center band. It's the God of War signal that actually
// works: GoW (esp. Ragnarök) renders cutscenes full-frame with NO letterbox
// bars — cropdetect finds nothing — but every story/dialogue beat shows a
// subtitle caption ("Heimdall: ...", "Atreus: ..."). We don't try to match
// specific words; we detect that a caption is *present* at all, which for God
// of War maps to "this is a story/cutscene moment worth clipping".
// See [[gameplay-clipper-gow-cutscene-signal]].
//
// This is why the region is OCR'd with heavy preprocessing (grayscale +
// upscale + binarization): GoW subtitles are semi-transparent white over a
// bright, varied background, which plain OCR reads as garbage. The threshold
// is tuned so gameplay frames with no caption OCR to near-empty, which is what
// makes "text present" a usable presence signal.
type SubtitleDetector struct{}

func (SubtitleDetector) Name() string { return "subtitle" }

// captionPreFilter binarizes the caption band so low-contrast subtitle text
// reads cleanly and captionless frames come back near-empty. Commas inside
// the lut expression are backslash-escaped because ffmpeg treats bare commas
// as filter-chain separators. Tuned against real gow_video3 proxy frames.
const captionPreFilter = `format=gray,scale=2*iw:2*ih,eq=contrast=2:brightness=0.05,lut=y=if(gt(val\,165)\,255\,0)`

// Cutscene-shaping tuning for the subtitle presence signal.
const (
	subtitleMinDurS = 4.0 // ignore <4s of isolated caption (a one-off popup)
	subtitleMaxGapS = 4.0 // bridge <=4s between caption lines within one scene
	subtitleStepS   = 4.0 // emit a fired event every 4s across a dialogue run
	subtitlePSM     = 6   // treat the band as a uniform text block
)

func (SubtitleDetector) Detect(proxyFile string, prof *profile.Profile) ([]Event, error) {
	var events []Event
	_, tesseractOK := tesseractPath()

	for i := range prof.Signals {
		sig := prof.Signals[i]
		if sig.Kind != profile.KindSubtitle {
			continue
		}
		if !tesseractOK {
			log.Printf("Warning: tesseract not found on PATH, skipping subtitle signal %q for %s", sig.Name, prof.Game)
			continue
		}
		if sig.Region == nil {
			return events, fmt.Errorf("subtitle signal %q has no region", sig.Name)
		}

		samples, err := sampleRegionTextOpts(proxyFile, *sig.Region, sig.SampleFPS,
			ocrOpts{preFilter: captionPreFilter, psm: subtitlePSM})
		if err != nil {
			return events, fmt.Errorf("subtitle %q: %w", sig.Name, err)
		}

		presence := make([]PresenceSample, len(samples))
		for j, s := range samples {
			presence[j] = PresenceSample{T: s.T, Active: isCaptionPresent(s.Text)}
		}
		segments := GroupCutscenes(presence, subtitleMinDurS, subtitleMaxGapS)
		for _, seg := range segments {
			for _, t := range emitTimes(seg, subtitleStepS) {
				events = append(events, Event{
					Kind:       sig.Name,
					T:          t,
					Confidence: 0.8,
					Payload:    fmt.Sprintf("dialogue %.1fs-%.1fs", seg.Start, seg.End),
					Source:     "subtitle",
				})
			}
		}
	}
	return events, nil
}

// isCaptionPresent decides whether an OCR reading of the caption band is a
// real dialogue line versus the near-empty noise a captionless frame produces
// after binarization. It counts word-like alphabetic tokens rather than
// matching specific words, since the dialogue text is arbitrary. Tuned so
// captionless proxy frames (which OCR to things like ":" or ": | 7 :") fall
// below the bar while real lines clear it.
func isCaptionPresent(text string) bool {
	letters := 0
	words := 0
	runLen := 0
	countTok := func() {
		if runLen >= 2 {
			words++
		}
		runLen = 0
	}
	for _, r := range text {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			letters++
			runLen++
		} else {
			countTok()
		}
	}
	countTok()
	return letters >= 12 && words >= 3
}
