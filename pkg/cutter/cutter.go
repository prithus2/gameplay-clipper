package cutter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gameplay-clipper/pkg/llm"
)

// Format names one renderable output for an approved window. The reviewer
// picks these per candidate in the review UI, so a clip that only ever needs
// a vertical cut doesn't pay for a full-resolution master too.
type Format string

const (
	// FormatClip is the native-resolution cut straight from the master file.
	FormatClip Format = "clip"
	// FormatShort is the 9:16 blur-pad render: the whole 16:9 frame preserved
	// as a centre strip over a blurred enlargement of itself. Nothing is lost
	// from the frame, but roughly two thirds of the canvas is decoration.
	FormatShort Format = "short"
	// FormatReel is the 9:16 full-bleed render: the action cropped to fill the
	// canvas edge to edge, with the HUD the crop would have cut off composited
	// back over it. This is the Instagram default — see renderReel.
	FormatReel Format = "reel"
)

// AllFormats is every renderable output, used to validate names and to
// document the choice. It is deliberately *not* the fallback set: see
// DefaultFormats.
var AllFormats = []Format{FormatClip, FormatShort, FormatReel}

// DefaultFormats is what a caller gets when it never presents a choice
// (--include windows, or an approve request that sends no selection).
//
// reel, not short, is the vertical default. Instagram's published
// recommendation guidance asks for "9 x 16 vertical videos with no borders",
// and lists reels that "contain borders" among the reasons a reel is made less
// visible; separately, watch time has been the top-ranked Reels signal since
// January 2025, and a canvas that spends 68% of itself on blur spends screen
// real estate rather than earning attention with it. short remains available
// for the case where cropping genuinely destroys the composition — wide
// cinematic framing — which profiles opt into per archetype.
//
// short is left out of the default set rather than added alongside reel so a
// caller that expresses no preference doesn't silently pay for three encodes
// of the same window.
var DefaultFormats = []Format{FormatClip, FormatReel}

// validFormat reports whether f names a real output.
func validFormat(f Format) bool {
	for _, known := range AllFormats {
		if f == known {
			return true
		}
	}
	return false
}

// Encode defaults. CRF 18 is visually lossless on gameplay footage, which
// matters more here than elsewhere because fast camera motion and particle
// effects are exactly what x264's default CRF 23 smears.
const (
	DefaultCRF    = 18
	DefaultPreset = "slow"
)

// ParseFormats turns comma/flag-supplied names into Formats, rejecting
// unknown ones rather than silently rendering less than the caller asked for.
// Blank entries are skipped so "clip," and " clip , short " both work.
func ParseFormats(names []string) ([]Format, error) {
	var out []Format
	seen := map[Format]bool{}
	for _, n := range names {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == "" {
			continue
		}
		f := Format(n)
		if !validFormat(f) {
			return nil, fmt.Errorf("unknown format %q (want one of %s)", n, formatNames())
		}
		if seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no output formats selected (want at least one of %s)", formatNames())
	}
	return out, nil
}

// formatNames renders the valid format list for an error message.
func formatNames() string {
	names := make([]string, len(AllFormats))
	for i, f := range AllFormats {
		names[i] = strconv.Quote(string(f))
	}
	return strings.Join(names, ", ")
}

// RenderOpts controls which outputs get written and how they're encoded.
// A zero CRF/Preset falls back to the package defaults, so callers that only
// care about Formats can leave the rest alone.
type RenderOpts struct {
	Formats []Format
	CRF     int
	Preset  string
	// ShortHeight overrides the 9:16 canvas height for short.mp4. 0 derives it
	// from the source (see shortCanvas), which is what you almost always want.
	ShortHeight int
	// ReelHeight overrides the 9:16 canvas height for reel.mp4. 0 derives it
	// from the source (see reelCanvas).
	ReelHeight int
	// SpanX0/SpanX1 override the horizontal extent reel.mp4 must keep, in
	// full-frame fractions. Zero-width defers to the LLM's crop_span, which is
	// the intended path; set it when the model misjudges the span, or to get
	// this framing at all with no API key.
	//
	// It is a span rather than a width because the override has to be able to
	// say *where* as well as *how much*: the model getting the span wrong and
	// the model getting the focus wrong are the same failure, and a width
	// centred on a bad focus point cannot correct it. See fitCrop.
	SpanX0, SpanX1 float64
	// HUD names the on-screen elements the reel crop must not lose. Supplied
	// by the caller from the game profile (see main.toHUDRegions); empty means
	// a plain centre-weighted crop with nothing composited back.
	HUD []HUDRegion
}

