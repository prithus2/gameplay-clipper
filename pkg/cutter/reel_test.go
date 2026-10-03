package cutter

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// The real footage in raw_clips: gow/mk are 3840x2160, valo is 2560x1440.
func TestReelCanvas(t *testing.T) {
	cases := []struct {
		name         string
		srcW, srcH   int
		override     int
		wantW, wantH int
	}{
		// A 4K master crops to 1214x2160, which is already above Instagram's
		// 1080x1920 — so the gameplay is shown at 1:1 with no scaling at all.
		{name: "4k master", srcW: 3840, srcH: 2160, wantW: 1214, wantH: 2160},
		// 1440p crops to 810x1440, below spec. "Low-resolution" is a stated
		// demotion reason, so it is upscaled to clear the bar.
		{name: "1440p master", srcW: 2560, srcH: 1440, wantW: 1080, wantH: 1920},
		{name: "1080p master", srcW: 1920, srcH: 1080, wantW: 1080, wantH: 1920},
		// An already-vertical source must not be pillar-cropped to nothing.
		{name: "vertical source", srcW: 1080, srcH: 1920, wantW: 1080, wantH: 1920},
		{name: "override pins height", srcW: 3840, srcH: 2160, override: 1920, wantW: 1080, wantH: 1920},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, h := reelCanvas(tc.srcW, tc.srcH, tc.override)
			if w != tc.wantW || h != tc.wantH {
				t.Errorf("reelCanvas(%d, %d, %d) = %dx%d, want %dx%d",
					tc.srcW, tc.srcH, tc.override, w, h, tc.wantW, tc.wantH)
			}
			if w%2 != 0 || h%2 != 0 {
				t.Errorf("canvas %dx%d must be even in both axes for yuv420p", w, h)
			}
		})
	}
}

func TestReelCropFollowsFocusAndStaysInFrame(t *testing.T) {
	const srcW, srcH = 3840, 2160

	centre := reelCrop(srcW, srcH, 0.5, 0.5)
	if centre.W != 1214 || centre.H != 2160 {
		t.Errorf("centre crop = %dx%d, want 1214x2160 (full height, 9:16 wide)", centre.W, centre.H)
	}
	if got, want := centre.X, (srcW-1214)/2; got != want {
		t.Errorf("centre crop X = %d, want %d", got, want)
	}

	// A focus to the right must actually move the window right.
	right := reelCrop(srcW, srcH, 0.8, 0.5)
	if right.X <= centre.X {
		t.Errorf("focus 0.8 gave X %d, expected further right than centre's %d", right.X, centre.X)
	}

	// A focus at either edge must clamp to the frame rather than run off it:
	// an out-of-frame crop is an ffmpeg error, not a black margin.
	for _, focus := range []float64{0, 0.02, 0.5, 0.98, 1} {
		c := reelCrop(srcW, srcH, focus, focus)
		if c.X < 0 || c.Y < 0 || c.X+c.W > srcW || c.Y+c.H > srcH {
			t.Errorf("focus %v produced crop %+v, which runs outside %dx%d", focus, c, srcW, srcH)
		}
	}
}

func TestReelCropOnVerticalSourceKeepsFullWidth(t *testing.T) {
	// An already-vertical capture has no width to spare; cropping it to 9:16
	// must take height, not invent pixels at the sides.
	c := reelCrop(1080, 2160, 0.5, 0.5)
	if c.W != 1080 {
		t.Errorf("crop width = %d, want the source's full 1080", c.W)
	}
	if c.H != 1920 {
		t.Errorf("crop height = %d, want 1920 (9:16 of 1080)", c.H)
	}
}

// Valorant's real kill-streak banner, measured off valo_01.mp4 at t=114: the
// lettering sits at source x 0.4625-0.5367, padded in the profile to 0.42-0.58.
// A centre crop keeps 0.342-0.658, so the banner survives unaided — and
// compositing it anyway is what put "QUADRAKILL" on screen twice in the first
// render of this format.
var valoKillBanner = HUDRegion{
	Name: "kill_streak_banner", X: 0.42, Y: 0.90, W: 0.16, H: 0.042,
	Signal: "banner_shown", HoldS: 4,
}

func TestValorantBannerNeedsNoBandOnACentreCrop(t *testing.T) {
	l := planReel(2560, 1440, 0.5, 0.5, []HUDRegion{valoKillBanner}, 0)
	if len(l.Bands) != 0 {
		t.Errorf("got %d bands; a centre crop already shows this banner, and compositing it duplicates the text", len(l.Bands))
	}
	if len(l.AlreadyInFrame) != 1 {
		t.Errorf("AlreadyInFrame = %v, want the banner recorded as retained by the crop", l.AlreadyInFrame)
	}
}

