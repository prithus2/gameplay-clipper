// Package scoring turns raw signal events into candidate clip windows via a
// deterministic, auditable function — not an LLM guess. score(t) = sum of
// weight * decay-activation across fired signals; accepted peaks above an
// archetype's threshold become candidates, with round-scoped boundary
// resolution so a multi-kill run (an Ace) becomes one window spanning the
// whole run instead of an isolated window around a single kill.
//
// This package is pure: no ffmpeg, no network, no filesystem. That's
// deliberate — it's the piece a game-patch-breaks-everything bug or a
// premature-clip complaint should be debuggable and testable against,
// without needing real video.
package scoring

import "sort"

// FiredSignal is one event that contributed to a candidate, kept for
// review-UI display ("signals fired: kill_feed_entry x5, ace_banner") and
// for passing structured evidence to the LLM judge.
type FiredSignal struct {
	Kind       string
	T          float64
	Confidence float64
	Payload    string
	Source     string
}

// Candidate is a scored clip window for one archetype.
type Candidate struct {
	Archetype    string
	TPeak        float64
	Start        float64
	End          float64
	Score        float64
	SignalsFired []FiredSignal
}

// Event is the subset of signal.Event this package needs. Defined locally
// (rather than importing pkg/signal) so scoring has zero dependency on the
// detection layer — it only needs data, not detector types.
type Event struct {
	Kind       string
	T          float64
	Confidence float64
	Payload    string
	Source     string
}

// SignalSpec is the subset of profile.Signal this package needs.
type SignalSpec struct {
	Name         string
	IsBoundary   bool // true for round_boundary-kind signals: structural, not scored
	Weight       map[string]float64
	BoundaryRole string // "open", "close", or ""
	DecaySeconds float64
}

// ArchetypeSpec is the subset of profile.Archetype this package needs.
type ArchetypeSpec struct {
	Name       string
	Threshold  float64
	MinLengthS float64
	MaxLengthS float64
	LeadPadS   float64
	TrailPadS  float64

	// StreakMergeGapS, if > 0, merges any relevant events within this many
	// seconds of each other (transitively) into one window spanning the
	// first kill through the last, instead of scoring each detected event
	// as its own isolated peak. Unlike round-scoped grouping this needs no
	// explicit boundary signal — it only needs events to not be too far
	// apart — which is what a fast-paced kill streak looks like even when
	// some of the intermediate/tail banners were never individually OCR'd.
	StreakMergeGapS float64
}

// Config bundles everything Score needs from a profile, decoupled from the
// profile package's YAML-shaped types.
type Config struct {
	Signals             []SignalSpec
	Archetypes          []ArchetypeSpec
	RoundBoundarySignal string
	NMSRadiusS           float64
}

func (c *Config) signalByName(name string) *SignalSpec {
	for i := range c.Signals {
		if c.Signals[i].Name == name {
			return &c.Signals[i]
		}
	}
	return nil
}

// Score computes candidates for every archetype and resolves overlaps
// across archetypes, keeping the highest-confidence candidate wherever two
// windows overlap.
func Score(events []Event, cfg Config, duration float64) []Candidate {
	var all []Candidate
	for _, arch := range cfg.Archetypes {
		all = append(all, scoreArchetype(events, cfg, arch, duration)...)
	}
	return resolveOverlaps(all)
}

// relevantEvent pairs a fired event with the signal spec and per-archetype
// weight that made it relevant for the archetype currently being scored.
type relevantEvent struct {
	ev     Event
	spec   *SignalSpec
	weight float64
}

