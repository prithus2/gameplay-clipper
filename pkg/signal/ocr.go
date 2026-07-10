package signal

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/gameplay-clipper/pkg/profile"
)

var (
	tesseractOnce sync.Once
	tesseractPathCache string
	tesseractFound     bool
)

// tesseractPath looks up the tesseract binary once per process. Detectors
// that depend on it must check the bool and skip (with a warning) rather
// than error, per this project's "degrade gracefully, don't silently keep
// everything or crash" rule for optional external tools.
func tesseractPath() (string, bool) {
	tesseractOnce.Do(func() {
		if p, err := exec.LookPath("tesseract"); err == nil {
			tesseractPathCache, tesseractFound = p, true
		}
	})
	return tesseractPathCache, tesseractFound
}

// ocrSample is one recognized-text reading at a point in the video.
type ocrSample struct {
	T    float64
	Text string
}

type videoDims struct{ W, H int }

func probeDims(proxyFile string) (videoDims, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=width,height", "-of", "json", proxyFile)
	out, err := cmd.Output()
	if err != nil {
		return videoDims{}, fmt.Errorf("ffprobe dims: %w", err)
	}
	var parsed struct {
		Streams []struct{ Width, Height int } `json:"streams"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil || len(parsed.Streams) == 0 {
		return videoDims{}, fmt.Errorf("ffprobe dims: unexpected output %s", out)
	}
	return videoDims{W: parsed.Streams[0].Width, H: parsed.Streams[0].Height}, nil
}

// sampleRegionText crops a fractional-coordinate region out of the proxy
// video, samples it at fps, and OCRs each sampled frame. Returns one
// ocrSample per extracted frame, in time order. Requires tesseract on PATH
// (callers must check tesseractPath() first).
func sampleRegionText(proxyFile string, region profile.Region, fps float64) ([]ocrSample, error) {
	if fps <= 0 {
		fps = 2
	}
	dims, err := probeDims(proxyFile)
	if err != nil {
		return nil, err
	}
	x := int(region.X * float64(dims.W))
	y := int(region.Y * float64(dims.H))
	w := int(region.W * float64(dims.W))
	h := int(region.H * float64(dims.H))
	// crop dims must be even for most codecs' chroma subsampling.
	if w%2 != 0 {
		w++
	}
	if h%2 != 0 {
		h++
	}

	framesDir, err := os.MkdirTemp("", "ocr-frames-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(framesDir)

	filter := fmt.Sprintf("fps=%g,crop=%d:%d:%d:%d", fps, w, h, x, y)
	pattern := filepath.Join(framesDir, "f_%06d.png")
	cmd := exec.Command("ffmpeg", "-y", "-i", proxyFile, "-vf", filter, pattern)
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg region crop failed: %w", err)
	}

	files, err := os.ReadDir(framesDir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(files))
	for _, f := range files {
		names = append(names, f.Name())
	}
	sort.Strings(names)

	tpath, _ := tesseractPath()
	samples := make([]ocrSample, 0, len(names))
	for i, name := range names {
		t := float64(i) / fps
		text, err := runTesseract(tpath, filepath.Join(framesDir, name))
		if err != nil {
			continue // one bad frame shouldn't sink the whole signal
		}
		samples = append(samples, ocrSample{T: t, Text: text})
	}
	return samples, nil
}

func runTesseract(tesseractBin, imgPath string) (string, error) {
	cmd := exec.Command(tesseractBin, imgPath, "stdout")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
