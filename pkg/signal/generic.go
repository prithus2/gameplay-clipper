package signal

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/gameplay-clipper/pkg/profile"
)

// GenericDetector is the zero-config fallback: loudness spikes (ebur128)
// and scene changes. It runs for every game regardless of profile, since
// per-game signals only ever add to this, never replace it (see
// profile.ensureGenericFallback). This is the same heuristic the pipeline
// used before per-game profiles existed — kept as a baseline, not because
// it's reliable on its own (the Valorant diagnostic showed 0 loudness
// spikes and scene-cuts that were just scoreboard toggles for that game).
type GenericDetector struct{}

func (GenericDetector) Name() string { return "generic" }

func (GenericDetector) Detect(proxyFile string, prof *profile.Profile) ([]Event, error) {
	var events []Event

	loudness, err := ExtractLoudnessSpikes(proxyFile)
	if err != nil {
		return nil, fmt.Errorf("generic loudness detection: %w", err)
	}
	for _, t := range loudness {
		events = append(events, Event{Kind: "loudness_spike", T: t, Confidence: 1.0, Source: "generic"})
	}

	scenes, err := ExtractSceneChanges(proxyFile)
	if err != nil {
		return nil, fmt.Errorf("generic scene detection: %w", err)
	}
	for _, t := range scenes {
		events = append(events, Event{Kind: "scene_cut", T: t, Confidence: 1.0, Source: "generic"})
	}

	return events, nil
}

// ExtractLoudnessSpikes runs ffmpeg's ebur128 filter and returns timestamps
// where momentary loudness exceeds the session mean by 8 LUFS.
func ExtractLoudnessSpikes(proxyFile string) ([]float64, error) {
	cmd := exec.Command("ffmpeg", "-y", "-i", proxyFile, "-filter_complex", "ebur128=metadata=1", "-f", "null", "-")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg ebur128 failed: %v", err)
	}
	return ParseEbur128(stderr.String()), nil
}

// ExtractSceneChanges runs ffmpeg's scene-change filter and returns
// timestamps of detected cuts.
func ExtractSceneChanges(proxyFile string) ([]float64, error) {
	sceneFile, err := os.CreateTemp("", "scenes-*.txt")
	if err != nil {
		return nil, err
	}
	sceneFile.Close()
	defer os.Remove(sceneFile.Name())

	cmd := exec.Command("ffmpeg", "-y", "-i", proxyFile, "-vf", "select='gt(scene,0.3)',metadata=print:file="+sceneFile.Name(), "-an", "-f", "null", "-")
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg scene detect failed: %v", err)
	}

	content, err := os.ReadFile(sceneFile.Name())
	if err != nil {
		if os.IsNotExist(err) {
			return []float64{}, nil
		}
		return nil, err
	}
	return ParseScenes(string(content)), nil
}

var ebur128Re = regexp.MustCompile(`t:\s+([\d\.]+)\s+M:\s+([\-\d\.]+)`)

// ParseEbur128 is pure: given raw ebur128 stderr text, return spike
// timestamps. Exported so tests can feed it fixture text without running
// ffmpeg.
func ParseEbur128(output string) []float64 {
	var spikes []float64
	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	type entry struct{ t, m float64 }
	var entries []entry

	for scanner.Scan() {
		matches := ebur128Re.FindStringSubmatch(scanner.Text())
		if len(matches) == 3 {
			t, _ := strconv.ParseFloat(matches[1], 64)
			m, _ := strconv.ParseFloat(matches[2], 64)
			if m < -70 {
				m = -70
			}
			entries = append(entries, entry{t, m})
		}
	}
	if len(entries) == 0 {
		return spikes
	}

	sum := 0.0
	for _, e := range entries {
		sum += e.m
	}
	avg := sum / float64(len(entries))

	for _, e := range entries {
		if e.m > avg+8.0 {
			spikes = append(spikes, e.t)
		}
	}
	return spikes
}

var sceneRe = regexp.MustCompile(`pts_time:([\d\.]+)`)

// ParseScenes is pure: given raw ffmpeg metadata:print text, return
// scene-cut timestamps.
func ParseScenes(output string) []float64 {
	var scenes []float64
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		matches := sceneRe.FindStringSubmatch(scanner.Text())
		if len(matches) == 2 {
			t, _ := strconv.ParseFloat(matches[1], 64)
			scenes = append(scenes, t)
		}
	}
	return scenes
}
