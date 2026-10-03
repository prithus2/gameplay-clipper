package cutter

import (
	"bytes"
	"fmt"
	"math"
	"os/exec"
	"sort"
	"strings"
)

// reel.mp4 is the full-bleed vertical render: the action cropped to fill a
// 9:16 canvas edge to edge, with the HUD that crop would have cut off
// composited back over it.
//
// Why crop rather than blur-pad (short.mp4): a 9:16 blur-pad keeps the whole
// 16:9 frame but spends 68% of the canvas on decoration. Instagram's published
// recommendation guidance asks for "9 x 16 vertical videos with no borders" and
// lists reels that "contain borders" among the reasons a reel is made less
// visible; independently of how that classifier treats a blurred pad
// specifically, watch time has been the top-ranked Reels signal since January
// 2025, and two thirds of a phone screen is a large thing to hand back on the
// surface where screen real estate converts into dwell time.
//
// Why not a *plain* crop: 9:16 out of 16:9 keeps only (9/16)/(16/9) = 31.6% of
// the source width — x 0.342 to 0.658. Most HUD regions worth declaring are
// wider than that (valo's kill-streak banner spans x 0.273-0.726, mk's
// fatality prompt 0.25-0.75), so a plain crop ships an Ace clip with the
// banner proving it was an Ace sliced off at both ends. The bands put those
// back — but only where the canvas is upscaled enough relative to the crop to
// land them *larger* than the 16:9 original; see minBandMagnification below
// for the case a band can't do that (God of War's subtitle band, x
// 0.10-0.90, was exactly this — it's no longer declared in profiles/gow.yaml
// because compositing it back only made it smaller).
const (
	// Instagram's stated reel spec. A crop smaller than this is upscaled to
	// reach it: "low-resolution" reels are one of the stated reasons a reel is
	// made less visible, and upscaling — while it adds no detail — at least
	// stops the file itself being the reason the post is demoted. A crop
	// larger than this is left alone for Instagram to downscale.
	reelMinWidth  = 1080
	reelMinHeight = 1920

	// Instagram overlays its own UI on the reel: the caption, username and
	// audio ticker along the bottom, the action rail up the right side, and
	// chrome across the top. Bands are kept out of those zones so a killfeed
	// doesn't end up underneath the like button.
	reelSafeTopFrac    = 0.10
	reelSafeBottomFrac = 0.20

	// A single band may not eat more than this fraction of the canvas height.
	// Without the cap, a tall source region (mk's fatality prompt is 20% of
	// frame height) scaled to full canvas width would cover the action it is
	// supposed to be annotating.
	reelMaxBandFrac = 0.22

	// Vertical gap between two bands stacked on the same edge.
	reelBandGapFrac = 0.015

	// How long a signal-gated band stays up after its signal fires, and how
	// far before it the band fades in. Detectors sample at 1-2 fps and fire
	// while the element is already on screen, so a little lead catches the
	// frames between the element appearing and the sampler noticing.
	defaultBandHoldS = 3.0
	bandLeadS        = 1.0

	// Blur radius and dim applied to the pad when the crop is too wide to fill
	// the canvas. Both are a fraction of canvas width rather than fixed pixels,
	// for the same reason renderShort's blur is: a radius tuned at 1080 wide
	// reads as nearly sharp at 2160. The slight dim keeps the eye on the action
	// strip instead of the backdrop, which matters more here than in short.mp4
	// because the strip is larger and the two are closer in brightness.
	reelPadBlurAt1080 = 26
	reelPadDim        = -0.06

	// minBandMagnification is the least a band may scale its element's width
	// (rendered px / source px) to be composited at all. A band exists to make
	// an element MORE legible after the crop discards it, not less, and below
	// 1.0 it can't even do that: God of War's subtitle band is the case that
	// forced this constant into existence. Its caption region is 3072px wide
	// against a 1214px-wide canvas (a 4K master's reel crop, which is 1:1
	// vertically — no scaling at all), so the band scales the text DOWN by
	// 0.39x while the ghost it exists to cover doesn't shrink vertically at
	// all. Content height over plate height therefore pins at ~0.39 regardless
	// of how the region's height is tuned — the plate is structurally ~60%
	// black bars, not a tuning mistake. Compare Valorant's kill-streak banner,
	// which goes 410px -> 1074px (2.6x) and reads correctly: a band only earns
	// its place when it magnifies. 1.0 is the break-even point, not a safety
	// margin — at exactly 1.0 the band adds no black plate but also no
	// legibility over the ghost, so this is a floor, not a target to tune.
	minBandMagnification = 1.0
)