func scoreArchetype(events []Event, cfg Config, arch ArchetypeSpec, duration float64) []Candidate {
	var relevant []relevantEvent
	var roundBoundaries []float64
	for _, ev := range events {
		spec := cfg.signalByName(ev.Kind)
		if spec == nil {
			continue
		}
		if spec.IsBoundary {
			roundBoundaries = append(roundBoundaries, ev.T)
			continue
		}
		w := spec.Weight[arch.Name]
		if w == 0 {
			continue
		}
		relevant = append(relevant, relevantEvent{ev: ev, spec: spec, weight: w})
	}
	if len(relevant) == 0 {
		return nil
	}
	sort.Float64s(roundBoundaries)

	activationAt := func(t float64) float64 {
		sum := 0.0
		for _, r := range relevant {
			decay := r.spec.DecaySeconds
			if decay <= 0 {
				decay = 5
			}
			dt := t - r.ev.T
			if dt < 0 {
				dt = -dt
			}
			if dt > decay {
				continue
			}
			sum += r.weight * (1 - dt/decay)
		}
		return sum
	}

	// Candidate peak locations: every relevant event's own timestamp. For a
	// sum of triangular kernels this is where the true local maxima are, or
	// close enough to them for threshold/NMS purposes.
	type peak struct {
		t     float64
		score float64
	}
	var peaks []peak
	for _, r := range relevant {
		s := activationAt(r.ev.T)
		if s >= arch.Threshold {
			peaks = append(peaks, peak{t: r.ev.T, score: s})
		}
	}
	if len(peaks) == 0 {
		return nil
	}

	nmsRadius := cfg.NMSRadiusS
	if nmsRadius <= 0 {
		nmsRadius = 5
	}
	sort.Slice(peaks, func(i, j int) bool { return peaks[i].score > peaks[j].score })
	var accepted []peak
	for _, p := range peaks {
		tooClose := false
		for _, a := range accepted {
			if abs(p.t-a.t) < nmsRadius {
				tooClose = true
				break
			}
		}
		if !tooClose {
			accepted = append(accepted, p)
		}
	}

	var candidates []Candidate
	for _, p := range accepted {
		start, end := resolveBoundary(p.t, cfg, arch, relevant, roundBoundaries, duration)
		start, end = enforceLength(start, end, arch, duration)

		var fired []FiredSignal
		for _, r := range relevant {
			if r.ev.T >= start && r.ev.T <= end {
				fired = append(fired, FiredSignal{
					Kind: r.ev.Kind, T: r.ev.T, Confidence: r.ev.Confidence,
					Payload: r.ev.Payload, Source: r.ev.Source,
				})
			}
		}

		candidates = append(candidates, Candidate{
			Archetype: arch.Name, TPeak: p.t, Start: start, End: end,
			Score: p.score, SignalsFired: fired,
		})
	}
	return candidates
}

// resolveBoundary decides a window's [start,end], in priority order:
//  1. Round-scoped grouping (cfg.RoundBoundarySignal set): every relevant
//     event within the same round as t_peak — however far apart — merges
//     into one window. Fixes the "Ace truncated to 9s" failure mode.
//  2. Streak-gap merging (arch.StreakMergeGapS set): every relevant event
//     transitively within that gap of t_peak merges into one window, with
//     no explicit boundary signal required.
//  3. Explicit boundary_role open/close signal pairs.
//  4. Fixed lead/trail envelope around t_peak.
func resolveBoundary(tPeak float64, cfg Config, arch ArchetypeSpec, relevant []relevantEvent, roundBoundaries []float64, duration float64) (float64, float64) {
	if cfg.RoundBoundarySignal != "" && len(roundBoundaries) > 0 {
		roundStart, roundEnd := 0.0, duration
		for _, b := range roundBoundaries {
			if b <= tPeak && b > roundStart {
				roundStart = b
			}
			if b > tPeak && b < roundEnd {
				roundEnd = b
			}
		}
		minT, maxT := tPeak, tPeak
		found := false
		for _, r := range relevant {
			if r.ev.T >= roundStart && r.ev.T < roundEnd {
				if !found || r.ev.T < minT {
					minT = r.ev.T
				}
				if !found || r.ev.T > maxT {
					maxT = r.ev.T
				}
				found = true
			}
		}
		return minT - arch.LeadPadS, maxT + arch.TrailPadS
	}

	if arch.StreakMergeGapS > 0 {
		// Only events strong enough to have qualified as a peak on their
		// own (weight >= threshold) act as bridge points. Without this, a
		// weak corroborating signal like a low-weight single_kill_banner
		// (deliberately scored low — "not a highlight on its own") would
		// chain-bridge every routine kill across the whole match into one
		// giant window, since those happen every 10-15s throughout normal
		// play. Bridging on strong events only keeps the merge scoped to
		// an actual multi-kill streak.
		minT, maxT := streakCluster(tPeak, arch.StreakMergeGapS, strongEvents(relevant, arch.Threshold))
		return minT - arch.LeadPadS, maxT + arch.TrailPadS
	}

	hasBoundaryRoles := false
	for _, r := range relevant {
		if r.spec.BoundaryRole != "" {
			hasBoundaryRoles = true
			break
		}
	}
	if hasBoundaryRoles {
		openT, closeT := tPeak, tPeak
		haveOpen, haveClose := false, false
		for _, r := range relevant {
			switch r.spec.BoundaryRole {
			case "open":
				if r.ev.T <= tPeak && (!haveOpen || r.ev.T > openT) {
					openT, haveOpen = r.ev.T, true
				}
			case "close":
				if r.ev.T >= tPeak && (!haveClose || r.ev.T < closeT) {
					closeT, haveClose = r.ev.T, true
				}
			}
		}
		return openT - arch.LeadPadS, closeT + arch.TrailPadS
	}

	return tPeak - arch.LeadPadS, tPeak + arch.TrailPadS
}

