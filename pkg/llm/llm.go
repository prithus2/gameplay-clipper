package llm

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gameplay-clipper/pkg/scoring"
)

type ClipMetadata struct {
	IsHighlight         bool    `json:"is_highlight"`
	Score               float64 `json:"score"`
	Type                string  `json:"type"`
	Title               string  `json:"title"`
	ThumbnailFrameIndex int     `json:"thumbnail_frame_index"`
	CropFocus           struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
	} `json:"crop_focus"`
	// CropSpan is the horizontal extent, in full-frame fractions, that has to
	// stay in shot for the clip to still read — both fighters in a two-shot,
	// not just the one the camera favours. The reel renderer widens its crop to
	// cover this, padding whatever the widened crop no longer fills, so a wide
	// two-shot degrades to a letterboxed strip instead of slicing a character
	// off at the frame edge. A zero/degenerate span means "no wider than 9:16
	// needs", which is the full-bleed crop and the right answer for most
	// single-subject action.
	CropSpan struct {
		X0 float64 `json:"x0"`
		X1 float64 `json:"x1"`
	} `json:"crop_span"`
	Reason string `json:"reason"`
}

// RankAndMetadata asks the LLM to make the final is_highlight/score/title
// call for one candidate window. The deterministic scoring engine
// (pkg/scoring) has already decided *which* window to look at and *why*
// (which signals fired, at what confidence) — that evidence is passed in
// as structured text alongside the sampled frames, so the model is judging
// from real detector evidence instead of guessing cold from a handful of
// images (the failure mode that scored a resting cutscene as a 0.6
// highlight). The LLM remains the final judge; this only gives it better
// inputs.
func RankAndMetadata(proxyFile, gameName string, cand scoring.Candidate, apiKey string) (*ClipMetadata, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("GEMINI_API_KEY is not set")
	}

	framesDir, err := os.MkdirTemp("", "frames-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(framesDir)

	duration := cand.End - cand.Start

	// Sample 1 frame every 3 seconds instead of every 1 second to reduce token usage
	fps := "1/3"
	if duration <= 10 {
		fps = "1" // For very short clips, keep 1fps
	}

	cmd := exec.Command("ffmpeg", "-y", "-ss", fmt.Sprintf("%.3f", cand.Start), "-t", fmt.Sprintf("%.3f", duration), "-i", proxyFile, "-vf", fmt.Sprintf("fps=%s", fps), "-q:v", "5", filepath.Join(framesDir, "window_%03d.jpg"))
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("failed to extract frames: %v", err)
	}

	files, err := os.ReadDir(framesDir)
	if err != nil {
		return nil, err
	}

	if len(files) == 0 {
		return nil, fmt.Errorf("no frames extracted")
	}

	var b64Frames []string
	totalBytes := 0
	maxFrames := 10 // Cap frames to limit token usage
	for i, f := range files {
		if i >= maxFrames {
			break
		}
		content, err := os.ReadFile(filepath.Join(framesDir, f.Name()))
		if err != nil {
			continue
		}
		totalBytes += len(content)
		b64Frames = append(b64Frames, base64.StdEncoding.EncodeToString(content))
	}

	// Estimate: ~258 tokens per image for Gemini vision
	estimatedTokens := len(b64Frames) * 258
	fmt.Printf("  → Sending %d frames (of %d extracted) | Payload: %.1f KB | Est. tokens: ~%d\n",
		len(b64Frames), len(files), float64(totalBytes)/1024.0, estimatedTokens)

	return callGemini(gameName, buildEvidenceText(cand), b64Frames, apiKey)
}