// rect is a pixel rectangle. Used for both source-frame and canvas
// coordinates; which one is meant is documented at each use.
type rect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

// ReelBand is one HUD region re-composited over the reframed action.
type ReelBand struct {
	Name string `json:"name"`
	// Src is the region in source-frame pixels.
	Src rect `json:"src"`
	// Dst is where the band lands on the canvas, including its border.
	Dst rect `json:"dst"`
	// Inner is the scaled region itself: X/Y are its offset inside Dst (the
	// border width), W/H its size.
	Inner rect `json:"inner"`
	// Enable is an ffmpeg timeline expression limiting the band to the moments
	// its element is on screen. Empty means always up.
	Enable string `json:"enable,omitempty"`
}

// ReelLayout is the resolved geometry of one reel.mp4 render. It is written
// next to the clip as reel_layout.json so a published reel can be joined back
// to the framing decision that produced it — otherwise an A/B between
// blur-pad and full-bleed tells you which file won but not why.
type ReelLayout struct {
	SourceW int     `json:"source_w"`
	SourceH int     `json:"source_h"`
	CanvasW int     `json:"canvas_w"`
	CanvasH int     `json:"canvas_h"`
	FocusX  float64 `json:"focus_x"`
	FocusY  float64 `json:"focus_y"`
	// SpanX0/SpanX1 are the horizontal extent the framing was asked to keep, in
	// full-frame fractions (llm.ClipMetadata.CropSpan, or a --reel-fit-width
	// override). Zero-width means none was supplied and the crop is the plain
	// full-bleed 9:16 window.
	SpanX0 float64 `json:"span_x0,omitempty"`
	SpanX1 float64 `json:"span_x1,omitempty"`
	// Crop is the action pane's rectangle in source-frame pixels.
	Crop rect `json:"crop"`
	// FitWidth is the fraction of the source width Crop kept. 9/16 ÷ 16/9 =
	// 0.316 is the full-bleed case; 1.0 means the whole frame was needed, which
	// is short.mp4's framing arrived at from the other direction.
	FitWidth float64 `json:"fit_width"`
	// Action is where the scaled crop lands on the canvas. When it covers the
	// canvas the render is full-bleed; when it is shorter, the remainder is
	// blurred pad — the price of a span too wide for 9:16 to hold.
	Action rect       `json:"action"`
	Bands  []ReelBand `json:"bands,omitempty"`
	// AlreadyInFrame names HUD regions that needed no band because the crop
	// happened to contain them outright.
	AlreadyInFrame []string `json:"already_in_frame,omitempty"`
	// SkippedBands names HUD regions that were left out not because the crop
	// already showed them, but because the band mechanism could not help:
	// composited at this canvas size, the element would render SMALLER than
	// it is in the source (see minBandMagnification). Distinct from
	// AlreadyInFrame — that means the crop already contains the element; this
	// means it doesn't, and covering it anyway would look worse than the
	// clipped remnant it was meant to replace. Kept here so a mostly-black
	// reel is diagnosable from reel_layout.json instead of looking like a
	// silent no-op.
	SkippedBands []string `json:"skipped_bands,omitempty"`
}

