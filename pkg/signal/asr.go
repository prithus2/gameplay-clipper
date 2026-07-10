package signal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gameplay-clipper/pkg/profile"
)

// ASRDetector transcribes match audio via a whisper.cpp binary and fires an
// event wherever a transcript segment contains one of a signal's target
// keywords — e.g. an announcer callout ("Ace", "Clutch") or a character
// voice line ("Fatality"), independent of whatever's on screen.
//
// NOTE: the exact CLI flags below (-oj/-of/-nt/-m/-f) match whisper.cpp's
// `main`/`whisper-cli` executable as of recent released versions, but
// whisper.cpp's CLI has changed shape across versions and this couldn't be
// verified end-to-end in this environment (no whisper.cpp install here).
// Verify against your installed build's `--help` before relying on it, and
// adjust extractSegments/the exec.Command args if the JSON schema differs.
type ASRDetector struct{}

func (ASRDetector) Name() string { return "asr" }

func (ASRDetector) Detect(proxyFile string, prof *profile.Profile) ([]Event, error) {
	hasASRSignals := false
	for _, sig := range prof.Signals {
		if sig.Kind == profile.KindASRKeyword {
			hasASRSignals = true
			break
		}
	}
	if !hasASRSignals {
		return nil, nil
	}

	bin, binOK := whisperPath()
	model, modelOK := whisperModelPath()
	if !binOK {
		log.Printf("Warning: no whisper.cpp binary found on PATH (tried whisper-cli, whisper-cpp, main), skipping ASR-based signals for %s", prof.Game)
		return nil, nil
	}
	if !modelOK {
		log.Printf("Warning: WHISPER_MODEL_PATH not set (or file missing), skipping ASR-based signals for %s", prof.Game)
		return nil, nil
	}

	segments, err := transcribeWithWhisper(bin, model, proxyFile)
	if err != nil {
		return nil, fmt.Errorf("asr transcribe: %w", err)
	}

	var events []Event
	for _, sig := range prof.Signals {
		if sig.Kind != profile.KindASRKeyword {
			continue
		}
		events = append(events, matchASRKeywordEvents(sig, segments)...)
	}
	return events, nil
}

type asrSegment struct {
	Start float64
	End   float64
	Text  string
}

// matchASRKeywordEvents is pure: one event per transcript segment whose
// text matches one of the signal's keywords.
func matchASRKeywordEvents(sig profile.Signal, segments []asrSegment) []Event {
	var events []Event
	for _, s := range segments {
		if containsAnyFold(s.Text, sig.Match) {
			events = append(events, Event{
				Kind: sig.Name, T: s.Start, Confidence: 0.9,
				Payload: s.Text, Source: "asr",
			})
		}
	}
	return events
}

var (
	whisperBinOnce   sync.Once
	whisperBinPath   string
	whisperBinFound  bool
)

// whisperPath looks up a whisper.cpp CLI binary once per process, trying
// the names used across different whisper.cpp releases.
func whisperPath() (string, bool) {
	whisperBinOnce.Do(func() {
		for _, name := range []string{"whisper-cli", "whisper-cpp", "main"} {
			if p, err := exec.LookPath(name); err == nil {
				whisperBinPath, whisperBinFound = p, true
				return
			}
		}
	})
	return whisperBinPath, whisperBinFound
}

func whisperModelPath() (string, bool) {
	p := os.Getenv("WHISPER_MODEL_PATH")
	if p == "" {
		return "", false
	}
	if _, err := os.Stat(p); err != nil {
		return "", false
	}
	return p, true
}

func transcribeWithWhisper(bin, model, proxyFile string) ([]asrSegment, error) {
	wavFile, err := extractWav16kMono(proxyFile)
	if err != nil {
		return nil, err
	}
	defer os.Remove(wavFile)

	outBase := strings.TrimSuffix(wavFile, filepath.Ext(wavFile))
	cmd := exec.Command(bin, "-m", model, "-f", wavFile, "-oj", "-of", outBase, "-nt")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("whisper.cpp failed: %v, stderr: %s", err, stderr.String())
	}

	jsonPath := outBase + ".json"
	defer os.Remove(jsonPath)
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		return nil, fmt.Errorf("read whisper.cpp json output: %w", err)
	}
	return parseWhisperJSON(data)
}

func extractWav16kMono(proxyFile string) (string, error) {
	f, err := os.CreateTemp("", "asr-*.wav")
	if err != nil {
		return "", err
	}
	f.Close()
	cmd := exec.Command("ffmpeg", "-y", "-i", proxyFile, "-ar", "16000", "-ac", "1", "-c:a", "pcm_s16le", f.Name())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("ffmpeg audio extract failed: %v, stderr: %s", err, stderr.String())
	}
	return f.Name(), nil
}

type whisperJSON struct {
	Transcription []struct {
		Offsets struct {
			From int64 `json:"from"`
			To   int64 `json:"to"`
		} `json:"offsets"`
		Text string `json:"text"`
	} `json:"transcription"`
}

// parseWhisperJSON is pure: given whisper.cpp's -oj JSON output, return
// segments with second-resolution timestamps.
func parseWhisperJSON(data []byte) ([]asrSegment, error) {
	var w whisperJSON
	if err := json.Unmarshal(data, &w); err != nil {
		return nil, fmt.Errorf("parse whisper.cpp json: %w", err)
	}
	segs := make([]asrSegment, 0, len(w.Transcription))
	for _, t := range w.Transcription {
		segs = append(segs, asrSegment{
			Start: float64(t.Offsets.From) / 1000.0,
			End:   float64(t.Offsets.To) / 1000.0,
			Text:  strings.TrimSpace(t.Text),
		})
	}
	return segs, nil
}
