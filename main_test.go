package main

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/gameplay-clipper/pkg/profile"
	"github.com/gameplay-clipper/pkg/scoring"
	"github.com/gameplay-clipper/pkg/signal"
)

// TestValoGoldenRegressionCapturesBothAces is a synthetic regression test
// for the round-scoped-grouping logic in pkg/scoring, wired through the
// real valo.yaml profile loader. The events (kill_streak_banner / ace_banner
// / round_reset timestamps) are entirely hand-authored to model a
// hypothetical Competitive-mode Ace (round start -> 5 kills -> ACE banner ->
// next round) — they are NOT derived from valo_01.mp4, which (per real-frame
// OCR verification on 2026-07-06) is actually Deathmatch footage with no
// rounds and no ACE banner at all, only a running kill-streak counter
// (SINGLE/DOUBLE/TRIPLE/QUADRA/PENTA KILL). There is also currently no
// Detector for round_boundary events, so this scenario can't happen for real
// yet regardless of game mode. This test exists purely to catch a
// regression in the grouping math: a run's window must span from its first
// kill through its confirmation banner, not be truncated to an isolated
// window around a single event. Replace with real Competitive-footage
// events once that footage and a round_boundary detector both exist.
func TestValoGoldenRegressionCapturesBothAces(t *testing.T) {
	data, err := os.ReadFile("testdata/valorant/golden/expected_events.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var events []signal.Event
	if err := json.Unmarshal(data, &events); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}

	prof, err := profile.Load("profiles/valo.yaml")
	if err != nil {
		t.Fatalf("load profile: %v", err)
	}

	candidates := scoring.Score(toScoringEvents(events), toScoringConfig(prof), 270)

	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidates (two Aces), got %d: %+v", len(candidates), candidates)
	}

	wantRuns := []struct{ firstKill, banner float64 }{
		{firstKill: 15, banner: 100},
		{firstKill: 200, banner: 252},
	}
	for i, want := range wantRuns {
		c := candidates[i]
		if c.Start > want.firstKill-1 {
			t.Errorf("candidate %d: Start=%.1f, want <= ~%.1f (must include the first kill of the run, not just the banner)", i, c.Start, want.firstKill-2)
		}
		if c.End < want.banner {
			t.Errorf("candidate %d: End=%.1f, want >= %.1f (must include the Ace confirmation)", i, c.End, want.banner)
		}
	}
}

func TestParseReelSpan(t *testing.T) {
	x0, x1, err := parseReelSpan("0.14-0.87")
	if err != nil || x0 != 0.14 || x1 != 0.87 {
		t.Errorf("parseReelSpan(\"0.14-0.87\") = %v,%v,%v want 0.14,0.87,nil", x0, x1, err)
	}
	// Empty means "no override", not an error: it is the default.
	if x0, x1, err := parseReelSpan(""); err != nil || x0 != 0 || x1 != 0 {
		t.Errorf("empty span = %v,%v,%v want 0,0,nil", x0, x1, err)
	}
	// A typo here silently reframes every clip in the run, so it must fail loudly.
	for _, bad := range []string{"0.5", "0.2-0.4-0.6", "a-b", "0.8-0.2", "-0.1-0.5", "0.5-1.4", "0.5-0.5"} {
		if _, _, err := parseReelSpan(bad); err == nil {
			t.Errorf("parseReelSpan(%q) accepted a bad span", bad)
		}
	}
}