// renderReel cuts the window and reframes it to full-bleed 9:16 in one pass,
// returning the geometry it used. The crop is centred on the LLM's crop_focus
// when there is one and on the middle of the frame when there isn't, so this
// works with no API key at all — it just frames on the centre.
func renderReel(inputFile, outputFile string, c Clip, opts RenderOpts) (ReelLayout, error) {
	srcW, srcH, err := probeDimensions(inputFile)
	if err != nil {
		return ReelLayout{}, err
	}

	focusX, focusY := 0.5, 0.5
	spanX0, spanX1 := 0.0, 0.0
	if c.Metadata != nil {
		focusX = sanitizeFocus(c.Metadata.CropFocus.X)
		focusY = sanitizeFocus(c.Metadata.CropFocus.Y)
		spanX0, spanX1 = sanitizeSpan(c.Metadata.CropSpan.X0, c.Metadata.CropSpan.X1)
	}
	// An explicit --reel-span overrides whatever the model said: the escape
	// hatch for when it misjudges what has to stay in shot, and the only way to
	// get this framing at all with no API key.
	if x0, x1 := sanitizeSpan(opts.SpanX0, opts.SpanX1); x1 > x0 {
		spanX0, spanX1 = x0, x1
	}

	duration := c.End - c.Start
	layout := planReelSpan(srcW, srcH, focusX, focusY, spanX0, spanX1, opts.HUD, opts.ReelHeight)
	layout.Bands = gateBands(layout.Bands, opts.HUD, c.Signals, c.Start, duration)

	// -filter_complex suppresses automatic stream selection, so audio is
	// mapped by hand; 0:a? keeps a silent source from failing outright, and
	// the audio is re-encoded because the master is typically Opus-in-WebM,
	// which mp4 won't take. +faststart because this is the file that gets
	// uploaded.
	cmd := exec.Command("ffmpeg", "-y",
		"-ss", fmt.Sprintf("%.3f", c.Start), "-i", inputFile, "-t", fmt.Sprintf("%.3f", duration),
		"-filter_complex", reelFilter(layout), "-map", "[vout]", "-map", "0:a?",
		"-c:v", "libx264", "-preset", opts.preset(), "-crf", opts.crf(),
		"-pix_fmt", "yuv420p", "-threads", encodeThreads,
		"-movflags", "+faststart",
		"-c:a", "aac", outputFile)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return layout, fmt.Errorf("reel render failed: %v, stderr: %s", err, stderr.String())
	}
	return layout, nil
}

// sanitizeFocus clamps an LLM-supplied focus coordinate into [0,1]. The model
// is asked for a fraction but is not constrained to return one, and a NaN or
// an out-of-range value would otherwise propagate into a crop offset.
func sanitizeFocus(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0.5
	}
	return math.Max(0, math.Min(1, v))
}

// sanitizeSpan clamps an LLM-supplied horizontal span into [0,1] and orders it.
// A degenerate or full-frame-by-accident answer is reduced to "no span": the
// model is asked for a fraction pair but is not constrained to return one, and
// a reversed or NaN span would otherwise widen the crop to nonsense.
func sanitizeSpan(x0, x1 float64) (float64, float64) {
	if math.IsNaN(x0) || math.IsNaN(x1) || math.IsInf(x0, 0) || math.IsInf(x1, 0) {
		return 0, 0
	}
	x0, x1 = math.Max(0, math.Min(1, x0)), math.Max(0, math.Min(1, x1))
	if x1 <= x0 {
		return 0, 0
	}
	return x0, x1
}

// planReel resolves the full geometry of a reel render with no span supplied —
// the plain full-bleed 9:16 framing. It is pure, so the framing decisions are
// testable without invoking ffmpeg.
func planReel(srcW, srcH int, focusX, focusY float64, hud []HUDRegion, heightOverride int) ReelLayout {
	return planReelSpan(srcW, srcH, focusX, focusY, 0, 0, hud, heightOverride)
}

// planReelSpan resolves the full geometry of a reel render, widening the crop
// to cover spanX0..spanX1 when a plain 9:16 window cannot hold it. Also pure.
func planReelSpan(srcW, srcH int, focusX, focusY, spanX0, spanX1 float64, hud []HUDRegion, heightOverride int) ReelLayout {
	canvasW, canvasH := reelCanvas(srcW, srcH, heightOverride)
	crop := fitCrop(srcW, srcH, focusX, focusY, spanX0, spanX1)
	action := actionRect(canvasW, canvasH, crop)
	l := ReelLayout{
		SourceW: srcW, SourceH: srcH,
		CanvasW: canvasW, CanvasH: canvasH,
		FocusX: focusX, FocusY: focusY,
		SpanX0: spanX0, SpanX1: spanX1,
		Crop:     crop,
		FitWidth: float64(crop.W) / float64(srcW),
		Action:   action,
	}
	l.Bands, l.AlreadyInFrame, l.SkippedBands = planBands(srcW, srcH, canvasW, canvasH, crop, action, hud)
	return l
}

