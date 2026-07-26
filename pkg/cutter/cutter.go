package cutter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gameplay-clipper/pkg/llm"
)

// Clip is one approved window ready to be cut, reformatted, and thumbnailed.
// Metadata is nil only for the (rare) case a clip was approved without any
// LLM metadata ever being generated for it.
type Clip struct {
	Start, End float64
	Metadata   *llm.ClipMetadata
}

// CutOne cuts, reformats to 9:16, and thumbnails a single clip into
// out/<gameName>/<name>/. name is caller-chosen (e.g. a candidate ID) so
// clips can be rendered one at a time, as soon as they're approved, without
// needing to know the final size or order of the whole batch.
func CutOne(originalFile, gameName, name string, c Clip) error {
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

	// 1. Cut from original
	if err := cutOriginal(originalFile, clipFile, c.Start, c.End); err != nil {
		return fmt.Errorf("cutting: %w", err)
	}

	// 2. Reformat to 9:16
	if err := reformatTo916(clipFile, shortFile); err != nil {
		return fmt.Errorf("reformatting: %w", err)
	}

	// 3. Thumbnail (Phase 4)
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
func CutAndReformat(originalFile, gameName string, clips []Clip) error {
	for i, c := range clips {
		name := fmt.Sprintf("clip_%02d", i+1)
		fmt.Printf("Processing %s (%.2f - %.2f)...\n", name, c.Start, c.End)
		if err := CutOne(originalFile, gameName, name, c); err != nil {
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

func cutOriginal(inputFile, outputFile string, start, end float64) error {
	duration := end - start
	// -pix_fmt yuv420p: 10-bit sources (VP9 Profile 2) otherwise push x264 into
	// the High 10 profile, which most players and hardware decoders reject.
	cmd := exec.Command("ffmpeg", "-y", "-ss", fmt.Sprintf("%.3f", start), "-i", inputFile, "-t", fmt.Sprintf("%.3f", duration), "-c:v", "libx264", "-preset", "medium", "-pix_fmt", "yuv420p", "-threads", encodeThreads, "-c:a", "aac", outputFile)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("cut failed: %v, stderr: %s", err, stderr.String())
	}
	return nil
}

func reformatTo916(inputFile, outputFile string) error {
	filter := "[0:v]scale=1080:1920:force_original_aspect_ratio=increase,crop=1080:1920,boxblur=20:5[bg];[0:v]scale=1080:-2[fg];[bg][fg]overlay=(W-w)/2:(H-h)/2"

	cmd := exec.Command("ffmpeg", "-y", "-i", inputFile, "-filter_complex", filter, "-pix_fmt", "yuv420p", "-threads", encodeThreads, "-c:a", "copy", outputFile)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("reformat failed: %v, stderr: %s", err, stderr.String())
	}
	return nil
}
