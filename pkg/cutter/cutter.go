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
	// FormatShort is the 9:16 vertical render for Reels/Shorts.
	FormatShort Format = "short"
)

// AllFormats is the fallback set for callers that never present a choice
// (--include windows, which bypass the review UI entirely).
var AllFormats = []Format{FormatClip, FormatShort}

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
		if f != FormatClip && f != FormatShort {
			return nil, fmt.Errorf("unknown format %q (want %q or %q)", n, FormatClip, FormatShort)
		}
		if seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no output formats selected (want at least one of %q, %q)", FormatClip, FormatShort)
	}
	return out, nil
}

// RenderOpts controls which outputs get written and how they're encoded.
// A zero CRF/Preset falls back to the package defaults, so callers that only
// care about Formats can leave the rest alone.
type RenderOpts struct {
	Formats []Format
	CRF     int
	Preset  string
	// ShortHeight overrides the 9:16 canvas height. 0 derives it from the
	// source (see shortCanvas), which is what you almost always want.
	ShortHeight int
}

// DefaultRenderOpts renders both outputs at the package's default quality.
func DefaultRenderOpts() RenderOpts {
	return RenderOpts{Formats: AllFormats, CRF: DefaultCRF, Preset: DefaultPreset}
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
}

// CutOne renders the formats opts asks for into out/<gameName>/<name>/. name is
// caller-chosen (e.g. a candidate ID) so clips can be rendered one at a time, as
// soon as they're approved, without needing to know the final size or order of
// the whole batch.
//
// Both outputs are cut independently from originalFile. short.mp4 used to be
// re-encoded out of clip.mp4, which cost a second generation of loss and made
// "vertical only" impossible without also paying for the master — sourcing each
// from the original fixes both.
func CutOne(originalFile, gameName, name string, c Clip, opts RenderOpts) error {
	if len(opts.Formats) == 0 {
		return fmt.Errorf("no output formats selected for %s", name)
	}

	clipDir := filepath.Join("out", gameName, name)
	if err := os.MkdirAll(clipDir, 0755); err != nil {
		return fmt.Errorf("failed to create output dir: %v", err)
	}

	clipFile := filepath.Join(clipDir, "clip.mp4")
	shortFile := filepath.Join(clipDir, "short.mp4")
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
		fmt.Printf("Successfully generated out/%s/%s\n", gameName, name)
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