// buildEvidenceText renders the deterministic detector's findings as plain
// text for the prompt: which archetype it thinks this is, its computed
// score, and exactly which signals fired, when, and with what confidence.
func buildEvidenceText(cand scoring.Candidate) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Detector archetype guess: %s (deterministic score %.2f)\n", cand.Archetype, cand.Score)
	if len(cand.SignalsFired) == 0 {
		b.WriteString("Signals fired: none (either a manually-specified window, or generic loudness/scene-change detection only — both are unreliable on their own; weigh the visual frames more heavily than the archetype guess)\n")
		return b.String()
	}
	b.WriteString("Signals fired (from OCR/audio detectors, not just visual guessing):\n")
	for _, s := range cand.SignalsFired {
		fmt.Fprintf(&b, "  - %s at t=%.1fs (confidence %.2f, source=%s)", s.Kind, s.T, s.Confidence, s.Source)
		if s.Payload != "" {
			fmt.Fprintf(&b, ": %q", s.Payload)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func callGemini(gameName, evidence string, frames []string, apiKey string) (*ClipMetadata, error) {
	url := "https://generativelanguage.googleapis.com/v1beta/models/gemini-3.5-flash:generateContent?key=" + apiKey

	prompt := fmt.Sprintf(`You are an expert gaming content creator acting as the final judge on whether a candidate clip from %s is worth publishing.

A separate deterministic detector (OCR on HUD elements like kill feeds and banners, audio transcription, loudness/scene-change) already flagged this window and produced this evidence:

%s
This evidence can be wrong: OCR misreads text, a stray loudness spike can be a cutscene or music swell rather than gameplay action, and "signals fired: none" means this window has no real per-game signal behind it at all — it was kept purely by a generic heuristic. Your job is to look at the actual sampled frames below and decide if this is genuinely a shareable highlight, using the evidence as a hint, not a verdict. If the frames show idle/cutscene/menu content despite a signal firing, score it low and set is_highlight to false — do not defer to the detector.

Analyze the sampled frames (roughly one every few seconds across the candidate window).
Return ONLY valid JSON (no markdown) with this schema:
{
  "is_highlight": true,
  "score": 0.0,
  "type": "action",
  "title": "string",
  "thumbnail_frame_index": 0,
  "crop_focus": { "x": 0.5, "y": 0.5 },
  "crop_span": { "x0": 0.0, "x1": 1.0 },
  "reason": "string"
}

About crop_focus — this field is used, so answer it deliberately:

This clip will be published as a vertical 9:16 reel, cut out of the widescreen
frames you are looking at. A 9:16 window keeps only the middle ~32%% of a 16:9
frame's width, so roughly two thirds of what you see gets discarded. crop_focus
tells the renderer where to put that window.

  - x and y are fractions of the FULL FRAME: x=0 is the left edge, x=1 the
    right edge, y=0 the top, y=1 the bottom. x=0.5,y=0.5 is dead centre.
  - x is what matters. The crop keeps the full frame height, so y has little
    or no effect on a normal widescreen capture.
  - Choose the x where the ACTION is across the window as a whole — the player
    character, the duel, the finisher — not where it is in a single frame. One
    static crop has to serve the entire clip.
  - Do NOT answer 0.5 by default. Answer 0.5 only if the action genuinely sits
    in the middle of the frame for most of the clip. If the fight happens on
    the left third, say x=0.33 and mean it.
  - Ignore HUD elements when choosing x. Kill feeds, banners and subtitles are
    re-composited separately and do not need to be inside the crop.

About crop_span — this decides whether the reel is full-bleed or letterboxed,
so answer it from what you actually see, not from a default:

  - x0 and x1 are fractions of the FULL FRAME width and mark the narrowest
    horizontal band that still contains everything the clip needs to make
    sense: every character involved in the beat, the finisher, the objective.
  - This is NOT the same question as crop_focus. crop_focus asks where to look;
    crop_span asks how much has to fit. crop_focus should sit inside crop_span.
  - If everything that matters fits in the middle third of the frame, say so —
    e.g. { "x0": 0.35, "x1": 0.68 }. A span that width or narrower renders
    full-bleed with nothing wasted, which is the best-looking result and what
    most single-subject combat should get.
  - The two errors are NOT equally bad. Too wide costs some screen area to
    letterboxing — a shame, but watchable. Too narrow slices a character in
    half at the frame edge and ruins the clip. When you are unsure, widen.
  - Before answering, check the LEFT and RIGHT extremes of every frame, not
    just the middle. Over-the-shoulder shots and two-shots routinely put one
    character hard against an edge, sometimes only partly in frame — a
    foreground shoulder, an arm entering from the side, a body cut by the
    frame border. That character is part of the beat and MUST be inside the
    span. A punch landing is two people: the one throwing it counts even when
    the camera favours the one receiving it.
  - Judge it across the whole clip, not one frame. If the shot changes — a
    wide gameplay shot, then a cutscene two-shot, then a single — the span is
    the UNION of what matters in each, because one crop has to serve them all.
  - { "x0": 0, "x1": 1 } means "the entire frame is essential". That is the
    right answer for a wide two-shot with a character at each edge.`, gameName, evidence)

	var parts []map[string]interface{}
	parts = append(parts, map[string]interface{}{"text": prompt})

	for _, frame := range frames {
		parts = append(parts, map[string]interface{}{
			"inline_data": map[string]interface{}{
				"mime_type": "image/jpeg",
				"data":      frame,
			},
		})
	}

	reqBody := map[string]interface{}{
		"contents": []map[string]interface{}{
			{"parts": parts},
		},
	}

	bodyBytes, _ := json.Marshal(reqBody)

	client := &http.Client{}

	req, _ := http.NewRequest("POST", url, bytes.NewBuffer(bodyBytes))
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}

	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var resData struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}

	if err := json.Unmarshal(respBody, &resData); err != nil {
		return nil, fmt.Errorf("failed to decode gemini response: %s", string(respBody))
	}

	if len(resData.Candidates) == 0 || len(resData.Candidates[0].Content.Parts) == 0 {
		return nil, fmt.Errorf("no content in gemini response: %s", string(respBody))
	}

	text := trimMarkdown(resData.Candidates[0].Content.Parts[0].Text)

	var meta ClipMetadata
	if err := json.Unmarshal([]byte(text), &meta); err != nil {
		return nil, fmt.Errorf("failed to parse json response: %s", text)
	}

	return &meta, nil
}

func trimMarkdown(s string) string {
	sStr := strings.TrimSpace(s)
	sStr = strings.TrimPrefix(sStr, "```json")
	sStr = strings.TrimPrefix(sStr, "```")
	sStr = strings.TrimSuffix(sStr, "```")
	return strings.TrimSpace(sStr)
}