// fitCrop is the framing decision: the tightest source rectangle that both
// respects the focus point and keeps the requested span in shot.
//
// Three outcomes, in order of how good they look:
//
//   - No span, or a span the full-bleed 9:16 window already covers: that window
//     is used unchanged, and the reel fills the canvas edge to edge. This is
//     what most single-subject action should land on.
//   - A span the 9:16 window could cover but isn't currently pointed at: the
//     window slides to contain it. Still full-bleed — the span has corrected
//     the focus rather than costing any screen area.
//   - A span wider than 9:16 can hold: the crop widens to the span, which means
//     the action pane no longer fills the canvas vertically and the remainder
//     becomes pad. Screen area is traded for not slicing a subject in half. At
//     the limit — a span covering the whole frame — this is exactly short.mp4's
//     framing, reached from the other end.
func fitCrop(srcW, srcH int, focusX, focusY, spanX0, spanX1 float64) rect {
	crop := reelCrop(srcW, srcH, focusX, focusY)
	if spanX1 <= spanX0 {
		return crop
	}

	// Floor/ceil rather than round: the span is a "must not be cut" promise, so
	// rounding is resolved outward, never inward.
	want0 := clampInt(int(math.Floor(spanX0*float64(srcW))), 0, srcW)
	want1 := clampInt(int(math.Ceil(spanX1*float64(srcW))), 0, srcW)

	if want1-want0 <= crop.W {
		// Fits full-bleed: slide the window over it instead of widening.
		x := crop.X
		if want0 < x {
			x = want0
		}
		if want1 > x+crop.W {
			x = want1 - crop.W
		}
		crop.X = clampInt(x, 0, srcW-crop.W)
		return crop
	}

	w := even(want1 - want0)
	if w > srcW {
		w = even(srcW)
	}
	centre := (want0 + want1) / 2
	return rect{
		X: clampInt(centre-w/2, 0, srcW-w),
		Y: crop.Y, W: w, H: crop.H,
	}
}

// actionRect places the scaled crop on the canvas. A 9:16 crop covers it
// exactly; anything wider is scaled to canvas width and centred vertically,
// leaving pad above and below.
//
// Centred rather than pinned to an edge because Instagram's chrome eats both
// the top and bottom of the canvas (see reelSafeTopFrac/reelSafeBottomFrac) —
// pinning the strip to one edge would push it under that UI.
func actionRect(canvasW, canvasH int, crop rect) rect {
	h := even(int(math.Round(float64(canvasW) * float64(crop.H) / float64(crop.W))))
	if h >= canvasH {
		return rect{X: 0, Y: 0, W: canvasW, H: canvasH}
	}
	return rect{X: 0, Y: even((canvasH - h) / 2), W: canvasW, H: h}
}

// reelCanvas picks the 9:16 canvas for a reel render.
//
// Unlike shortCanvas — which sizes from the source's short side because the
// whole frame has to fit inside it — this sizes from the crop the reel will
// actually take, so the action pane is shown at 1:1 wherever the master allows
// it. A 3840x2160 master crops to 1214x2160 and is encoded at exactly that:
// no scaling of the gameplay at all, and still above Instagram's 1080x1920.
// A 2560x1440 master crops to 810x1440, which is below spec, so it is
// upscaled to 1080x1920 rather than shipped under the bar.
func reelCanvas(srcW, srcH, override int) (w, h int) {
	if override > 0 {
		h = even(override)
		return even(h * 9 / 16), h
	}
	w, h = naturalReelSize(srcW, srcH)
	if h < reelMinHeight {
		return reelMinWidth, reelMinHeight
	}
	return w, h
}