// The band is insurance: it engages exactly when crop_focus slides the window
// far enough off-centre to start clipping the banner.
func TestValorantBannerGetsABandWhenFocusMovesOff(t *testing.T) {
	l := planReel(2560, 1440, 0.25, 0.5, []HUDRegion{valoKillBanner}, 0)

	src := regionRect(2560, 1440, valoKillBanner)
	if containsRect(l.Crop, src) {
		t.Fatalf("premise broken: crop %+v still contains the banner %+v at focus 0.25", l.Crop, src)
	}
	if len(l.Bands) != 1 {
		t.Fatalf("got %d bands, want the banner composited back once the crop loses it", len(l.Bands))
	}
	b := l.Bands[0]
	// It came from the bottom of the frame, so it belongs at the bottom.
	if b.Dst.Y < l.CanvasH/2 {
		t.Errorf("band Y = %d on a %d-tall canvas, want the bottom half", b.Dst.Y, l.CanvasH)
	}
	// And it must clear Instagram's bottom UI, not sit under the like button.
	if bottom := b.Dst.Y + b.Dst.H; bottom > l.CanvasH-int(float64(l.CanvasH)*reelSafeBottomFrac) {
		t.Errorf("band bottom edge %d intrudes into the bottom safe zone of a %d-tall canvas", bottom, l.CanvasH)
	}
	// The point of the exercise: it ends up bigger on screen than it was.
	if got, want := float64(b.Inner.W)/float64(l.CanvasW), valoKillBanner.W; got <= want {
		t.Errorf("band covers %.2f of the canvas width but only %.2f of the source frame — it should read larger, not smaller", got, want)
	}
}

func TestBandsStayOnCanvasAndClearSafeZones(t *testing.T) {
	// Two bottom regions plus a top one, to exercise stacking on both edges.
	// Each sits outside the crop's 0.342-0.658 (so it earns a band at all —
	// a narrower, centred one would be skipped, see
	// TestRegionAlreadyInCropGetsNoBand) but is itself narrow in source
	// pixels, so the band actually magnifies it (see minBandMagnification).
	// On this 4K source the reel crop takes the full source height at 1:1 —
	// no upscaling — so canvasW equals crop.W exactly; a region that needs a
	// band by being *wider* than the crop would therefore always render
	// smaller than its source (this is the God of War subtitle defect, see
	// TestShrinkingBandIsSkippedAndRecorded), so these are corner elements
	// instead, off to the side rather than spanning the frame.
	hud := []HUDRegion{
		{Name: "corner_banner", X: 0.02, Y: 0.90, W: 0.10, H: 0.042},
		{Name: "corner_caption", X: 0.02, Y: 0.80, W: 0.10, H: 0.05},
		{Name: "corner_meter", X: 0.02, Y: 0.03, W: 0.10, H: 0.05},
	}
	l := planReel(3840, 2160, 0.5, 0.5, hud, 0)
	if len(l.Bands) != 3 {
		t.Fatalf("got %d bands, want 3", len(l.Bands))
	}
	for _, b := range l.Bands {
		if b.Dst.X < 0 || b.Dst.Y < 0 || b.Dst.X+b.Dst.W > l.CanvasW || b.Dst.Y+b.Dst.H > l.CanvasH {
			t.Errorf("band %q at %+v runs off the %dx%d canvas", b.Name, b.Dst, l.CanvasW, l.CanvasH)
		}
		if b.Inner.W > b.Dst.W || b.Inner.H > b.Dst.H {
			t.Errorf("band %q inner %+v does not fit inside its border %+v", b.Name, b.Inner, b.Dst)
		}
		if b.Dst.H > int(float64(l.CanvasH)*reelMaxBandFrac)+2*(l.CanvasW/360) {
			t.Errorf("band %q is %d tall on a %d canvas — over the height cap", b.Name, b.Dst.H, l.CanvasH)
		}
	}
	// Stacked bottom bands must not sit on top of each other.
	lower, upper := l.Bands[0], l.Bands[1]
	if lower.Dst.Y < upper.Dst.Y+upper.Dst.H {
		t.Errorf("bottom bands overlap: %+v and %+v", lower.Dst, upper.Dst)
	}
}

