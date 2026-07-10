// Package signal defines the common event type every detector (generic or
// per-game) produces, and the Detector interface that lets the pipeline
// treat "loudness spike" and "killfeed OCR hit" uniformly.
package signal

import "github.com/gameplay-clipper/pkg/profile"

// Event is one detected occurrence at a point in time. Kind matches a
// profile.Signal.Name (e.g. "kill_feed_entry", "ace_banner") or one of the
// built-in generic kinds ("loudness_spike", "scene_cut").
type Event struct {
	Kind       string
	T          float64
	Confidence float64
	// Payload carries detector-specific detail for debugging/review (e.g.
	// the OCR'd text, or the matched keyword) — not used in scoring math.
	Payload string
	// Source names the detector that produced this event ("generic",
	// "text_banner", "kill_feed", "round_boundary", "asr"), independent of
	// Kind, so review UI / logs can show provenance.
	Source string
}

// Detector runs one detection method over a proxy video for a given
// profile and returns whatever events it found. Implementations must
// degrade gracefully (return an empty slice + log a warning) rather than
// error out when an optional external tool (tesseract, whisper.cpp) isn't
// installed — a missing tool should silently drop that signal, not crash
// the run or silently keep every candidate (the old GEMINI_API_KEY-missing
// behavior this project explicitly wants to move away from).
type Detector interface {
	Name() string
	Detect(proxyFile string, prof *profile.Profile) ([]Event, error)
}

// Run executes every applicable detector for a profile and returns the
// combined, unsorted event list. A detector returning an error for one
// signal shouldn't abort the whole pipeline — errors are logged by the
// caller and that detector's events are simply omitted.
func Run(proxyFile string, prof *profile.Profile, detectors []Detector) ([]Event, []error) {
	var events []Event
	var errs []error
	for _, d := range detectors {
		evs, err := d.Detect(proxyFile, prof)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		events = append(events, evs...)
	}
	return events, errs
}