// HUDRegion is one on-screen element that has to survive the vertical
// reframe, expressed in frame fractions (0-1) so it scales across capture
// resolutions. This mirrors profile.KeepVisible; the two are kept separate so
// the renderer doesn't depend on the profile loader, matching how
// scoring.Config is built from a profile in main.
type HUDRegion struct {
	Name       string
	X, Y, W, H float64
	// Place pins the band to the "top" or "bottom" of the canvas; empty
	// derives it from where the region sits in the source frame.
	Place string
	// Scale is the band's width as a fraction of canvas width; 0 means full.
	Scale float64
	// Signal names the detector signal this element is on screen for, gating
	// the band to those moments. Empty means a persistent element, always up.
	Signal string
	// HoldS is how long the band stays up after its signal fires; 0 uses
	// defaultBandHoldS.
	HoldS float64
}

// FiredSignal is one detector hit inside a clip's window, in absolute
// source-file seconds. Used to gate transient HUD bands to the moments the
// element they capture is actually on screen.
type FiredSignal struct {
	Kind string
	T    float64
}

// DefaultRenderOpts renders the default outputs at the package's default quality.
func DefaultRenderOpts() RenderOpts {
	return RenderOpts{Formats: DefaultFormats, CRF: DefaultCRF, Preset: DefaultPreset}
}

// WithFormats returns a copy of o rendering exactly f, leaving the encode
// settings intact. Used by the review server to apply one reviewer's
// per-candidate checkbox selection on top of the run-wide encode config.
func (o RenderOpts) WithFormats(f []Format) RenderOpts {
	o.Formats = f
	return o
}

func (o RenderOpts) wants(f Format) bool {
	for _, want := range o.Formats {
		if want == f {
			return true
		}
	}
	return false
}

func (o RenderOpts) crf() string {
	if o.CRF <= 0 {
		return strconv.Itoa(DefaultCRF)
	}
	return strconv.Itoa(o.CRF)
}

func (o RenderOpts) preset() string {
	if o.Preset == "" {
		return DefaultPreset
	}
	return o.Preset
}

// Clip is one approved window ready to be cut, reformatted, and thumbnailed.
// Metadata is nil only for the (rare) case a clip was approved without any
// LLM metadata ever being generated for it.
type Clip struct {
	Start, End float64
	Metadata   *llm.ClipMetadata
	// Signals are the detector hits inside this window. Only reel.mp4 uses
	// them, to gate transient HUD bands; an empty slice leaves every band up
	// for the whole clip, which is the right fallback for a hand-picked
	// --include window that never went through detection.
	Signals []FiredSignal
}