// naturalReelSize is the largest 9:16 rectangle that fits inside the source
// frame, which is both the crop size and (subject to the resolution floor) the
// canvas size.
func naturalReelSize(srcW, srcH int) (w, h int) {
	w = even(srcH * 9 / 16)
	if w <= srcW {
		return w, even(srcH)
	}
	// The source is already narrower than 9:16 — an already-vertical capture.
	// Keep its full width and crop height instead of inventing pixels at the
	// sides.
	w = even(srcW)
	return w, even(srcW * 16 / 9)
}

// reelCrop picks the source-space rectangle the action pane is cut from: the
// largest 9:16 window the source allows, slid to centre on the focus point and
// clamped so it never runs off the frame. A focus near an edge therefore
// yields an edge-aligned crop rather than a crop with a black margin.
func reelCrop(srcW, srcH int, focusX, focusY float64) rect {
	w, h := naturalReelSize(srcW, srcH)
	if w > srcW {
		w = even(srcW)
	}
	if h > srcH {
		h = even(srcH)
	}
	x := int(math.Round(focusX*float64(srcW))) - w/2
	y := int(math.Round(focusY*float64(srcH))) - h/2
	return rect{
		X: clampInt(x, 0, srcW-w),
		Y: clampInt(y, 0, srcH-h),
		W: w, H: h,
	}
}

// planBands places each HUD region on the canvas. A region the crop already
// contains outright is skipped — it is visible in the action pane, and
// compositing it a second time would duplicate it on screen. A region whose
// band would shrink it below source size is also skipped — see
// minBandMagnification — and named in the returned skipped list rather than
// silently rendered as a mostly-black plate.
func planBands(srcW, srcH, canvasW, canvasH int, crop, action rect, hud []HUDRegion) (bands []ReelBand, already, skipped []string) {
	border := canvasW / 360
	if border < 2 {
		border = 2
	}
	gap := int(float64(canvasH) * reelBandGapFrac)
	topCursor := int(float64(canvasH) * reelSafeTopFrac)
	bottomCursor := canvasH - int(float64(canvasH)*reelSafeBottomFrac)

	for _, r := range hud {
		src := regionRect(srcW, srcH, r)
		if src.W < 2 || src.H < 2 {
			continue
		}
		if containsRect(crop, src) {
			already = append(already, r.Name)
			continue
		}

		scale := r.Scale
		if scale <= 0 {
			scale = 1
		}
		// The border pads inward, not outward: at scale 1.0 the *outer* box has
		// to be the canvas width, or the band overhangs the edge and gets
		// clipped by exactly the border it was given.
		innerW := even(even(int(float64(canvasW)*scale)) - 2*border)
		innerH := even(int(math.Round(float64(innerW) * float64(src.H) / float64(src.W))))
		// Cap the band's height, re-deriving its width so the aspect ratio of
		// the captured region is preserved rather than squashed.
		if maxH := even(int(float64(canvasH) * reelMaxBandFrac)); innerH > maxH {
			innerH = maxH
			innerW = even(int(math.Round(float64(innerH) * float64(src.W) / float64(src.H))))
		}
		if innerW < 2 || innerH < 2 {
			continue
		}
		if float64(innerW) < minBandMagnification*float64(src.W) {
			// Composited, this element would come out SMALLER than it already
			// is in the source — see minBandMagnification. That's not a
			// smaller version of the fix, it's a plate that's mostly black
			// padding around a shrunk, harder-to-read copy: worse than the
			// clipped ghost it would replace. Record it so a mostly-black reel
			// is diagnosable from reel_layout.json instead of a silent no-op.
			skipped = append(skipped, r.Name)
			continue
		}

		outerW, outerH := innerW+2*border, innerH+2*border
		dst := rect{W: outerW, H: outerH}

		// Where the surviving sliver of this element still shows in the action
		// pane. If it is non-empty, the element is *partly* visible — put the
		// band there, sized to cover it, so the full version replaces the
		// clipped one instead of appearing alongside it. God of War's subtitles
		// are the case that forced this: the crop keeps the middle third of a
		// caption line, and a band placed down in the safe zone left the
		// clipped original ghosting above it.
		if ghost := ghostRect(action, crop, src); ghost.W > 0 && ghost.H > 0 {
			dst = coverRect(ghost, outerW, outerH)
			dst.X = clampInt(dst.X, 0, maxInt(0, canvasW-dst.W))
			dst.Y = clampInt(dst.Y, 0, maxInt(0, canvasH-dst.H))
		} else {
			// Nothing of it survived the crop, so there is nothing to line up
			// with: place it where a viewer will actually read it, clear of
			// Instagram's own UI.
			dst.X = clampInt((canvasW-outerW)/2, 0, maxInt(0, canvasW-outerW))
			if placeOf(r) == "top" {
				dst.Y = topCursor
				topCursor += outerH + gap
			} else {
				dst.Y = bottomCursor - outerH
				bottomCursor -= outerH + gap
			}
			dst.Y = clampInt(dst.Y, 0, maxInt(0, canvasH-outerH))
		}

		bands = append(bands, ReelBand{
			Name:  r.Name,
			Src:   src,
			Dst:   dst,
			Inner: rect{X: (dst.W - innerW) / 2, Y: (dst.H - innerH) / 2, W: innerW, H: innerH},
		})
	}
	return bands, already, skipped
}