// A region that straddles the crop boundary — partly kept, partly cut — still
// leaves a clipped remnant on screen the same way God of War's subtitles did.
// The band has to land on top of that remnant and cover it, or the reel shows
// the full (banded) version and a ghost of the truncated one at once. Unlike
// the original version of this test, the region here is narrow enough in
// source pixels to actually earn its band under minBandMagnification — see
// TestShrinkingBandIsSkippedAndRecorded for what happens to a region that
// isn't (which is what God of War's actual subtitle band turned out to be).
func TestBandCoversItsClippedRemnant(t *testing.T) {
	subs := HUDRegion{Name: "subtitle_edge", X: 0.30, Y: 0.70, W: 0.25, H: 0.12}
	l := planReel(3840, 2160, 0.5, 0.5, []HUDRegion{subs}, 0)
	if len(l.SkippedBands) != 0 {
		t.Fatalf("premise broken: band was skipped as non-magnifying: %v", l.SkippedBands)
	}
	if len(l.Bands) != 1 {
		t.Fatalf("got %d bands, want 1", len(l.Bands))
	}
	b := l.Bands[0]

	src := regionRect(3840, 2160, subs)
	if got, want := float64(b.Inner.W), float64(src.W); got < want {
		t.Errorf("band inner width %v is smaller than its %v source — it should magnify, not shrink", got, want)
	}

	ghost := ghostRect(l.Action, l.Crop, src)
	if ghost.W <= 0 || ghost.H <= 0 {
		t.Fatalf("premise broken: nothing of the region survives the crop, so there is no ghost to cover")
	}
	if !containsRect(b.Dst, ghost) {
		t.Errorf("band %+v does not cover the clipped remnant at %+v — the truncated line will show through", b.Dst, ghost)
	}
	// And the readable content sits inside that box, not outside it.
	if b.Inner.X < 0 || b.Inner.Y < 0 ||
		b.Inner.X+b.Inner.W > b.Dst.W || b.Inner.Y+b.Inner.H > b.Dst.H {
		t.Errorf("band content %+v does not fit inside its box %+v", b.Inner, b.Dst)
	}
}

// This is the actual God of War subtitle-band defect: the caption region is
// 3072px wide (0.80 of a 3840px source) but the reel canvas for a 4K master
// is only 1214px wide, so compositing it back would render the text at 0.39x
// — a forced shrink, not the magnification a band needs to earn its place
// (see minBandMagnification). It rendered as a full-width, mostly-black plate
// in practice. planBands must skip it rather than draw it, and name it in
// SkippedBands so a mostly-black reel is diagnosable from reel_layout.json.
func TestShrinkingBandIsSkippedAndRecorded(t *testing.T) {
	subs := HUDRegion{Name: "subtitles", X: 0.10, Y: 0.70, W: 0.80, H: 0.12}
	l := planReel(3840, 2160, 0.5, 0.5, []HUDRegion{subs}, 0)
	if len(l.Bands) != 0 {
		t.Errorf("got %d bands, want 0 — this region shrinks when composited, see minBandMagnification", len(l.Bands))
	}
	if len(l.AlreadyInFrame) != 0 {
		t.Errorf("AlreadyInFrame = %v, want none — this region is genuinely cut by the crop, it just can't be fixed by a band", l.AlreadyInFrame)
	}
	if len(l.SkippedBands) != 1 || l.SkippedBands[0] != "subtitles" {
		t.Errorf("SkippedBands = %v, want [subtitles]", l.SkippedBands)
	}
}

// The flip side of the God of War case: Valorant's real kill-streak banner
// (see valoKillBanner) magnifies 410px -> well over 1000px when it needs a
// band at all, so it must be drawn and must not appear in SkippedBands.
func TestMagnifyingBandIsKept(t *testing.T) {
	l := planReel(2560, 1440, 0.25, 0.5, []HUDRegion{valoKillBanner}, 0)
	if len(l.Bands) != 1 {
		t.Fatalf("got %d bands, want 1", len(l.Bands))
	}
	if len(l.SkippedBands) != 0 {
		t.Errorf("SkippedBands = %v, want none — this band magnifies its element", l.SkippedBands)
	}
	src := regionRect(2560, 1440, valoKillBanner)
	if got := l.Bands[0].Inner.W; got < src.W {
		t.Errorf("band inner width %d is smaller than its %d source — it should magnify, not shrink", got, src.W)
	}
}

