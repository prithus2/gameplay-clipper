package signal

import (
	"testing"

	"github.com/gameplay-clipper/pkg/profile"
)

func TestParseWhisperJSON(t *testing.T) {
	data := []byte(`{
		"transcription": [
			{"offsets": {"from": 1000, "to": 2500}, "text": " nice shot"},
			{"offsets": {"from": 99000, "to": 101000}, "text": " ACE! What a round."}
		]
	}`)
	segs, err := parseWhisperJSON(data)
	if err != nil {
		t.Fatalf("parseWhisperJSON: %v", err)
	}
	if len(segs) != 2 {
		t.Fatalf("expected 2 segments, got %d", len(segs))
	}
	if segs[1].Start != 99.0 {
		t.Errorf("Start = %.2f, want 99.0", segs[1].Start)
	}
	if segs[1].Text != "ACE! What a round." {
		t.Errorf("Text = %q, unexpected trimming", segs[1].Text)
	}
}

func TestMatchASRKeywordEvents(t *testing.T) {
	sig := profile.Signal{Name: "ace_callout", Match: []string{"ace", "clutch"}}
	segments := []asrSegment{
		{Start: 5, Text: "nice shot"},
		{Start: 100, Text: "ACE! unbelievable"},
		{Start: 150, Text: "that was a clutch play"},
	}
	events := matchASRKeywordEvents(sig, segments)
	if len(events) != 2 {
		t.Fatalf("expected 2 keyword matches, got %d: %+v", len(events), events)
	}
	if events[0].T != 100 || events[1].T != 150 {
		t.Errorf("unexpected event timestamps: %+v", events)
	}
	if events[0].Source != "asr" {
		t.Errorf("Source = %q, want asr", events[0].Source)
	}
}

func TestMatchASRKeywordEventsNoMatch(t *testing.T) {
	sig := profile.Signal{Name: "ace_callout", Match: []string{"ace"}}
	segments := []asrSegment{{Start: 5, Text: "just walking around"}}
	if events := matchASRKeywordEvents(sig, segments); len(events) != 0 {
		t.Errorf("expected 0 events, got %d", len(events))
	}
}

func TestASRDetectorSkipsGracefullyWithoutBinaryOrModel(t *testing.T) {
	prof := &profile.Profile{
		Game: "valo",
		Signals: []profile.Signal{
			{Name: "ace_callout", Kind: profile.KindASRKeyword, Match: []string{"ace"}},
		},
	}
	events, err := ASRDetector{}.Detect("nonexistent.mp4", prof)
	if err != nil {
		t.Fatalf("expected graceful skip (nil error), got: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events when whisper.cpp/model unavailable, got %d", len(events))
	}
}

func TestASRDetectorNoOpWithoutASRSignals(t *testing.T) {
	prof := &profile.Profile{Game: "gow"}
	events, err := ASRDetector{}.Detect("nonexistent.mp4", prof)
	if err != nil {
		t.Fatalf("expected nil error for profile with no asr_keyword signals, got: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events, got %d", len(events))
	}
}
