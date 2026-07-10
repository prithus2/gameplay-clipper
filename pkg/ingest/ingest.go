package ingest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type VideoMetadata struct {
	Streams []struct {
		CodecName  string `json:"codec_name"`
		Width      int    `json:"width"`
		Height     int    `json:"height"`
		RFrameRate string `json:"r_frame_rate"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

// ProbeVideo returns metadata for the input video using ffprobe.
func ProbeVideo(inputFile string) (*VideoMetadata, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-show_entries",
		"format=duration:stream=codec_name,width,height,r_frame_rate",
		"-of", "json", inputFile)

	var out bytes.Buffer
	cmd.Stdout = &out

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffprobe failed: %v", err)
	}

	var meta VideoMetadata
	if err := json.Unmarshal(out.Bytes(), &meta); err != nil {
		return nil, fmt.Errorf("failed to parse ffprobe output: %v", err)
	}

	return &meta, nil
}

// NormalizeToProxy creates a 720p 30fps proxy of the input video for faster analysis.
// If a proxy already exists from a previous run, it is reused.
func NormalizeToProxy(inputFile string) (string, error) {
	// e.g., video.mp4 -> video_proxy.mp4
	ext := filepath.Ext(inputFile)
	base := strings.TrimSuffix(inputFile, ext)
	proxyFile := base + "_proxy.mp4"

	// Skip if proxy already exists and is non-empty
	if info, err := os.Stat(proxyFile); err == nil && info.Size() > 0 {
		fmt.Printf("Proxy already exists (%s, %.1f MB). Skipping Phase 0.\n", proxyFile, float64(info.Size())/(1024*1024))
		return proxyFile, nil
	}

	// ffmpeg -i input.mp4 -vf scale=-2:720 -r 30 -c:v libx264 -preset veryfast -c:a aac proxy.mp4
	cmd := exec.Command("ffmpeg", "-y", "-i", inputFile, "-vf", "scale=-2:720", "-r", "30", "-c:v", "libx264", "-preset", "veryfast", "-c:a", "aac", proxyFile)

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ffmpeg proxy failed: %v", err)
	}

	return proxyFile, nil
}
