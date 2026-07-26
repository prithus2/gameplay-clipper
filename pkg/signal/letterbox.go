package signal

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/gameplay-clipper/pkg/profile"
)

// LetterboxDetector finds cinematic cutscenes by their letterbox bars: the
// symmetric top+bottom black bars a game switches to for a cinematic. It's a
// full-frame, OCR-free, resolution-independent heuristic (ffmpeg cropdetect),
// which is why it's a good fit for God of War — there's no killfeed or banner
// to OCR, but its story cinematics letterbox. See profiles/gow.yaml.
//
// Caveat (honest): this fires on *any* letterboxing, so a game/scene that
// letterboxes outside cutscenes would false-positive, and a cutscene that
// runs full-frame (e.g. much of God of War 2018's seamless one-shot dialogue)
// is invisible to it. It's deliberately a recall-oriented signal weighted
// high and left for the LLM judge + human review to filter, not an
// auto-cut — see [[gameplay-clipper-v2-implementation]].
type LetterboxDetector struct{}

func (LetterboxDetector) Name() string { return "letterbox" }

// Tuning. Bars must cover at least minBarFrac of frame height on *both* edges
// and be roughly symmetric (a one-sided dark HUD panel is not a cutscene).
const (
	letterboxMinBarFrac = 0.06 // each bar >= 6% of frame height
	letterboxSymTolFrac = 0.03 // |top-bottom| bar diff must be < 3% of height
	letterboxMinDurS    = 3.0  // ignore sub-3s letterboxing (fade transitions)
	letterboxMaxGapS    = 2.0  // bridge gaps <= 2s within one cutscene
	letterboxEventStepS = 4.0  // emit a fired event every 4s across a cutscene
	letterboxDefaultFPS = 2.0  // sampling rate if the signal omits sample_fps
)

func (LetterboxDetector) Detect(proxyFile string, prof *profile.Profile) ([]Event, error) {
	sig := letterboxSignal(prof)
	if sig == nil {
		return nil, nil // this game's profile declares no letterbox signal
	}

	fps := sig.SampleFPS
	if fps <= 0 {
		fps = letterboxDefaultFPS
	}

	height, err := probeHeight(proxyFile)
	if err != nil {
		return nil, fmt.Errorf("letterbox: probe height: %w", err)
	}

	log, err := runCropdetect(proxyFile, fps)
	if err != nil {
		return nil, fmt.Errorf("letterbox: cropdetect: %w", err)
	}

	samples := ParseCropdetect(log, height)
	segments := GroupCutscenes(samples, letterboxMinDurS, letterboxMaxGapS)

	var events []Event
	for _, seg := range segments {
		for _, t := range emitTimes(seg, letterboxEventStepS) {
			events = append(events, Event{
				Kind:       sig.Name,
				T:          t,
				Confidence: 0.8,
				Payload:    fmt.Sprintf("cutscene %.1fs-%.1fs", seg.Start, seg.End),
				Source:     "letterbox",
			})
		}
	}
	return events, nil
}

// letterboxSignal returns the first letterbox-kind signal in the profile, or
// nil if the game declares none.
func letterboxSignal(prof *profile.Profile) *profile.Signal {
	for i := range prof.Signals {
		if prof.Signals[i].Kind == profile.KindLetterbox {
			return &prof.Signals[i]
		}
	}
	return nil
}

// runCropdetect samples the proxy at the given fps and returns ffmpeg's
// cropdetect log (stderr), which carries one "[Parsed_cropdetect ...]" line
// per sampled frame. reset=1 recomputes the crop box every frame instead of
// accumulating, so each line reflects that instant's bars.
func runCropdetect(proxyFile string, fps float64) (string, error) {
	vf := fmt.Sprintf("fps=%g,cropdetect=limit=24:round=2:reset=1", fps)
	cmd := exec.Command("ffmpeg", "-y", "-i", proxyFile, "-vf", vf, "-an", "-f", "null", "-")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ffmpeg cropdetect failed: %v", err)
	}
	return stderr.String(), nil
}

