package signal

import (
	"testing"

	"github.com/gameplay-clipper/pkg/profile"
)

func TestMatchTextBannerEventsRisingEdgeOnly(t *testing.T) {
	sig := profile.Signal{Name: "ace_banner", Match: []string{"ACE", "CLUTCH"}}
	samples := []ocrSample{
		{T: 0, Text: ""},
		{T: 0.5, Text: "some other hud text"},
		{T: 1.0, Text: "ACE"},
		{T: 1.5, Text: "ACE"}, // banner still on screen — must not re-fire
		{T: 2.0, Text: "ACE"},
		{T: 2.5, Text: ""},
		{T: 3.0, Text: "CLUTCH"}, // separate occurrence — must fire again
	}
	events := matchTextBannerEvents(sig, samples)
	if len(events) != 2 {
		t.Fatalf("expected 2 events (one per rising edge), got %d: %+v", len(events), events)
	}
	if events[0].T != 1.0 || events[1].T != 3.0 {
		t.Errorf("unexpected event timestamps: %+v", events)
	}
}

func TestMatchTextBannerEventsCaseInsensitive(t *testing.T) {
	sig := profile.Signal{Name: "fatality_prompt", Match: []string{"FINISH HIM"}}
	samples := []ocrSample{{T: 0, Text: "finish him!"}}
	events := matchTextBannerEvents(sig, samples)
	if len(events) != 1 {
		t.Fatalf("expected case-insensitive match to fire, got %d events", len(events))
	}
}

func TestMatchTextBannerEventsNoMatch(t *testing.T) {
	sig := profile.Signal{Name: "ace_banner", Match: []string{"ACE"}}
	samples := []ocrSample{{T: 0, Text: "just some hud noise"}}
	if events := matchTextBannerEvents(sig, samples); len(events) != 0 {
		t.Errorf("expected 0 events, got %d", len(events))
	}
}

func TestMatchKillFeedEventsNewEntryOnly(t *testing.T) {
	sig := profile.Signal{Name: "kill_feed_entry"}
	samples := []ocrSample{
		{T: 0, Text: ""},
		{T: 0.5, Text: "Reyna  Yoru"},
		{T: 1.0, Text: "Reyna  Yoru"}, // same entry still visible — no re-fire
		{T: 1.5, Text: ""},
		{T: 2.0, Text: "Killjoy  Me"}, // new entry
	}
	events := matchKillFeedEvents(sig, samples)
	if len(events) != 2 {
		t.Fatalf("expected 2 kill feed events, got %d: %+v", len(events), events)
	}
	if events[0].T != 0.5 || events[1].T != 2.0 {
		t.Errorf("unexpected event timestamps: %+v", events)
	}
}
