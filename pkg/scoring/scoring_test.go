package scoring

import "testing"

func valoLikeConfig() Config {
	return Config{
		NMSRadiusS: 4,
		Signals: []SignalSpec{
			{Name: "kill_feed_entry", Weight: map[string]float64{"ace": 1.0}, DecaySeconds: 8},
			{Name: "ace_banner", Weight: map[string]float64{"ace": 3.0}, DecaySeconds: 3},
			{Name: "round_reset", IsBoundary: true},
		},
		Archetypes: []ArchetypeSpec{
			{Name: "ace", Threshold: 2.0, MinLengthS: 12, MaxLengthS: 100, LeadPadS: 2, TrailPadS: 3},
		},
		RoundBoundarySignal: "round_reset",
	}
}

// This is the exact bug the user reported: an Ace's kills are spread far
// enough apart (well beyond any sane NMS/merge radius) that gap-based
// merging would chop it into isolated single-kill windows, missing the
// build-up. Round-scoped grouping must span the whole run instead.
func TestRoundScopedGroupingCapturesFullAceRun(t *testing.T) {
	cfg := valoLikeConfig()
	events := []Event{
		{Kind: "round_reset", T: 0},
		{Kind: "kill_feed_entry", T: 10, Confidence: 0.8},
		{Kind: "kill_feed_entry", T: 25, Confidence: 0.8}, // 15s gap — would exceed old MergeGap=8
		{Kind: "kill_feed_entry", T: 50, Confidence: 0.8}, // 25s gap
		{Kind: "kill_feed_entry", T: 70, Confidence: 0.8},
		{Kind: "kill_feed_entry", T: 95, Confidence: 0.8},
		{Kind: "ace_banner", T: 100, Confidence: 1.0},
		{Kind: "round_reset", T: 110},
	}

	candidates := Score(events, cfg, 200)
	if len(candidates) != 1 {
		t.Fatalf("expected exactly 1 merged candidate for the whole Ace run, got %d: %+v", len(candidates), candidates)
	}
	c := candidates[0]
	if c.Archetype != "ace" {
		t.Errorf("Archetype = %q, want ace", c.Archetype)
	}
	// Window must start at/near the FIRST kill (t=10), not the last one.
	if c.Start > 9 {
		t.Errorf("Start = %.2f, want <= ~8 (first kill at t=10 minus lead pad) — clip is still truncating the build-up", c.Start)
	}
	// Window must extend through the Ace banner (t=100).
	if c.End < 100 {
		t.Errorf("End = %.2f, want >= 100 (must include the ace_banner) — clip is being cut short", c.End)
	}
	if len(c.SignalsFired) != 6 {
		t.Errorf("expected all 6 in-round scoring signals attributed to the candidate, got %d", len(c.SignalsFired))
	}
}

func TestIsolatedSignalBelowThresholdProducesNoCandidate(t *testing.T) {
	cfg := valoLikeConfig()
	events := []Event{
		{Kind: "kill_feed_entry", T: 10, Confidence: 0.8}, // weight 1.0 alone, threshold is 2.0
	}
	candidates := Score(events, cfg, 200)
	if len(candidates) != 0 {
		t.Errorf("expected 0 candidates (single kill below ace threshold), got %d: %+v", len(candidates), candidates)
	}
}

func TestDecayEnvelopeIgnoresFarEvents(t *testing.T) {
	cfg := valoLikeConfig()
	events := []Event{
		{Kind: "ace_banner", T: 100, Confidence: 1.0},
		{Kind: "kill_feed_entry", T: 10, Confidence: 0.8}, // 90s away, decay is 8s — must not contribute
	}
	candidates := Score(events, cfg, 200)
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate (ace_banner alone clears threshold), got %d", len(candidates))
	}
	// Score should be close to the ace_banner's own weight (3.0), not boosted
	// by the far-away kill_feed_entry.
	if candidates[0].Score < 2.9 || candidates[0].Score > 3.1 {
		t.Errorf("Score = %.2f, want ~3.0 (far event should not contribute)", candidates[0].Score)
	}
}

func TestBoundaryRoleOpenCloseResolution(t *testing.T) {
	cfg := Config{
		NMSRadiusS: 3,
		Signals: []SignalSpec{
			{Name: "combo_start", Weight: map[string]float64{"combo": 1.0}, DecaySeconds: 20, BoundaryRole: "open"},
			{Name: "combo_finish", Weight: map[string]float64{"combo": 2.0}, DecaySeconds: 5, BoundaryRole: "close"},
		},
		Archetypes: []ArchetypeSpec{
			{Name: "combo", Threshold: 1.5, MinLengthS: 1, MaxLengthS: 60, LeadPadS: 1, TrailPadS: 1},
		},
	}
	events := []Event{
		{Kind: "combo_start", T: 5},
		{Kind: "combo_finish", T: 15},
	}
	candidates := Score(events, cfg, 100)
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	c := candidates[0]
	if c.Start != 4 { // combo_start.T(5) - LeadPadS(1)
		t.Errorf("Start = %.2f, want 4 (open event minus lead pad)", c.Start)
	}
	if c.End != 16 { // combo_finish.T(15) + TrailPadS(1)
		t.Errorf("End = %.2f, want 16 (close event plus trail pad)", c.End)
	}
}

