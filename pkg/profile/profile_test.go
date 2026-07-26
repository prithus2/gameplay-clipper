package profile

import "testing"

func TestLoadValo(t *testing.T) {
	p, err := Load("../../profiles/valo.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.Game != "valo" {
		t.Errorf("Game = %q, want valo", p.Game)
	}
	if p.RoundBoundarySignal != "round_reset" {
		t.Errorf("RoundBoundarySignal = %q, want round_reset", p.RoundBoundarySignal)
	}
	if p.SignalByName("kill_streak_banner") == nil {
		t.Error("expected kill_streak_banner signal")
	}
	// generic fallback must always be present
	if p.SignalByName("loudness_spike") == nil {
		t.Error("expected generic loudness_spike fallback to be merged in")
	}
	if p.ArchetypeByName("ace") == nil {
		t.Error("expected ace archetype")
	}
}

func TestLoadForGameFallsBackToGeneric(t *testing.T) {
	p, err := LoadForGame("../../profiles", "totally_unknown_game")
	if err != nil {
		t.Fatalf("LoadForGame: %v", err)
	}
	if len(p.Signals) != 2 {
		t.Errorf("expected 2 generic signals, got %d", len(p.Signals))
	}
	if p.ArchetypeByName("generic") == nil {
		t.Error("expected generic archetype")
	}
}

func TestGowProfileHasCutsceneSignalPlusGenericFallback(t *testing.T) {
	p, err := Load("../../profiles/gow.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// gow declares one letterbox (cutscene) signal; the two generic
	// fallbacks still get merged in on top of it.
	if len(p.Signals) != 3 {
		t.Errorf("expected gow.yaml's 1 cutscene signal + 2 generic fallbacks = 3, got %d", len(p.Signals))
	}
	cut := p.SignalByName("cutscene")
	if cut == nil {
		t.Fatal("expected a cutscene signal")
	}
	if cut.Kind != KindSubtitle {
		t.Errorf("cutscene signal should be kind %q, got %q", KindSubtitle, cut.Kind)
	}
	if cut.Region == nil {
		t.Error("subtitle cutscene signal needs a caption region")
	}
	if p.ArchetypeByName("cutscene") == nil {
		t.Error("expected a cutscene archetype")
	}
}

func TestValidateRejectsBadBoundaryRole(t *testing.T) {
	p := &Profile{
		Game:       "x",
		Archetypes: []Archetype{{Name: "generic"}},
		Signals:    []Signal{{Name: "s", BoundaryRole: "sideways"}},
	}
	if err := p.validate(); err == nil {
		t.Error("expected validation error for invalid boundary_role")
	}
}

func TestValidateRejectsMissingRegionForTextBanner(t *testing.T) {
	p := &Profile{
		Game:       "x",
		Archetypes: []Archetype{{Name: "generic"}},
		Signals:    []Signal{{Name: "s", Kind: KindTextBanner}},
	}
	if err := p.validate(); err == nil {
		t.Error("expected validation error for text_banner signal missing region")
	}
}
