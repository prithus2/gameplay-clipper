package llm

import (
	"strings"
	"testing"

	"github.com/gameplay-clipper/pkg/scoring"
)

func TestBuildEvidenceTextWithSignals(t *testing.T) {
	cand := scoring.Candidate{
		Archetype: "ace",
		Score:     3.38,
		SignalsFired: []scoring.FiredSignal{
			{Kind: "kill_feed_entry", T: 10, Confidence: 0.8, Source: "kill_feed"},
			{Kind: "ace_banner", T: 100, Confidence: 1.0, Source: "text_banner", Payload: "ACE"},
		},
	}
	got := buildEvidenceText(cand)
	if !strings.Contains(got, "ace") || !strings.Contains(got, "3.38") {
		t.Errorf("expected archetype and score in evidence text, got: %s", got)
	}
	if !strings.Contains(got, "kill_feed_entry") || !strings.Contains(got, "ace_banner") {
		t.Errorf("expected both fired signals listed, got: %s", got)
	}
	if !strings.Contains(got, `"ACE"`) {
		t.Errorf("expected payload text quoted in evidence, got: %s", got)
	}
}

func TestBuildEvidenceTextNoSignalsWarnsModel(t *testing.T) {
	cand := scoring.Candidate{Archetype: "generic", Score: 1.0}
	got := buildEvidenceText(cand)
	if !strings.Contains(got, "none") || !strings.Contains(got, "unreliable") {
		t.Errorf("expected an explicit low-trust warning when no real signals fired, got: %s", got)
	}
}

func TestTrimMarkdown(t *testing.T) {
	in := "```json\n{\"a\":1}\n```"
	got := trimMarkdown(in)
	if got != `{"a":1}` {
		t.Errorf("trimMarkdown(%q) = %q", in, got)
	}
}