func TestFixedEnvelopeFallbackWithNoBoundarySignals(t *testing.T) {
	cfg := Config{
		NMSRadiusS: 5,
		Signals: []SignalSpec{
			{Name: "loudness_spike", Weight: map[string]float64{"generic": 1.0}, DecaySeconds: 6},
		},
		Archetypes: []ArchetypeSpec{
			{Name: "generic", Threshold: 0.5, MinLengthS: 8, MaxLengthS: 60, LeadPadS: 6, TrailPadS: 3},
		},
	}
	events := []Event{{Kind: "loudness_spike", T: 50}}
	candidates := Score(events, cfg, 200)
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	c := candidates[0]
	if c.Start != 44 || c.End != 53 {
		t.Errorf("got [%.2f, %.2f], want [44, 53] (fixed lead/trail pad around t=50)", c.Start, c.End)
	}
}

func TestOverlapResolutionKeepsHighestConfidence(t *testing.T) {
	candidates := []Candidate{
		{Archetype: "a", Start: 10, End: 20, Score: 1.0},
		{Archetype: "b", Start: 15, End: 25, Score: 3.0}, // overlaps the above, higher score
		{Archetype: "c", Start: 100, End: 110, Score: 0.5}, // no overlap, independent
	}
	kept := resolveOverlaps(candidates)
	if len(kept) != 2 {
		t.Fatalf("expected 2 candidates after overlap resolution, got %d: %+v", len(kept), kept)
	}
	if kept[0].Archetype != "b" {
		t.Errorf("expected the higher-scoring overlapping candidate (b) to survive, got %q", kept[0].Archetype)
	}
	if kept[1].Archetype != "c" {
		t.Errorf("expected the non-overlapping candidate (c) to survive independently, got %q", kept[1].Archetype)
	}
}

func TestMinLengthEnforced(t *testing.T) {
	cfg := Config{
		NMSRadiusS: 5,
		Signals: []SignalSpec{
			{Name: "fatality_prompt", Weight: map[string]float64{"fatality": 3.0}, DecaySeconds: 4},
		},
		Archetypes: []ArchetypeSpec{
			{Name: "fatality", Threshold: 1.0, MinLengthS: 6, MaxLengthS: 15, LeadPadS: 1, TrailPadS: 1},
		},
	}
	events := []Event{{Kind: "fatality_prompt", T: 50}}
	candidates := Score(events, cfg, 200)
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	c := candidates[0]
	if got := c.End - c.Start; got < 6 {
		t.Errorf("length = %.2f, want >= MinLengthS(6) even though lead+trail pad alone give 2", got)
	}
}

// This is the exact bug reported against a real valo_01.mp4 clip: a
// DOUBLE KILL banner at t=104 and a QUADRAKILL banner at t=113.5 (9.5s
// apart — beyond NMSRadiusS, so each independently clears threshold and
// becomes its own peak) with no round-boundary signals available. Without
// streak merging these would resolve to two separate isolated windows, and
// resolveOverlaps would arbitrarily keep only one of them — silently
// dropping the run's other end rather than covering the whole streak.
func TestStreakMergeGapBridgesGapBetweenDetectedBanners(t *testing.T) {
	cfg := Config{
		NMSRadiusS: 4,
		Signals: []SignalSpec{
			{Name: "kill_streak_banner", Weight: map[string]float64{"multikill": 1.5}, DecaySeconds: 4},
		},
		Archetypes: []ArchetypeSpec{
			{Name: "multikill", Threshold: 1.5, MinLengthS: 8, MaxLengthS: 60, LeadPadS: 2, TrailPadS: 6, StreakMergeGapS: 15},
		},
	}
	events := []Event{
		{Kind: "kill_streak_banner", T: 104, Payload: "DOUBLE KILL"},
		{Kind: "kill_streak_banner", T: 113.5, Payload: "QUADRAKILL"}, // the TRIPLE KILL banner between these was never OCR'd
	}
	candidates := Score(events, cfg, 200)
	if len(candidates) != 1 {
		t.Fatalf("expected the two banners to merge into 1 candidate, got %d: %+v", len(candidates), candidates)
	}
	c := candidates[0]
	if c.Start > 102 {
		t.Errorf("Start = %.2f, want <= 102 (must include the first kill at t=104 minus lead pad)", c.Start)
	}
	if c.End < 119.5 {
		t.Errorf("End = %.2f, want >= 119.5 (must include the last detected kill at t=113.5 plus trail pad)", c.End)
	}
}

func TestMaxLengthClampPreservesTailNotCenter(t *testing.T) {
	cfg := valoLikeConfig() // ace archetype: MaxLengthS=100, TrailPadS=3
	events := []Event{
		{Kind: "round_reset", T: 0},
		{Kind: "kill_feed_entry", T: 5, Confidence: 0.8},
		{Kind: "ace_banner", T: 400, Confidence: 1.0}, // absurdly long round, must be clamped
		{Kind: "round_reset", T: 410},
	}
	candidates := Score(events, cfg, 500)
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	c := candidates[0]
	if got := c.End - c.Start; got > 100.01 {
		t.Errorf("length = %.2f, want <= MaxLengthS(100)", got)
	}
	// The clamp must trim from the front and keep the payoff (ace_banner +
	// trail pad) intact — recentering on t_peak would cut the ending short
	// instead, which is worse than cutting extra build-up.
	wantEnd := 400.0 + 3.0
	if c.End != wantEnd {
		t.Errorf("End = %.2f, want %.2f (tail/payoff must be preserved, not trimmed)", c.End, wantEnd)
	}
}