// When nothing of an element survives the crop there is no remnant to line up
// with, so placement falls back to the safe zone — and two such elements on the
// same edge must stack rather than overlap.
func TestFullyClippedBandsStackInTheSafeZone(t *testing.T) {
	// Both sit left of the 0.342-0.658 a centre crop keeps.
	hud := []HUDRegion{
		{Name: "abilities", X: 0.02, Y: 0.88, W: 0.10, H: 0.05},
		{Name: "ammo", X: 0.02, Y: 0.80, W: 0.10, H: 0.05},
	}
	l := planReel(3840, 2160, 0.5, 0.5, hud, 0)
	if len(l.Bands) != 2 {
		t.Fatalf("got %d bands, want 2", len(l.Bands))
	}
	a, b := l.Bands[0], l.Bands[1]
	if a.Dst.Y < b.Dst.Y+b.Dst.H {
		t.Errorf("bands overlap instead of stacking: %+v and %+v", a.Dst, b.Dst)
	}
	for _, band := range l.Bands {
		if bottom := band.Dst.Y + band.Dst.H; bottom > l.CanvasH-int(float64(l.CanvasH)*reelSafeBottomFrac) {
			t.Errorf("band %q bottom %d intrudes into the bottom safe zone of a %d canvas", band.Name, bottom, l.CanvasH)
		}
	}
}

func TestFullWidthBandFitsExactly(t *testing.T) {
	// Regression: the band's border used to pad outward from a full-canvas-width
	// interior, so a default (scale 1.0) band came out 6px wider than the canvas
	// and lost its right edge to clamping.
	//
	// The source region here is a small corner element, not a wide banner: what
	// this test checks (the *rendered* band coming out full-bleed) is driven by
	// Scale defaulting to 1.0, which sizes the band off canvasW regardless of
	// the source region's own width — so a small region exercises the same
	// border math while also clearing minBandMagnification comfortably.
	l := planReel(2560, 1440, 0.5, 0.5, []HUDRegion{
		{Name: "corner_badge", X: 0.02, Y: 0.90, W: 0.10, H: 0.042},
	}, 0)
	b := l.Bands[0]
	if b.Dst.W > l.CanvasW {
		t.Errorf("band outer width %d exceeds the %d canvas", b.Dst.W, l.CanvasW)
	}
	if b.Dst.X < 0 || b.Dst.X+b.Dst.W > l.CanvasW {
		t.Errorf("band spans %d..%d outside the %d canvas", b.Dst.X, b.Dst.X+b.Dst.W, l.CanvasW)
	}
	// It should still be very nearly full width — the border is a hairline, not a margin.
	if float64(b.Dst.W) < 0.98*float64(l.CanvasW) {
		t.Errorf("band width %d is well under the %d canvas; scale 1.0 should be near-full-bleed", b.Dst.W, l.CanvasW)
	}
}

func TestRegionAlreadyInCropGetsNoBand(t *testing.T) {
	// A narrow centred region survives the crop on its own; compositing it
	// again would show it twice.
	hud := []HUDRegion{{Name: "centre_prompt", X: 0.45, Y: 0.45, W: 0.10, H: 0.10}}
	l := planReel(3840, 2160, 0.5, 0.5, hud, 0)
	if len(l.Bands) != 0 {
		t.Errorf("got %d bands, want none for a region the crop already contains", len(l.Bands))
	}
	if len(l.AlreadyInFrame) != 1 || l.AlreadyInFrame[0] != "centre_prompt" {
		t.Errorf("AlreadyInFrame = %v, want [centre_prompt]", l.AlreadyInFrame)
	}
}

