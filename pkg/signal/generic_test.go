package signal

import (
	"os"
	"testing"
)

func TestParseEbur128Synthetic(t *testing.T) {
	// Two entries far above the mean should be flagged; one near it should not.
	out := `[Parsed_ebur128_0 @ 0x0] t: 1.0       M: -60.0 S:-23.0     I: -23.0 LUFS
[Parsed_ebur128_0 @ 0x0] t: 2.0       M: -60.0 S:-23.0     I: -23.0 LUFS
[Parsed_ebur128_0 @ 0x0] t: 3.0       M: -10.0 S:-23.0     I: -23.0 LUFS
[Parsed_ebur128_0 @ 0x0] t: 4.0       M: -60.0 S:-23.0     I: -23.0 LUFS
[Parsed_ebur128_0 @ 0x0] t: 5.0       M: -8.0  S:-23.0     I: -23.0 LUFS
`
	spikes := ParseEbur128(out)
	if len(spikes) != 2 {
		t.Fatalf("expected 2 spikes, got %d: %v", len(spikes), spikes)
	}
	if spikes[0] != 3.0 || spikes[1] != 5.0 {
		t.Errorf("unexpected spike timestamps: %v", spikes)
	}
}

func TestParseScenesSynthetic(t *testing.T) {
	out := `frame:100 pts:3000 pts_time:41.43
lavfi.scene_score=0.412000
frame:200 pts:6000 pts_time:52.77
lavfi.scene_score=0.500000
`
	scenes := ParseScenes(out)
	if len(scenes) != 2 {
		t.Fatalf("expected 2 scene changes, got %d: %v", len(scenes), scenes)
	}
}

// This is the actual captured output from raw_clips/valo/valo_01_proxy.mp4
// (a real match containing two Aces) that motivated the whole per-game
// signal redesign: zero loudness spikes across the match, and every
// "scene change" turned out to be the player opening the Tab scoreboard,
// not a kill. Locking this in as a regression fixture so the generic
// detector's known failure mode on this footage doesn't silently change
// (and so it's obvious in CI that generic-only detection is not enough for
// this game — that's the point of the per-game profile).
func TestGenericDetectorOnRealValoFixture(t *testing.T) {
	ebur, err := os.ReadFile("../../testdata/valorant/golden/ebur128_stderr.txt")
	if err != nil {
		t.Skipf("golden fixture not present: %v", err)
	}
	scenesRaw, err := os.ReadFile("../../testdata/valorant/golden/scenes.txt")
	if err != nil {
		t.Skipf("golden fixture not present: %v", err)
	}

	spikes := ParseEbur128(string(ebur))
	if len(spikes) != 0 {
		t.Errorf("expected 0 loudness spikes on this fixture (documented finding), got %d: %v", len(spikes), spikes)
	}

	scenes := ParseScenes(string(scenesRaw))
	if len(scenes) != 6 {
		t.Errorf("expected 6 scene-change events on this fixture (documented finding: all scoreboard toggles), got %d: %v", len(scenes), scenes)
	}
}