// VideoName is the per-recording directory segment a clip is rendered under,
// derived from the master file's own name (gow_video05.webm -> "gow_video05").
//
// Clip names are only unique within a run — --include windows restart at
// clip_01 every time, and candidate IDs restart whenever the job store is
// rebuilt — so keying output on the game alone let a second recording's clip_01
// overwrite the first's. Keying on the recording as well makes the output path
// a function of something that actually identifies the footage.
//
// The name is reduced to characters that are safe in a path on both WSL and
// Windows: capture tools produce filenames like "Mortal Kombat™ 1_2026.webm",
// and a directory named after that verbatim is awkward at best and unopenable
// at worst. A name that survives none of that falls back to "unknown", matching
// how main derives an unknown game.
func VideoName(originalFile string) string {
	base := filepath.Base(originalFile)
	// filepath here is the POSIX implementation, which does not treat "\" as a
	// separator — but a Windows-style path reaching this binary through WSL
	// still has them, and the whole path would otherwise become the directory.
	if i := strings.LastIndex(base, `\`); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimSuffix(base, filepath.Ext(base))

	var b strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	// Collapsing runs and trimming edges keeps "Mortal Kombat™ 1" from becoming
	// "Mortal_Kombat__1" and a leading dot from producing a hidden directory.
	name := strings.Trim(collapseUnderscores(b.String()), "_")
	if name == "" {
		return "unknown"
	}
	return name
}

// collapseUnderscores squeezes runs of "_" down to one.
func collapseUnderscores(s string) string {
	var b strings.Builder
	prev := false
	for _, r := range s {
		if r == '_' && prev {
			continue
		}
		prev = r == '_'
		b.WriteRune(r)
	}
	return b.String()
}

// OutDir is the directory every clip cut from originalFile lands in. Exported
// so callers can name the path in their own console output without rebuilding
// the layout by hand and drifting from it.
func OutDir(gameName, originalFile string) string {
	return filepath.Join("out", gameName, VideoName(originalFile))
}

// CutOne renders the formats opts asks for into
// out/<gameName>/<VideoName(originalFile)>/<name>/. name is caller-chosen (e.g.
// a candidate ID) so clips can be rendered one at a time, as soon as they're
// approved, without needing to know the final size or order of the whole batch.
//
// Both outputs are cut independently from originalFile. short.mp4 used to be
// re-encoded out of clip.mp4, which cost a second generation of loss and made
// "vertical only" impossible without also paying for the master — sourcing each
// from the original fixes both.
func CutOne(originalFile, gameName, name string, c Clip, opts RenderOpts) error {
	if len(opts.Formats) == 0 {
		return fmt.Errorf("no output formats selected for %s", name)
	}

	clipDir := filepath.Join(OutDir(gameName, originalFile), name)
	if err := os.MkdirAll(clipDir, 0755); err != nil {
		return fmt.Errorf("failed to create output dir: %v", err)
	}

	clipFile := filepath.Join(clipDir, "clip.mp4")
	shortFile := filepath.Join(clipDir, "short.mp4")
	reelFile := filepath.Join(clipDir, "reel.mp4")
	thumbFile := filepath.Join(clipDir, "thumb.png")
	metaFile := filepath.Join(clipDir, "metadata.json")

	if c.Metadata != nil {
		b, _ := json.MarshalIndent(c.Metadata, "", "  ")
		os.WriteFile(metaFile, b, 0644)
	}

	if opts.wants(FormatClip) {
		if err := cutOriginal(originalFile, clipFile, c.Start, c.End, opts); err != nil {
			return fmt.Errorf("cutting: %w", err)
		}
	}

	if opts.wants(FormatShort) {
		if err := renderShort(originalFile, shortFile, c.Start, c.End, opts); err != nil {
			return fmt.Errorf("reformatting: %w", err)
		}
	}

	if opts.wants(FormatReel) {
		layout, err := renderReel(originalFile, reelFile, c, opts)
		if err != nil {
			return fmt.Errorf("reframing: %w", err)
		}
		// Persisted so a published reel can later be joined back to the exact
		// geometry that produced it. Without this, comparing a blur-pad post
		// against a full-bleed post tells you which file won but not which
		// framing decision did.
		if b, err := json.MarshalIndent(layout, "", "  "); err == nil {
			os.WriteFile(filepath.Join(clipDir, "reel_layout.json"), b, 0644)
		}
	}

	if c.Metadata != nil {
		thumbTs := c.Start + float64(c.Metadata.ThumbnailFrameIndex)
		if thumbTs > c.End {
			thumbTs = c.Start // fallback
		}
		if err := generateThumbnail(originalFile, thumbFile, c.Metadata.Title, thumbTs); err != nil {
			fmt.Printf("Warning: failed to generate thumbnail for %s: %v\n", name, err)
		}
	}

	return nil
}

// CutAndReformat processes a batch of clips sequentially (used for manually
// hand-picked --include windows, which bypass the review queue entirely and
// so have no per-candidate ID to name a directory after).
func CutAndReformat(originalFile, gameName string, clips []Clip, opts RenderOpts) error {
	for i, c := range clips {
		name := fmt.Sprintf("clip_%02d", i+1)
		fmt.Printf("Processing %s (%.2f - %.2f)...\n", name, c.Start, c.End)
		if err := CutOne(originalFile, gameName, name, c, opts); err != nil {
			fmt.Printf("Error processing %s: %v\n", name, err)
			continue
		}
		fmt.Printf("Successfully generated %s\n", filepath.Join(OutDir(gameName, originalFile), name))
	}
	return nil
}

func generateThumbnail(inputFile, outputFile, title string, ts float64) error {
	// ffmpeg -ss <thumb_ts> -i original.mp4 -frames:v 1 thumb_raw.png
	// Then overlay text. For simplicity, we'll do it in one command.
	filter := fmt.Sprintf(`drawtext=text='%s':fontsize=72:fontcolor=white:borderw=4:bordercolor=black:x=(w-text_w)/2:y=h-220`, strings.ReplaceAll(title, "'", "\\'"))
	cmd := exec.Command("ffmpeg", "-y", "-ss", fmt.Sprintf("%.3f", ts), "-i", inputFile, "-frames:v", "1", "-vf", filter, outputFile)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("thumbnail failed: %v, stderr: %s", err, stderr.String())
	}
	return nil
}

// encodeThreads caps x264's worker count. Left to itself x264 picks 1.5x the
// core count (36 threads on a 24-core box) and every frame thread holds its own
// copy of the frame plus its references — at 4K that reached ~7GB RSS and swapped
// the WSL VM, turning a 4-minute cut into 18. 8 is enough to saturate the encode
// without the footprint.
const encodeThreads = "8"

func cutOriginal(inputFile, outputFile string, start, end float64, opts RenderOpts) error {
	duration := end - start
	// -pix_fmt yuv420p: 10-bit sources (VP9 Profile 2) otherwise push x264 into
	// the High 10 profile, which most players and hardware decoders reject.
	cmd := exec.Command("ffmpeg", "-y",
		"-ss", fmt.Sprintf("%.3f", start), "-i", inputFile, "-t", fmt.Sprintf("%.3f", duration),
		"-c:v", "libx264", "-preset", opts.preset(), "-crf", opts.crf(),
		"-pix_fmt", "yuv420p", "-threads", encodeThreads,
		"-c:a", "aac", outputFile)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("cut failed: %v, stderr: %s", err, stderr.String())
	}
	return nil
}

// renderShort cuts the window and lays it out 9:16 in one pass: the source
// blown up and blurred as a background, the source scaled to canvas width as
// the foreground strip.
func renderShort(inputFile, outputFile string, start, end float64, opts RenderOpts) error {
	duration := end - start

	srcW, srcH, err := probeDimensions(inputFile)
	if err != nil {
		return err
	}
	w, h := shortCanvas(srcW, srcH, opts.ShortHeight)

	// The blur is a fraction of the frame, not a fixed pixel count: the
	// original radius of 20 was tuned against a 1080-wide canvas and reads as
	// nearly sharp once the canvas is 2160 wide.
	blur := 20 * w / 1080
	if blur < 2 {
		blur = 2
	}

	filter := fmt.Sprintf(
		"[0:v]scale=%d:%d:force_original_aspect_ratio=increase,crop=%d:%d,boxblur=%d:5[bg];"+
			"[0:v]scale=%d:-2[fg];"+
			"[bg][fg]overlay=(W-w)/2:(H-h)/2[vout]",
		w, h, w, h, blur, w)

	// -filter_complex suppresses ffmpeg's automatic stream selection, so the
	// audio has to be mapped by hand. 0:a? keeps a silent source from failing
	// outright. Audio is re-encoded rather than copied because the master is
	// typically Opus-in-WebM, which mp4 won't take.
	cmd := exec.Command("ffmpeg", "-y",
		"-ss", fmt.Sprintf("%.3f", start), "-i", inputFile, "-t", fmt.Sprintf("%.3f", duration),
		"-filter_complex", filter, "-map", "[vout]", "-map", "0:a?",
		"-c:v", "libx264", "-preset", opts.preset(), "-crf", opts.crf(),
		"-pix_fmt", "yuv420p", "-threads", encodeThreads,
		"-movflags", "+faststart", // this is an upload artifact too
		"-c:a", "aac", outputFile)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("reformat failed: %v, stderr: %s", err, stderr.String())
	}
	return nil
}

// shortCanvas picks the 9:16 canvas for a source of the given dimensions.
//
// The canvas is as wide as the source's short side, so the gameplay strip laid
// over the blurred background is downscaled as little as the aspect ratio
// allows: a 3840x2160 master yields a 2160x3840 canvas whose strip is 2160x1215,
// instead of the old hardcoded 1080x1920 whose strip was 1080x608 — a 3.6x
// downscale of 4K footage. Deriving from the short side (rather than the width)
// means a source that is already 1080p, or already vertical, lands back on a
// canvas it fits without ever being upscaled.
//
// override, when non-zero, pins the canvas height instead — for when the target
// platform's ingest limit matters more than retained detail.
func shortCanvas(srcW, srcH, override int) (w, h int) {
	if override > 0 {
		h = even(override)
		return even(h * 9 / 16), h
	}
	short := srcW
	if srcH < short {
		short = srcH
	}
	w = even(short)
	return w, even(w * 16 / 9)
}

// even rounds down to a multiple of 2: yuv420p subsamples chroma by two in each
// direction, so libx264 rejects an odd width or height outright.
func even(n int) int {
	if n < 2 {
		return 2
	}
	return n &^ 1
}

// probeDimensions reads the first video stream's pixel dimensions. It selects
// v:0 explicitly rather than trusting stream ordering, since a container whose
// audio track is listed first would otherwise report a 0x0 "video".
func probeDimensions(inputFile string) (int, int, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=width,height", "-of", "csv=p=0:s=x", inputFile)

	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return 0, 0, fmt.Errorf("probe %s for short canvas size: %w", inputFile, err)
	}

	fields := strings.Split(strings.TrimSpace(out.String()), "x")
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("probe %s: unexpected dimensions %q", inputFile, out.String())
	}
	w, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, 0, fmt.Errorf("probe %s: bad width %q: %w", inputFile, fields[0], err)
	}
	h, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, fmt.Errorf("probe %s: bad height %q: %w", inputFile, fields[1], err)
	}
	if w <= 0 || h <= 0 {
		return 0, 0, fmt.Errorf("probe %s: non-positive dimensions %dx%d", inputFile, w, h)
	}
	return w, h, nil
}