func TestSanitizeFocusRejectsGarbage(t *testing.T) {
	// crop_focus comes from an LLM, which is asked for a 0-1 fraction but not
	// constrained to return one.
	for _, tc := range []struct{ in, want float64 }{
		{0.5, 0.5}, {0, 0}, {1, 1}, {-3, 0}, {42, 1},
	} {
		if got := sanitizeFocus(tc.in); got != tc.want {
			t.Errorf("sanitizeFocus(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
	if got := sanitizeFocus(nan()); got != 0.5 {
		t.Errorf("sanitizeFocus(NaN) = %v, want the 0.5 centre fallback", got)
	}
}

func nan() float64 {
	var zero float64
	return zero / zero
}

// A transient banner's band must only be up while the banner is, or a 40s Ace
// spends most of its runtime showing an enlarged strip of floor texture.
func TestTransientBandIsGatedToItsSignal(t *testing.T) {
	banner := HUDRegion{
		Name: "corner_badge", X: 0.02, Y: 0.90, W: 0.10, H: 0.042,
		Signal: "banner_shown", HoldS: 3,
	}
	hud := []HUDRegion{banner}
	l := planReel(2560, 1440, 0.5, 0.5, hud, 0)
	if len(l.Bands) != 1 {
		t.Fatalf("got %d bands, want 1", len(l.Bands))
	}

	// Clip covers absolute 100-140s; banners detected at 104s and 113.5s
	// (the real gap documented in valo.yaml), plus one outside the window.
	signals := []FiredSignal{
		{Kind: "banner_shown", T: 104},
		{Kind: "banner_shown", T: 113.5},
		{Kind: "banner_shown", T: 900},
		{Kind: "something_else", T: 105},
	}
	bands := gateBands(l.Bands, hud, signals, 100, 40)

	if len(bands) != 1 {
		t.Fatalf("got %d bands after gating, want 1 (evidence exists, just not for every window)", len(bands))
	}
	got := bands[0].Enable
	want := "'between(t,3.000,7.000)+between(t,12.500,16.500)'"
	if got != want {
		t.Errorf("Enable = %s, want %s", got, want)
	}
}

func TestGatedWindowsMergeAndClamp(t *testing.T) {
	// Overlapping detections collapse into one span, and a span running past
	// either clip edge is clamped rather than emitted out of range.
	w := signalWindows([]FiredSignal{
		{Kind: "b", T: 100.5}, {Kind: "b", T: 101}, {Kind: "b", T: 102},
	}, "b", 100, 10, 3)
	if len(w) != 1 {
		t.Fatalf("got %d windows, want 1 merged: %+v", len(w), w)
	}
	if w[0].Start != 0 {
		t.Errorf("start = %v, want clamped to 0", w[0].Start)
	}
	if w[0].End != 5 {
		t.Errorf("end = %v, want 5", w[0].End)
	}

	// A detection past the end of the clip contributes nothing.
	if got := signalWindows([]FiredSignal{{Kind: "b", T: 500}}, "b", 100, 10, 3); len(got) != 0 {
		t.Errorf("got %+v, want no windows for a signal outside the clip", got)
	}

	// And one just before the clip still shows, clamped to the start.
	got := signalWindows([]FiredSignal{{Kind: "b", T: 99.5}}, "b", 100, 10, 3)
	if len(got) != 1 || got[0].Start != 0 {
		t.Errorf("got %+v, want one window clamped to the clip start", got)
	}
}

// The OCR-miss fallback: valo.yaml documents OCR missing banners mid-streak.
// Detection ran over this clip (signals is non-empty) and simply didn't catch
// THIS element that time — that is evidence the detector failed, not that the
// element was absent — so the band stays up rather than hiding the payoff.
// This must keep working after change 1, which only drops a band when
// signals is empty (see TestZeroSignalsDropsSignalGatedBand).
func TestUndetectedSignalLeavesBandUp(t *testing.T) {
	hud := []HUDRegion{{
		Name: "corner_badge", X: 0.02, Y: 0.90, W: 0.10, H: 0.042,
		Signal: "banner_shown",
	}}
	l := planReel(2560, 1440, 0.5, 0.5, hud, 0)
	bands := gateBands(l.Bands, hud, []FiredSignal{{Kind: "unrelated", T: 5}}, 0, 30)
	if len(bands) != 1 {
		t.Fatalf("got %d bands, want 1 (kept up, not dropped) — other signals fired, so detection ran", len(bands))
	}
	if bands[0].Enable != "" {
		t.Errorf("Enable = %q, want empty (always up) when the signal never fired", bands[0].Enable)
	}
}

// The other half of change 1: zero signals on the whole clip is not "the
// detector looked and missed" (see above), it's "detection never ran" — a
// hand-picked --include window never runs the detectors at all (see
// Clip.Signals) — so a signal-gated band has no evidence behind it at all and
// must be dropped rather than left up for the clip's entire length. This is
// the actual God of War defect: a manual include of a cutscene carried no
// signals, so the (would-be) subtitle band rendered as a permanent bar.
func TestZeroSignalsDropsSignalGatedBand(t *testing.T) {
	hud := []HUDRegion{{
		Name: "corner_badge", X: 0.02, Y: 0.90, W: 0.10, H: 0.042,
		Signal: "banner_shown",
	}}
	l := planReel(2560, 1440, 0.5, 0.5, hud, 0)
	if len(l.Bands) != 1 {
		t.Fatalf("premise broken: got %d bands before gating, want 1", len(l.Bands))
	}
	bands := gateBands(l.Bands, hud, nil, 0, 30)
	if len(bands) != 0 {
		t.Errorf("got %d bands, want 0 — no signals at all means no evidence the element was ever on screen", len(bands))
	}
}

func TestPersistentBandIsNeverGated(t *testing.T) {
	// An element with no signal named is always on screen (a health bar), so
	// it must not be gated — or dropped — even when unrelated signals fired,
	// or when there are no signals at all.
	hud := []HUDRegion{{Name: "top_meter", X: 0.02, Y: 0.03, W: 0.10, H: 0.05}}
	l := planReel(3840, 2160, 0.5, 0.5, hud, 0)
	if len(l.Bands) != 1 {
		t.Fatalf("got %d bands, want 1", len(l.Bands))
	}

	bands := gateBands(l.Bands, hud, []FiredSignal{{Kind: "top_meter", T: 5}}, 0, 30)
	if len(bands) != 1 || bands[0].Enable != "" {
		t.Errorf("gateBands(with signals) = %+v, want 1 band with Enable empty for a persistent element", bands)
	}

	bands = gateBands(l.Bands, hud, nil, 0, 30)
	if len(bands) != 1 || bands[0].Enable != "" {
		t.Errorf("gateBands(no signals) = %+v, want 1 band still up — a persistent element needs no evidence at all", bands)
	}
}

func TestGatedFilterEmitsQuotedEnable(t *testing.T) {
	hud := []HUDRegion{{
		Name: "corner_badge", X: 0.02, Y: 0.90, W: 0.10, H: 0.042,
		Signal: "banner_shown",
	}}
	l := planReel(2560, 1440, 0.5, 0.5, hud, 0)
	l.Bands = gateBands(l.Bands, hud, []FiredSignal{{Kind: "banner_shown", T: 5}}, 0, 30)

	f := reelFilter(l)
	// Unquoted, the commas inside between() would be read as filter-argument
	// separators and the graph would fail to parse.
	if !strings.Contains(f, ":enable='between(t,") {
		t.Errorf("expected a quoted enable expression on the overlay: %s", f)
	}
}

func TestReelFilterShape(t *testing.T) {
	plain := planReel(3840, 2160, 0.5, 0.5, nil, 0)
	f := reelFilter(plain)
	if strings.Contains(f, "split") {
		t.Errorf("a band-less layout should not split the source: %s", f)
	}
	if !strings.HasSuffix(f, "[vout]") {
		t.Errorf("filter must end at [vout]: %s", f)
	}

	withBands := planReel(2560, 1440, 0.5, 0.5, []HUDRegion{
		{Name: "corner_badge", X: 0.02, Y: 0.90, W: 0.10, H: 0.042},
		{Name: "top_meter", X: 0.02, Y: 0.03, W: 0.10, H: 0.05},
	}, 0)
	f = reelFilter(withBands)
	if !strings.Contains(f, "split=3") {
		t.Errorf("two bands plus the action pane need split=3: %s", f)
	}
	if !strings.HasSuffix(f, "[vout]") {
		t.Errorf("filter must end at [vout]: %s", f)
	}
	// Every label produced must be consumed, or ffmpeg rejects the graph.
	for _, label := range []string{"[src0]", "[src1]", "[hud0]", "[hud1]", "[v0]", "[v1]"} {
		if strings.Count(f, label) != 2 {
			t.Errorf("label %s appears %d times, want exactly one producer and one consumer: %s",
				label, strings.Count(f, label), f)
		}
	}
}

func TestDefaultFormatsPrefersReelOverShort(t *testing.T) {
	// The whole point of the change: a caller that expresses no preference
	// gets the full-bleed vertical, and does not silently pay for three encodes.
	o := DefaultRenderOpts()
	if !o.wants(FormatReel) {
		t.Error("the default set must include reel")
	}
	if o.wants(FormatShort) {
		t.Error("short must be opt-in, so no-preference callers render two files, not three")
	}
	if !o.wants(FormatClip) {
		t.Error("the native master should still be a default output")
	}
}

// --- crop_span framing -------------------------------------------------
//
// The case that motivated all of this: an 8-second God of War window at
// 1920x1088 whose cutscene beat is a two-shot with Kratos at the left edge and
// Heimdall at the right. A full-bleed 9:16 crop keeps 612 of 1920 px (31.9%)
// and lands in the empty background between them, clipping both.

func TestSanitizeSpan(t *testing.T) {
	cases := []struct {
		name         string
		in0, in1     float64
		want0, want1 float64
	}{
		{"ordinary", 0.14, 0.86, 0.14, 0.86},
		{"reversed is not a span", 0.8, 0.2, 0, 0},
		{"degenerate is not a span", 0.5, 0.5, 0, 0},
		{"out of range is clamped", -0.4, 1.7, 0, 1},
		{"NaN falls back to none", math.NaN(), 0.9, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got0, got1 := sanitizeSpan(tc.in0, tc.in1)
			if got0 != tc.want0 || got1 != tc.want1 {
				t.Errorf("sanitizeSpan(%v, %v) = %v,%v want %v,%v", tc.in0, tc.in1, got0, got1, tc.want0, tc.want1)
			}
		})
	}
}

func TestFitCropLeavesFullBleedAloneWithoutASpan(t *testing.T) {
	// The regression that matters most: no span must reproduce exactly the
	// framing this renderer had before crop_span existed.
	for _, focus := range []float64{0.0, 0.25, 0.5, 0.62, 1.0} {
		want := reelCrop(1920, 1088, focus, 0.5)
		if got := fitCrop(1920, 1088, focus, 0.5, 0, 0); got != want {
			t.Errorf("fitCrop(focus %v, no span) = %+v, want the plain crop %+v", focus, got, want)
		}
	}
}

func TestFitCropSlidesRatherThanWidensWhenTheSpanFits(t *testing.T) {
	// A span narrower than 9:16 costs no screen area: the window slides to
	// contain it and the reel stays full-bleed. This is the single-subject
	// combat case, and it is also how a span corrects a bad focus point.
	full := reelCrop(1920, 1088, 0.5, 0.5)
	got := fitCrop(1920, 1088, 0.5, 0.5, 0.10, 0.25)

	if got.W != full.W {
		t.Errorf("crop width %d, want the full-bleed %d — a span this narrow must not cost any width", got.W, full.W)
	}
	if want := int(0.10 * float64(1920)); got.X > want {
		t.Errorf("crop starts at x=%d, want it slid left to at most %d so the span is inside it", got.X, want)
	}
	if got.X+got.W < int(0.25*float64(1920)) {
		t.Errorf("crop %+v does not reach the right end of the span", got)
	}
}

func TestFitCropWidensForATwoShot(t *testing.T) {
	// Kratos and Heimdall: the span is wider than 9:16 can hold, so the crop
	// widens and the reel pays for it in pad rather than slicing a character.
	l := planReelSpan(1920, 1088, 0.58, 0.5, 0.14, 0.86, nil, 0)

	if l.CanvasW != 1080 || l.CanvasH != 1920 {
		t.Fatalf("canvas %dx%d, want 1080x1920", l.CanvasW, l.CanvasH)
	}
	if l.FitWidth < 0.70 || l.FitWidth > 0.75 {
		t.Errorf("fit_width %.3f, want ~0.72 (the span widened to cover both characters)", l.FitWidth)
	}
	// Both ends of the span have to be inside the crop, or a character is cut.
	if want := int(0.14 * float64(l.SourceW)); l.Crop.X > want {
		t.Errorf("crop starts at %d, past the left end of the span at %d — Kratos is clipped", l.Crop.X, want)
	}
	if want := int(0.86 * float64(l.SourceW)); l.Crop.X+l.Crop.W < want {
		t.Errorf("crop ends at %d, short of the right end of the span at %d — Heimdall is clipped", l.Crop.X+l.Crop.W, want)
	}
	// And it letterboxes rather than stretching to fill.
	if l.Action.H >= l.CanvasH {
		t.Fatalf("action pane %+v fills the canvas; a crop this wide must letterbox", l.Action)
	}
	if frac := float64(l.Action.H) / float64(l.CanvasH); frac < 0.40 || frac > 0.48 {
		t.Errorf("action strip is %.0f%% of the canvas, want ~44%% — better than short.mp4's 32%%", frac*100)
	}
	if l.Action.Y != (l.CanvasH-l.Action.H)/2 && l.Action.Y != (l.CanvasH-l.Action.H)/2-1 {
		t.Errorf("action pane at y=%d, want it centred so neither edge sits under Instagram's chrome", l.Action.Y)
	}
	if l.Action.H%2 != 0 || l.Action.Y%2 != 0 {
		t.Errorf("action pane %+v has odd geometry; libx264 rejects it under yuv420p", l.Action)
	}
}

func TestFitCropAtFullSpanIsShortsFraming(t *testing.T) {
	// The limit case. A span covering the whole frame is "nothing may be cut",
	// which is short.mp4's answer arrived at from the other direction — the
	// whole frame as a strip. It must not silently do something else.
	l := planReelSpan(1920, 1088, 0.5, 0.5, 0, 1, nil, 0)
	if l.Crop.W != 1920 {
		t.Errorf("crop width %d, want the whole 1920px frame", l.Crop.W)
	}
	want := even(int(math.Round(float64(l.CanvasW) * 1088 / 1920)))
	if l.Action.H != want {
		t.Errorf("action height %d, want %d (the full frame scaled to canvas width)", l.Action.H, want)
	}
}

func TestReelFilterPadPath(t *testing.T) {
	l := planReelSpan(1920, 1088, 0.58, 0.5, 0.14, 0.86, nil, 0)
	f := reelFilter(l)

	// Two consumers of the source with no bands: the action pane and the pad.
	if !strings.Contains(f, "split=2") {
		t.Errorf("filter must split the source for the pane and the pad:\n%s", f)
	}
	if !strings.Contains(f, "boxblur") {
		t.Errorf("padded render has no blur in it:\n%s", f)
	}
	// The pad is cut from the same crop the pane shows, not the whole frame —
	// otherwise it reveals the content the crop deliberately excluded.
	cropExpr := fmt.Sprintf("crop=%d:%d:%d:%d", l.Crop.W, l.Crop.H, l.Crop.X, l.Crop.Y)
	if strings.Count(f, cropExpr) < 2 {
		t.Errorf("pad is not sourced from the action crop %q:\n%s", cropExpr, f)
	}
	if want := fmt.Sprintf("overlay=%d:%d", l.Action.X, l.Action.Y); !strings.Contains(f, want) {
		t.Errorf("action pane is not laid at %q:\n%s", want, f)
	}
	if !strings.HasSuffix(f, "[vout]") {
		t.Errorf("filter must end at [vout]:\n%s", f)
	}
}

func TestReelFilterPadPathWithABand(t *testing.T) {
	// A band and a pad at once: three consumers of the source, and the band
	// still has to be the last thing composited.
	edge := HUDRegion{Name: "left_edge", X: 0.0, Y: 0.02, W: 0.10, H: 0.05}
	l := planReelSpan(1920, 1088, 0.5, 0.5, 0.14, 0.86, []HUDRegion{edge}, 0)
	if len(l.Bands) != 1 {
		t.Fatalf("got %d bands (already=%v skipped=%v), want 1 outside the widened crop",
			len(l.Bands), l.AlreadyInFrame, l.SkippedBands)
	}
	f := reelFilter(l)
	if !strings.Contains(f, "split=3") {
		t.Errorf("want a 3-way split (pane, pad, one band):\n%s", f)
	}
	if !strings.HasSuffix(f, "[vout]") {
		t.Errorf("filter must end at [vout]:\n%s", f)
	}
	if b := l.Bands[0]; b.Dst.X < 0 || b.Dst.Y < 0 ||
		b.Dst.X+b.Dst.W > l.CanvasW || b.Dst.Y+b.Dst.H > l.CanvasH {
		t.Errorf("band %+v falls outside the %dx%d canvas", b.Dst, l.CanvasW, l.CanvasH)
	}
}

func TestGhostIsMeasuredInsideTheActionPane(t *testing.T) {
	// A ghost measured against the canvas instead of the letterboxed pane would
	// be too tall and offset upward by the pad, putting the band that covers it
	// somewhere the clipped remnant isn't.
	l := planReelSpan(1920, 1088, 0.5, 0.5, 0.14, 0.86, nil, 0)
	src := regionRect(1920, 1088, HUDRegion{X: 0.10, Y: 0.70, W: 0.15, H: 0.12})

	ghost := ghostRect(l.Action, l.Crop, src)
	if ghost.W <= 0 || ghost.H <= 0 {
		t.Fatalf("premise broken: the region should partly survive this crop")
	}
	if ghost.Y < l.Action.Y {
		t.Errorf("ghost at y=%d sits above the action pane at y=%d — the pad was not accounted for", ghost.Y, l.Action.Y)
	}
	if ghost.Y+ghost.H > l.Action.Y+l.Action.H {
		t.Errorf("ghost %+v runs past the bottom of the action pane %+v", ghost, l.Action)
	}
}
