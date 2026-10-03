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

// KeepVisible is one HUD element that must survive the vertical reframe.
//
// A full-bleed 9:16 crop of a 16:9 frame keeps only (9/16)/(16/9) = 31.6% of
// the source width — x 0.342 to 0.658. Most HUD regions worth declaring are
// wider than that: valo's kill-streak banner spans x 0.273-0.726, mk's
// fatality banner 0.25-0.75. A naive crop therefore ships an Ace clip with the
// banner proving it was an Ace sliced off at both ends. Each region listed
// here is instead cropped from the source and re-composited over the
// reframed action, where it lands *larger* relative to the frame than it was
// in the 16:9 original — but only when the canvas is upscaled enough
// relative to the crop to make that true (see minBandMagnification in
// pkg/cutter/reel.go). A region can be too wide for this to ever work: God of
// War's subtitle band (x 0.10-0.90) is the real example, and it is
// deliberately not declared here for that reason — see profiles/gow.yaml.
type KeepVisible struct {
	Name   string `yaml:"name"`
	Region Region `yaml:"region"`

	// Place pins the band to the "top" or "bottom" of the reel canvas. Empty
	// derives it from where the region sits in the source frame, which is
	// almost always what you want — a bottom-of-screen killfeed reads as a
	// bottom-of-screen killfeed.
	Place string `yaml:"place,omitempty"`

	// Scale is the band's width as a fraction of the canvas width. 0 means
	// 1.0 (full width). Lower it for a region that only needs to stay legible
	// rather than dominate, e.g. a small round timer.
	Scale float64 `yaml:"scale,omitempty"`

	// Signal names the detector signal whose fire times this element is
	// on-screen for. Most HUD elements worth preserving are *transient* — a
	// "DOUBLE KILL" banner shows for a few seconds, a subtitle for one line —
	// and a band left up for the whole clip would spend the rest of its time
	// showing an enlarged strip of floor texture. Naming the signal gates the
	// band to when the element is actually there.
	//
	// Leave empty for a persistently-rendered element (a health bar, an ammo
	// counter), which should stay up for the whole clip.
	Signal string `yaml:"signal,omitempty"`

	// HoldS is how long the band stays up after its signal fires. 0 uses the
	// renderer's default.
	HoldS float64 `yaml:"hold_s,omitempty"`
}

// Reel configures the full-bleed 9:16 reel.mp4 render for this game.
type Reel struct {
	KeepVisible []KeepVisible `yaml:"keep_visible,omitempty"`

	// BlurpadArchetypes names the archetypes whose composition cropping would
	// destroy — wide cinematic framing where the subject is the whole frame,
	// not a localised patch of it. These default to short.mp4 (blur-pad, full
	// frame preserved) in the review UI instead of reel.mp4. Everything else
	// defaults to reel: Instagram's own recommendation guidance asks for
	// "9 x 16 vertical videos with no borders", and watch time — which a
	// two-thirds-blurred frame spends rather than earns — is the top-ranked
	// Reels signal.
	BlurpadArchetypes []string `yaml:"blurpad_archetypes,omitempty"`
}

// PrefersBlurpad reports whether archetype should default to short.mp4 rather
// than reel.mp4.
func (r *Reel) PrefersBlurpad(archetype string) bool {
	if r == nil {
		return false
	}
	for _, a := range r.BlurpadArchetypes {
		if a == archetype {
			return true
		}
	}
	return false
}

// Signal kinds. Each maps to one Detector implementation in pkg/signal.
const (
	KindLoudness      = "loudness"       // generic fallback, always available
	KindSceneCut      = "scene_cut"      // generic fallback, always available
	KindTextBanner    = "text_banner"    // OCR a region, match against a string list
	KindKillFeed      = "kill_feed"      // OCR a region, track new entries
	KindRoundBoundary = "round_boundary" // marks round start/end for round-scoped grouping
	KindASRKeyword    = "asr_keyword"    // whisper transcript keyword match
	KindLetterbox     = "letterbox"      // full-frame cinematic bar detection (cutscenes)
	KindSubtitle      = "subtitle"       // OCR a caption band, fire on dialogue-text presence
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

	// Reel configures the vertical reel.mp4 render. Absent means "crop on the
	// centre and composite nothing back" — safe, but it will cut this game's
	// HUD off, so a game whose highlights depend on on-screen text should
	// declare keep_visible regions.
	Reel *Reel `yaml:"reel,omitempty"`

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
	if err := p.validateReel(); err != nil {
		return err
	}
	return nil
}

// validateReel checks the reel block up front. A region that runs off the edge
// of the frame, or names an archetype that doesn't exist, would otherwise
// surface as a mangled overlay or a silently-ignored setting in a render that
// only happens minutes later, at approval time.
func (p *Profile) validateReel() error {
	if p.Reel == nil {
		return nil
	}
	for _, kv := range p.Reel.KeepVisible {
		if kv.Name == "" {
			return fmt.Errorf("every reel.keep_visible entry needs a name")
		}
		r := kv.Region
		if r.W <= 0 || r.H <= 0 {
			return fmt.Errorf("reel.keep_visible %q has a non-positive w/h", kv.Name)
		}
		if r.X < 0 || r.Y < 0 || r.X+r.W > 1 || r.Y+r.H > 1 {
			return fmt.Errorf("reel.keep_visible %q region %+v runs outside the frame (x+w and y+h must be <= 1)", kv.Name, r)
		}
		if kv.Place != "" && kv.Place != "top" && kv.Place != "bottom" {
			return fmt.Errorf("reel.keep_visible %q has invalid place %q (want top/bottom, or omit to derive it)", kv.Name, kv.Place)
		}
		if kv.Scale < 0 || kv.Scale > 1 {
			return fmt.Errorf("reel.keep_visible %q has scale %v (want 0-1, or omit for full width)", kv.Name, kv.Scale)
		}
		if kv.Signal != "" && p.SignalByName(kv.Signal) == nil {
			return fmt.Errorf("reel.keep_visible %q is gated on signal %q, which is not declared", kv.Name, kv.Signal)
		}
		if kv.HoldS < 0 {
			return fmt.Errorf("reel.keep_visible %q has a negative hold_s", kv.Name)
		}
	}
	for _, a := range p.Reel.BlurpadArchetypes {
		if p.ArchetypeByName(a) == nil {
			return fmt.Errorf("reel.blurpad_archetypes names %q, which is not a declared archetype", a)
		}
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
