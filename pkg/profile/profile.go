// Package profile loads per-game detection profiles: HUD regions, signal
// definitions, and archetype scoring parameters. This is the "games as
// config, not code" layer — adding a new game means writing a YAML file,
// not a new Go package.
package profile

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Region is a screen region expressed as a fraction of frame width/height
// (0-1), so it scales across capture resolutions instead of being pinned to
// absolute pixels.
type Region struct {
	X float64 `yaml:"x"`
	Y float64 `yaml:"y"`
	W float64 `yaml:"w"`
	H float64 `yaml:"h"`
}

// Signal kinds. Each maps to one Detector implementation in pkg/signal.
const (
	KindLoudness     = "loudness"      // generic fallback, always available
	KindSceneCut     = "scene_cut"     // generic fallback, always available
	KindTextBanner   = "text_banner"   // OCR a region, match against a string list
	KindKillFeed     = "kill_feed"     // OCR a region, track new entries
	KindRoundBoundary = "round_boundary" // marks round start/end for round-scoped grouping
	KindASRKeyword   = "asr_keyword"   // whisper transcript keyword match
	KindLetterbox    = "letterbox"     // full-frame cinematic bar detection (cutscenes)
	KindSubtitle     = "subtitle"      // OCR a caption band, fire on dialogue-text presence
)

// Signal declares one detectable event type and how much it should
// contribute to each archetype's score.
type Signal struct {
	Name         string             `yaml:"name"`
	Kind         string             `yaml:"kind"`
	Region       *Region            `yaml:"region,omitempty"`
	Match        []string           `yaml:"match,omitempty"`
	Weight       map[string]float64 `yaml:"weight"`
	BoundaryRole string             `yaml:"boundary_role,omitempty"` // "open", "close", or ""
	DecaySeconds float64            `yaml:"decay_seconds,omitempty"`
	SampleFPS    float64            `yaml:"sample_fps,omitempty"`
}

// Archetype is a kind of highlight this game produces (e.g. "ace",
// "fatality", "boss_kill"), with its own score threshold and clip shape.
type Archetype struct {
	Name       string  `yaml:"name"`
	Threshold  float64 `yaml:"threshold"`
	MinLengthS float64 `yaml:"min_length_s"`
	MaxLengthS float64 `yaml:"max_length_s"`
	LeadPadS   float64 `yaml:"lead_pad_s"`
	TrailPadS  float64 `yaml:"trail_pad_s"`

	// StreakMergeGapS, if set, merges any relevant events within this many
	// seconds of each other (transitively) into one window instead of
	// scoring each detected event as an isolated peak. See
	// pkg/scoring.streakCluster.
	StreakMergeGapS float64 `yaml:"streak_merge_gap_s,omitempty"`
}

// Profile is one game's full detection configuration.
type Profile struct {
	Game    string `yaml:"game"`
	Version int    `yaml:"version"`

	Signals    []Signal    `yaml:"signals"`
	Archetypes []Archetype `yaml:"archetypes"`

	// RoundBoundarySignal, if set, names a Signal (kind round_boundary)
	// whose events bound "belongs to the same run" grouping: all fired
	// signals between two consecutive round-boundary events merge into a
	// single candidate window regardless of the gaps between them. This is
	// what lets a 5-kill Ace (kills spread 10-40s apart) become one clip
	// instead of 5 isolated ones.
	RoundBoundarySignal string `yaml:"round_boundary_signal,omitempty"`

	// NMSRadiusS is the minimum spacing between two accepted score peaks;
	// peaks closer than this are treated as one candidate.
	NMSRadiusS float64 `yaml:"nms_radius_s"`
}

// SignalByName returns the named signal, or nil if not declared.
func (p *Profile) SignalByName(name string) *Signal {
	for i := range p.Signals {
		if p.Signals[i].Name == name {
			return &p.Signals[i]
		}
	}
	return nil
}

// ArchetypeByName returns the named archetype, or nil if not declared.
func (p *Profile) ArchetypeByName(name string) *Archetype {
	for i := range p.Archetypes {
		if p.Archetypes[i].Name == name {
			return &p.Archetypes[i]
		}
	}
	return nil
}

// Load reads and validates a profile YAML file.
func Load(path string) (*Profile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read profile %s: %w", path, err)
	}
	var p Profile
	if err := yaml.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("parse profile %s: %w", path, err)
	}
	p.ensureGenericFallback()
	if err := p.validate(); err != nil {
		return nil, fmt.Errorf("invalid profile %s: %w", path, err)
	}
	return &p, nil
}

// ensureGenericFallback guarantees every profile — even a near-empty one for
// a game with no dedicated signals yet — always has the generic loudness/
// scene-change path and a "generic" archetype to fall back on. Per-game
// signals layer on top of this; they never replace it.
func (p *Profile) ensureGenericFallback() {
	if p.SignalByName("loudness_spike") == nil {
		p.Signals = append(p.Signals, Signal{
			Name: "loudness_spike", Kind: KindLoudness,
			Weight: map[string]float64{"generic": 1.0}, DecaySeconds: 6,
		})
	}
	if p.SignalByName("scene_cut") == nil {
		p.Signals = append(p.Signals, Signal{
			Name: "scene_cut", Kind: KindSceneCut,
			Weight: map[string]float64{"generic": 1.0}, DecaySeconds: 6,
		})
	}
	if p.ArchetypeByName("generic") == nil {
		p.Archetypes = append(p.Archetypes, Archetype{
			Name: "generic", Threshold: 0.5,
			MinLengthS: 8, MaxLengthS: 60, LeadPadS: 6, TrailPadS: 3,
		})
	}
}

func (p *Profile) validate() error {
	if p.Game == "" {
		return fmt.Errorf("game name is required")
	}
	if len(p.Archetypes) == 0 {
		return fmt.Errorf("at least one archetype is required")
	}
	if p.NMSRadiusS <= 0 {
		p.NMSRadiusS = 5.0
	}
	for _, s := range p.Signals {
		if s.Kind == KindTextBanner || s.Kind == KindKillFeed {
			if s.Region == nil {
				return fmt.Errorf("signal %q of kind %q requires a region", s.Name, s.Kind)
			}
		}
		if s.BoundaryRole != "" && s.BoundaryRole != "open" && s.BoundaryRole != "close" {
			return fmt.Errorf("signal %q has invalid boundary_role %q (want open/close)", s.Name, s.BoundaryRole)
		}
	}
	if p.RoundBoundarySignal != "" && p.SignalByName(p.RoundBoundarySignal) == nil {
		return fmt.Errorf("round_boundary_signal %q does not match any declared signal", p.RoundBoundarySignal)
	}
	return nil
}

// LoadForGame looks up profiles/<gameName>.yaml relative to dir. If it
// doesn't exist, it returns a generic-only fallback profile rather than
// failing — an unsupported game should degrade gracefully, not crash.
func LoadForGame(dir, gameName string) (*Profile, error) {
	path := filepath.Join(dir, gameName+".yaml")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return DefaultProfile(gameName), nil
	}
	return Load(path)
}

// DefaultProfile is the zero-config fallback for any game without a
// dedicated profile: generic loudness/scene-change signals only, one
// archetype, matching the pipeline's original (pre-profile) behavior.
func DefaultProfile(gameName string) *Profile {
	p := &Profile{Game: gameName, Version: 0, NMSRadiusS: 5.0}
	p.ensureGenericFallback()
	return p
}