// probeHeight returns the proxy's frame height in pixels — cropdetect reports
// the content box (y1/y2) but not the full frame, so bar sizes are measured
// against this.
func probeHeight(proxyFile string) (int, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=height", "-of", "json", proxyFile)
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("ffprobe failed: %v", err)
	}
	var probe struct {
		Streams []struct {
			Height int `json:"height"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		return 0, fmt.Errorf("parse ffprobe output: %v", err)
	}
	if len(probe.Streams) == 0 || probe.Streams[0].Height <= 0 {
		return 0, fmt.Errorf("ffprobe returned no usable height for %s", proxyFile)
	}
	return probe.Streams[0].Height, nil
}

// PresenceSample is one sampled frame's yes/no verdict for some condition
// (letterbox bars present, caption text present). Shared by the letterbox and
// subtitle detectors, which both turn a per-frame presence signal into
// cutscene segments via GroupCutscenes.
type PresenceSample struct {
	T      float64
	Active bool
}

// Segment is a contiguous cutscene window [Start, End] in seconds.
type Segment struct {
	Start float64
	End   float64
}

var cropLineRe = regexp.MustCompile(`\bt:([\d.]+)\b`)
var cropY1Re = regexp.MustCompile(`\by1:(\d+)\b`)
var cropY2Re = regexp.MustCompile(`\by2:(\d+)\b`)

// ParseCropdetect is pure: given ffmpeg's cropdetect stderr and the frame
// height, it returns one sample per "[Parsed_cropdetect ...]" line marking
// whether that frame is letterboxed. A frame is letterboxed when both the top
// bar (y1) and bottom bar (height-1-y2) exceed minBarFrac of the height and
// are roughly symmetric — a one-sided dark region (e.g. a HUD panel) is not.
// Exported so tests can feed fixture text without running ffmpeg.
func ParseCropdetect(output string, frameHeight int) []PresenceSample {
	var samples []PresenceSample
	if frameHeight <= 0 {
		return samples
	}
	minBar := letterboxMinBarFrac * float64(frameHeight)
	symTol := letterboxSymTolFrac * float64(frameHeight)

	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.Contains(line, "Parsed_cropdetect") {
			continue
		}
		tm := cropLineRe.FindStringSubmatch(line)
		y1m := cropY1Re.FindStringSubmatch(line)
		y2m := cropY2Re.FindStringSubmatch(line)
		if len(tm) != 2 || len(y1m) != 2 || len(y2m) != 2 {
			continue
		}
		t, _ := strconv.ParseFloat(tm[1], 64)
		y1, _ := strconv.Atoi(y1m[1])
		y2, _ := strconv.Atoi(y2m[1])

		topBar := float64(y1)
		bottomBar := float64(frameHeight - 1 - y2)
		letterboxed := topBar >= minBar && bottomBar >= minBar &&
			absF(topBar-bottomBar) <= symTol
		samples = append(samples, PresenceSample{T: t, Active: letterboxed})
	}
	return samples
}

// GroupCutscenes is pure: it collapses runs of letterboxed samples into
// cutscene segments. Consecutive letterboxed samples separated by <= maxGapS
// stay in one segment (bridging brief detection dropouts); segments shorter
// than minDurS are discarded as transient fades rather than real cutscenes.
func GroupCutscenes(samples []PresenceSample, minDurS, maxGapS float64) []Segment {
	sort.Slice(samples, func(i, j int) bool { return samples[i].T < samples[j].T })

	var segments []Segment
	inSeg := false
	var start, last float64
	for _, s := range samples {
		if !s.Active {
			continue
		}
		switch {
		case !inSeg:
			inSeg, start, last = true, s.T, s.T
		case s.T-last <= maxGapS:
			last = s.T
		default:
			if last-start >= minDurS {
				segments = append(segments, Segment{Start: start, End: last})
			}
			start, last = s.T, s.T
		}
	}
	if inSeg && last-start >= minDurS {
		segments = append(segments, Segment{Start: start, End: last})
	}
	return segments
}

// emitTimes returns the timestamps at which to fire an event for a cutscene:
// the start, the end, and every stepS in between. The scoring engine merges
// these (via the cutscene archetype's streak_merge_gap_s) into one window
// spanning the whole cutscene, so intermediate events are what let a long
// cutscene stay a single candidate.
func emitTimes(seg Segment, stepS float64) []float64 {
	times := []float64{seg.Start}
	if stepS > 0 {
		for t := seg.Start + stepS; t < seg.End; t += stepS {
			times = append(times, t)
		}
	}
	if seg.End > seg.Start {
		times = append(times, seg.End)
	}
	return times
}

func absF(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