// gateBands resolves each band's timeline expression, limiting a transient
// element's band to the moments it is actually on screen — and drops bands
// that have no evidence at all that their element was ever visible.
//
// Without gating the format is worse than it sounds: valo's kill-streak
// banner is up for a couple of seconds, so a permanently-composited band
// would spend the rest of a 40-second Ace showing an enlarged strip of the
// floor below the crosshair.
//
// "No evidence" is deliberately narrower than "the signal didn't fire": when
// signals is non-empty, detection ran over this clip and simply didn't catch
// this element that time, which valo.yaml documents happening mid-streak
// (OCR missing the TRIPLE/PENTA banners between two it did catch) — so the
// band stays up rather than hiding the payoff exactly when the detector was
// the thing that failed. But when signals is empty, detection never ran at
// all: a hand-picked --include window skips it entirely (see Clip.Signals),
// so there is no "OCR might have missed it" story to fall back on, only a
// band that would sit up covering nothing for the clip's whole length. That
// was the actual God of War subtitle-band defect — a manual include of a
// cutscene rendered as a permanent full-width black bar.
//
// Pure and returns the filtered slice rather than mutating in place, because
// unlike setting Enable, removing a band changes its length.
func gateBands(bands []ReelBand, hud []HUDRegion, signals []FiredSignal, clipStart, duration float64) []ReelBand {
	byName := make(map[string]HUDRegion, len(hud))
	for _, r := range hud {
		byName[r.Name] = r
	}
	kept := make([]ReelBand, 0, len(bands))
	for _, b := range bands {
		r, ok := byName[b.Name]
		if !ok || r.Signal == "" {
			kept = append(kept, b) // a persistent element: leave it up for the whole clip
			continue
		}
		if len(signals) == 0 {
			continue // no detection ran at all: no evidence, so no band (see above)
		}
		hold := r.HoldS
		if hold <= 0 {
			hold = defaultBandHoldS
		}
		windows := signalWindows(signals, r.Signal, clipStart, duration, hold)
		if len(windows) == 0 {
			kept = append(kept, b) // declared but silent this time: the OCR-miss fallback (see above)
			continue
		}
		b.Enable = enableExpr(windows)
		kept = append(kept, b)
	}
	return kept
}

// timeWindow is a clip-relative visible span in seconds.
type timeWindow struct{ Start, End float64 }