// strongEvents returns only the events whose own weight would have cleared
// the archetype's threshold in isolation — i.e. events that could
// legitimately anchor a candidate on their own, as opposed to low-weight
// corroborating signals that only add color to a window someone else opened.
func strongEvents(relevant []relevantEvent, threshold float64) []relevantEvent {
	var out []relevantEvent
	for _, r := range relevant {
		if r.weight >= threshold {
			out = append(out, r)
		}
	}
	return out
}

// streakCluster grows [tPeak, tPeak] outward, absorbing any relevant event
// whose gap to the current boundary is <= gapS, repeating until nothing
// more qualifies. This is a transitive closure: event A can pull in event C
// through B even if A and C alone are further apart than gapS, as long as
// each step in the chain is within gapS. That's what lets a streak survive
// a missed detection in the middle — e.g. DOUBLE KILL and QUADRAKILL
// banners 9.5s apart still merge even though the TRIPLE KILL banner
// between them was never OCR'd.
func streakCluster(tPeak, gapS float64, relevant []relevantEvent) (minT, maxT float64) {
	minT, maxT = tPeak, tPeak
	for {
		grew := false
		for _, r := range relevant {
			t := r.ev.T
			if t < minT && minT-t <= gapS {
				minT = t
				grew = true
			}
			if t > maxT && t-maxT <= gapS {
				maxT = t
				grew = true
			}
		}
		if !grew {
			break
		}
	}
	return minT, maxT
}

func enforceLength(start, end float64, arch ArchetypeSpec, duration float64) (float64, float64) {
	if start < 0 {
		start = 0
	}
	if end > duration && duration > 0 {
		end = duration
	}
	if end < start {
		end = start
	}

	if arch.MinLengthS > 0 && end-start < arch.MinLengthS {
		deficit := arch.MinLengthS - (end - start)
		end += deficit / 2
		start -= deficit / 2
		if start < 0 {
			end -= start
			start = 0
		}
		if duration > 0 && end > duration {
			start -= end - duration
			end = duration
			if start < 0 {
				start = 0
			}
		}
	}
	if arch.MaxLengthS > 0 && end-start > arch.MaxLengthS {
		// Trim from the front, keeping the tail anchored. For a round-scoped
		// or boundary-resolved window, the tail is the payoff (an Ace
		// banner, a close event) — recentering on tPeak instead would
		// discard the resolved start and reintroduce exactly the
		// "truncated highlight" bug this boundary resolution exists to fix.
		start = end - arch.MaxLengthS
		if start < 0 {
			start = 0
		}
	}
	return start, end
}

// resolveOverlaps keeps only the highest-confidence candidate wherever two
// windows (from the same or different archetypes) overlap.
func resolveOverlaps(candidates []Candidate) []Candidate {
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Score > candidates[j].Score })
	var kept []Candidate
	for _, c := range candidates {
		overlaps := false
		for _, k := range kept {
			if c.Start < k.End && c.End > k.Start {
				overlaps = true
				break
			}
		}
		if !overlaps {
			kept = append(kept, c)
		}
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].Start < kept[j].Start })
	return kept
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