// signalWindows turns the fire times of one signal into merged, clip-relative
// visible spans, clamped to the clip. Absolute source seconds come in; output
// seconds go out, because ffmpeg's timeline clock starts at zero after the
// input seek.
func signalWindows(signals []FiredSignal, name string, clipStart, duration, hold float64) []timeWindow {
	var out []timeWindow
	for _, s := range signals {
		if s.Kind != name {
			continue
		}
		rel := s.T - clipStart
		w := timeWindow{Start: rel - bandLeadS, End: rel + hold}
		if w.End <= 0 || w.Start >= duration {
			continue // fired outside the window this clip actually covers
		}
		out = append(out, timeWindow{
			Start: math.Max(0, w.Start),
			End:   math.Min(duration, w.End),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })

	// Merge overlaps so a burst of detections becomes one span rather than a
	// dozen redundant terms in the expression.
	var merged []timeWindow
	for _, w := range out {
		if n := len(merged); n > 0 && w.Start <= merged[n-1].End {
			if w.End > merged[n-1].End {
				merged[n-1].End = w.End
			}
			continue
		}
		merged = append(merged, w)
	}
	return merged
}

// enableExpr renders visible spans as an ffmpeg timeline expression. The terms
// are summed because between() yields 1/0 and ffmpeg treats any non-zero
// result as true; the whole thing is single-quoted so its commas are not read
// as filter-argument separators.
func enableExpr(windows []timeWindow) string {
	terms := make([]string, len(windows))
	for i, w := range windows {
		terms[i] = fmt.Sprintf("between(t,%.3f,%.3f)", w.Start, w.End)
	}
	return "'" + strings.Join(terms, "+") + "'"
}

// placeOf resolves which edge a band sits on, defaulting to the half of the
// source frame the region came from — a bottom-of-screen killfeed stays a
// bottom-of-screen killfeed.
func placeOf(r HUDRegion) string {
	if r.Place == "top" || r.Place == "bottom" {
		return r.Place
	}
	if r.Y+r.H/2 < 0.5 {
		return "top"
	}
	return "bottom"
}

// regionRect converts a fractional HUD region to source pixels, clamped to the
// frame and rounded to even dimensions (yuv420p subsamples chroma by two, so
// odd crop sizes are rejected).
func regionRect(srcW, srcH int, r HUDRegion) rect {
	x := clampInt(int(math.Round(r.X*float64(srcW))), 0, srcW)
	y := clampInt(int(math.Round(r.Y*float64(srcH))), 0, srcH)
	w := even(clampInt(int(math.Round(r.W*float64(srcW))), 0, srcW-x))
	h := even(clampInt(int(math.Round(r.H*float64(srcH))), 0, srcH-y))
	return rect{X: x, Y: y, W: w, H: h}
}

// ghostRect maps the part of a source region that survives the crop into
// canvas coordinates — the clipped remnant the viewer can still see in the
// action pane. Empty when the crop excludes the region entirely.
//
// Mapped through the action pane rather than the canvas: once a wide span
// letterboxes the render, the pane is a sub-rectangle of the canvas, and a
// ghost measured against the full canvas would be both too tall and offset
// upward by the pad — putting the band that is meant to cover it somewhere
// else entirely.
func ghostRect(action, crop, src rect) rect {
	x0, x1 := maxInt(src.X, crop.X), minInt(src.X+src.W, crop.X+crop.W)
	y0, y1 := maxInt(src.Y, crop.Y), minInt(src.Y+src.H, crop.Y+crop.H)
	if x1 <= x0 || y1 <= y0 {
		return rect{}
	}
	sx := float64(action.W) / float64(crop.W)
	sy := float64(action.H) / float64(crop.H)
	gx := action.X + int(math.Round(float64(x0-crop.X)*sx))
	gy := action.Y + int(math.Round(float64(y0-crop.Y)*sy))
	return rect{
		X: gx, Y: gy,
		W: action.X + int(math.Round(float64(x1-crop.X)*sx)) - gx,
		H: action.Y + int(math.Round(float64(y1-crop.Y)*sy)) - gy,
	}
}

// coverRect returns a box at least w x h that fully contains target, centred on
// it. Used to size a band so its (black) padding hides the clipped remnant it
// is replacing.
func coverRect(target rect, w, h int) rect {
	outW, outH := maxInt(w, target.W), maxInt(h, target.H)
	return rect{
		X: target.X + (target.W-outW)/2,
		Y: target.Y + (target.H-outH)/2,
		W: even(outW), H: even(outH),
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// containsRect reports whether outer fully encloses inner.
func containsRect(outer, inner rect) bool {
	return inner.X >= outer.X && inner.Y >= outer.Y &&
		inner.X+inner.W <= outer.X+outer.W &&
		inner.Y+inner.H <= outer.Y+outer.H
}

// reelFilter builds the filter_complex for a planned layout: crop the action
// pane and scale it to the canvas, then crop each HUD band from the *source*
// (not from the already-cropped pane, which by definition no longer contains
// it) and overlay it.
//
// setsar=1 because a source with non-square pixels would otherwise carry its
// sample aspect ratio through the crop and display stretched.
func reelFilter(l ReelLayout) string {
	cropExpr := fmt.Sprintf("crop=%d:%d:%d:%d", l.Crop.W, l.Crop.H, l.Crop.X, l.Crop.Y)
	action := fmt.Sprintf("%s,scale=%d:%d:flags=lanczos,setsar=1", cropExpr, l.Action.W, l.Action.H)
	padded := l.Action.W < l.CanvasW || l.Action.H < l.CanvasH

	if len(l.Bands) == 0 && !padded {
		return "[0:v]" + action + "[vout]"
	}

	// The source is consumed once per band, once for the action pane, and once
	// more for the pad when there is one, so it has to be split rather than
	// referenced repeatedly.
	var b strings.Builder
	inputs := len(l.Bands) + 1
	if padded {
		inputs++
	}
	fmt.Fprintf(&b, "[0:v]split=%d[base]", inputs)
	if padded {
		b.WriteString("[padsrc]")
	}
	for i := range l.Bands {
		fmt.Fprintf(&b, "[src%d]", i)
	}
	b.WriteString(";")

	// The first stage produces [v0] for the bands to composite onto, or
	// [vout] directly when there are none.
	stage0 := "[v0]"
	if len(l.Bands) == 0 {
		stage0 = "[vout]"
	}
	if padded {
		blur := reelPadBlurAt1080 * l.CanvasW / 1080
		if blur < 2 {
			blur = 2
		}
		// The pad is built from the same crop the action pane shows, enlarged —
		// not from the whole source frame. Padding that reveals content the crop
		// deliberately excluded reads as a mistake rather than as a backdrop,
		// and on a wide two-shot it would put a sharp-edged ghost of a character
		// directly above the strip they already appear in.
		fmt.Fprintf(&b, "[padsrc]%s,scale=%d:%d:force_original_aspect_ratio=increase,crop=%d:%d,boxblur=%d:5,eq=brightness=%.2f,setsar=1[bg];",
			cropExpr, l.CanvasW, l.CanvasH, l.CanvasW, l.CanvasH, blur, reelPadDim)
		fmt.Fprintf(&b, "[base]%s[fg];", action)
		fmt.Fprintf(&b, "[bg][fg]overlay=%d:%d%s", l.Action.X, l.Action.Y, stage0)
	} else {
		fmt.Fprintf(&b, "[base]%s%s", action, stage0)
	}
	if len(l.Bands) == 0 {
		return b.String()
	}
	b.WriteString(";")

	for i, band := range l.Bands {
		fmt.Fprintf(&b, "[src%d]crop=%d:%d:%d:%d,scale=%d:%d:flags=lanczos,pad=%d:%d:%d:%d:black[hud%d];",
			i, band.Src.W, band.Src.H, band.Src.X, band.Src.Y,
			band.Inner.W, band.Inner.H,
			band.Dst.W, band.Dst.H, band.Inner.X, band.Inner.Y, i)
	}

	for i, band := range l.Bands {
		out := fmt.Sprintf("[v%d]", i+1)
		if i == len(l.Bands)-1 {
			out = "[vout]"
		}
		enable := ""
		if band.Enable != "" {
			enable = ":enable=" + band.Enable
		}
		fmt.Fprintf(&b, "[v%d][hud%d]overlay=%d:%d%s%s", i, i, band.Dst.X, band.Dst.Y, enable, out)
		if i < len(l.Bands)-1 {
			b.WriteString(";")
		}
	}
	return b.String()
}

func clampInt(v, lo, hi int) int {
	if hi < lo {
		return lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
