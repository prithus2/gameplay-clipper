package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gameplay-clipper/pkg/cutter"
	"github.com/gameplay-clipper/pkg/ingest"
	"github.com/gameplay-clipper/pkg/llm"
	"github.com/gameplay-clipper/pkg/profile"
	"github.com/gameplay-clipper/pkg/review"
	"github.com/gameplay-clipper/pkg/scoring"
	"github.com/gameplay-clipper/pkg/signal"
	"github.com/gameplay-clipper/pkg/store"
)

func main() {
	includeFlag := flag.String("include", "", "Comma-separated manual time ranges (e.g. 12:30-13:00)")
	onlyIncludeFlag := flag.Bool("only-include", false, "cut only the --include windows: skip detection, scoring, LLM judging, and the review queue")
	reviewAddr := flag.String("review-addr", "127.0.0.1:8787", "address the local review UI listens on")
	profilesDir := flag.String("profiles-dir", "profiles", "directory of per-game profile YAML files")
	dbPath := flag.String("db", filepath.Join("out", "clipper.db"), "path to the job/candidate SQLite database")
	crf := flag.Int("crf", cutter.DefaultCRF, "x264 quality target for rendered clips (lower is better and larger; 18 is visually lossless on gameplay)")
	preset := flag.String("preset", cutter.DefaultPreset, "x264 encode preset (slower trades encode time for a smaller file at the same quality)")
	shortHeight := flag.Int("short-height", 0, "height of the 9:16 short.mp4 canvas; 0 derives it from the source, so a 3840x2160 master renders 2160x3840")
	reelHeight := flag.Int("reel-height", 0, "height of the 9:16 reel.mp4 canvas; 0 derives it from the source, so a 3840x2160 master renders 1214x2160 with no scaling of the gameplay at all")
	reelSpan := flag.String("reel-span", "", "horizontal extent reel.mp4 must keep in frame, as frame fractions (e.g. 0.14-0.87); empty uses the LLM's crop_span. A span of 0.32 or less renders full-bleed; wider letterboxes to fit it, down to 0-1 which keeps the whole frame. Set it when the model misjudges what has to stay in shot, or to reframe without an API key")
	formatsFlag := flag.String("formats", "clip,reel", "outputs to render for windows that skip the review queue (--include): clip (native master), reel (9:16 full-bleed, the Instagram default), short (9:16 blur-pad, for wide cinematic framing). Reviewed candidates are chosen per clip in the review UI.")
	flag.Parse()

	args := flag.Args()
	if len(args) < 1 {
		log.Fatalf("Usage: %s [flags] <input_video>", os.Args[0])
	}
	inputFile := args[0]

	if *onlyIncludeFlag && *includeFlag == "" {
		log.Fatalf("--only-include needs --include=MM:SS-MM:SS: it cuts exactly the windows you name and nothing else, so with no windows there is nothing to do.")
	}

	// Encode settings are run-wide; which of the two outputs to write is
	// per-clip (the review UI's checkboxes), so Formats here only applies to
	// --include windows, which never reach the review queue.
	manualFormats, err := cutter.ParseFormats(strings.Split(*formatsFlag, ","))
	if err != nil {
		log.Fatalf("--formats: %v", err)
	}
	spanX0, spanX1, err := parseReelSpan(*reelSpan)
	if err != nil {
		log.Fatalf("--reel-span: %v", err)
	}
	renderOpts := cutter.RenderOpts{CRF: *crf, Preset: *preset, ShortHeight: *shortHeight, ReelHeight: *reelHeight, SpanX0: spanX0, SpanX1: spanX1}

	// Parse game name from filename (e.g., gow_2026-06-29.mp4 -> Game: "gow")
	filename := filepath.Base(inputFile)
	gameName := "unknown"
	if parts := strings.SplitN(filename, "_", 2); len(parts) > 0 && parts[0] != "" {
		gameName = parts[0]
	}
	fmt.Printf("Detected Game: %s\n", gameName)

	// Phase 0: Ingest & Normalize
	fmt.Println("Phase 0: Probing input...")

	meta, err := ingest.ProbeVideo(inputFile)
	if err != nil {
		log.Fatalf("Failed to probe input video: %v (the pipeline needs a real duration for scoring bounds, so this is fatal rather than a warning)", err)
	}
	if len(meta.Streams) == 0 {
		log.Fatalf("ffprobe returned no video streams for %s", inputFile)
	}
	stream := meta.Streams[0]
	duration, err := strconv.ParseFloat(meta.Format.Duration, 64)
	if err != nil || duration <= 0 {
		log.Fatalf("Failed to parse a valid duration from ffprobe output (%q): %v", meta.Format.Duration, err)
	}
	fmt.Printf("Input info: %dx%d, Codec: %s, Framerate: %s, Duration: %.1fs\n",
		stream.Width, stream.Height, stream.CodecName, stream.RFrameRate, duration)

	// Parsed before any transcoding: a typo in --include should fail in
	// milliseconds, not after a full-length proxy encode.
	var manualWindows []scoring.Candidate
	if *includeFlag != "" {
		manualWindows, err = parseManualWindows(*includeFlag, duration)
		if err != nil {
			log.Fatalf("Failed to parse manual windows: %v", err)
		}
		if len(manualWindows) == 0 {
			log.Fatalf("--include=%q parsed to zero windows.", *includeFlag)
		}
		fmt.Printf("Parsed %d manual window(s).\n", len(manualWindows))
	}

	// Loaded before anything is rendered, not just before detection: reel.mp4
	// takes its keep_visible HUD regions from the profile, so even
	// --only-include (which skips every detection stage) needs it.
	prof, err := profile.LoadForGame(*profilesDir, gameName)
	if err != nil {
		log.Fatalf("Failed to load profile for %q: %v", gameName, err)
	}
	fmt.Printf("Loaded profile %q (version %d): %d signals, %d archetypes\n",
		prof.Game, prof.Version, len(prof.Signals), len(prof.Archetypes))
	renderOpts.HUD = toHUDRegions(prof)

	apiKey := os.Getenv("GEMINI_API_KEY")

	// The proxy is a full-length 720p transcode. Detection needs it, but in
	// --only-include mode its only remaining consumer is Gemini (titles and
	// thumbnail frame choice) — so with no key there is nobody to hand it to
	// and we skip the encode outright.
	proxyFile := ""
	if !*onlyIncludeFlag || apiKey != "" {
		fmt.Println("Normalizing to 720p proxy...")
		proxyFile, err = ingest.NormalizeToProxy(inputFile)
		if err != nil {
			log.Fatalf("Ingest failed: %v", err)
		}
		fmt.Printf("Proxy created: %s\n", proxyFile)
	}

	// --only-include: the human already picked the windows, so every stage
	// that exists to *find* or *vet* windows is dead weight. Cut and exit
	// before touching the profile, the job store, or the review server.
	if *onlyIncludeFlag {
		if apiKey == "" {
			fmt.Println("GEMINI_API_KEY is not set — cutting windows with no title, metadata.json, or thumbnail.")
		}
		fmt.Printf("\nOnly-include mode: cutting %d window(s), skipping detection, scoring, LLM judging, and review.\n", len(manualWindows))
		cutManualWindows(inputFile, proxyFile, gameName, apiKey, manualWindows, renderOpts.WithFormats(manualFormats))
		fmt.Println("\nDone.")
		return
	}

	if err := os.MkdirAll(filepath.Dir(*dbPath), 0755); err != nil {
		log.Fatalf("Failed to create db directory: %v", err)
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("Failed to open job store: %v", err)
	}
	defer st.Close()

	sessionID := sessionIDFor(inputFile)
	if err := st.UpsertSession(store.Session{
		ID: sessionID, Game: gameName, InputPath: inputFile, ProxyPath: proxyFile,
		DurationS: duration, ProfileVersion: prof.Version,
	}); err != nil {
		log.Fatalf("Failed to record session: %v", err)
	}

	// Phase 1: per-game + generic signal detection, keyed for idempotent re-runs.
	fmt.Println("\nPhase 1: Running signal detectors...")
	detectors := []signal.Detector{
		signal.GenericDetector{},
		signal.TextBannerDetector{},
		signal.KillFeedDetector{},
		signal.ASRDetector{},
		signal.LetterboxDetector{},
		signal.SubtitleDetector{},
	}
	detectHash, err := store.HashConfig(struct {
		ProfileVersion int
		ProxyFile      string
	}{prof.Version, proxyFile})
	if err != nil {
		log.Fatalf("Failed to hash detection config: %v", err)
	}
	eventsJSON, err := st.RunJob(sessionID, "detect", detectHash, func() (json.RawMessage, error) {
		events, errs := signal.Run(proxyFile, prof, detectors)
		for _, e := range errs {
			log.Printf("Warning: a detector failed and its events were dropped: %v", e)
		}
		return json.Marshal(events)
	})
	if err != nil {
		log.Fatalf("Signal detection failed: %v", err)
	}
	var events []signal.Event
	if err := json.Unmarshal(eventsJSON, &events); err != nil {
		log.Fatalf("Failed to decode signal events: %v", err)
	}
	fmt.Printf("Detected %d raw signal events.\n", len(events))

	// Phase 1b: deterministic scoring — replaces the old gap-merge heuristic.
	fmt.Println("\nPhase 1b: Deterministic scoring...")
	candidates := scoring.Score(toScoringEvents(events), toScoringConfig(prof), duration)
	fmt.Printf("Scored %d candidate window(s) from %d event(s).\n", len(candidates), len(events))

	// The human review queue is the accept/reject gate now, not the LLM: no
	// Gemini call is spent up front. Candidates that survive scoring are
	// persisted as pending_review, and the LLM only generates a title/thumbnail
	// for the ones a human actually approves (see pkg/review.cutApproved). A
	// missing key is therefore no longer fatal — it just means approved clips
	// get cut without a title or thumbnail.
	if apiKey == "" && len(candidates) > 0 {
		fmt.Println("GEMINI_API_KEY is not set — approved clips will be cut without a title, metadata.json, or thumbnail.")
	}

	fmt.Println("\nPhase 3: Queueing candidates for review...")
	for i, c := range candidates {
		// Find-or-insert, not insert: scoring is deterministic, so a re-run
		// reproduces the same windows, and each must resolve to its existing
		// row (keeping its review status) instead of duplicating it. Nothing
		// is judged here — the row simply enters (or stays in) the queue.
		signalsJSON, _ := json.Marshal(c.SignalsFired)
		cand, existed, err := st.FindOrInsertCandidate(store.Candidate{
			SessionID: sessionID, Archetype: c.Archetype, Start: c.Start, End: c.End,
			DetScore: c.Score, SignalsFired: signalsJSON,
		})
		if err != nil {
			log.Printf("Failed to store candidate %d: %v", i+1, err)
			continue
		}
		if existed && cand.Status != "pending_review" {
			fmt.Printf("Candidate %d/%d (%s, %.1fs-%.1fs) already %s in a previous run — skipping.\n",
				i+1, len(candidates), c.Archetype, c.Start, c.End, cand.Status)
			continue
		}
		fmt.Printf("Queued candidate %d/%d (%s, %.1fs-%.1fs, det_score=%.2f) for review.\n",
			i+1, len(candidates), c.Archetype, c.Start, c.End, c.Score)
	}

	// Manual windows bypass review entirely (a human already hand-picked
	// them), so render them now rather than waiting on the review pass below.
	cutManualWindows(inputFile, proxyFile, gameName, apiKey, manualWindows, renderOpts.WithFormats(manualFormats))

	// Review queue: nothing detected gets cut without a human approving it.
	// Each candidate is rendered as soon as it's approved (see
	// pkg/review.cutApproved) — and the LLM title/thumbnail is generated right
	// then, only for approved clips, so clip 1 lands on disk while clip 40 is
	// still waiting for the reviewer. apiKey is handed to the server for that
	// just-in-time metadata call.
	pending, err := st.ListCandidatesByStatus(sessionID, "pending_review")
	if err != nil {
		log.Fatalf("Failed to list pending candidates: %v", err)
	}
	if len(pending) > 0 {
		srv, err := review.New(st, sessionID, proxyFile, inputFile, gameName, apiKey, renderOpts, defaultFormatsFor(prof))
		if err != nil {
			log.Fatalf("Failed to start review server: %v", err)
		}
		if err := srv.RunUntilDone(*reviewAddr); err != nil {
			log.Fatalf("Review server error: %v", err)
		}
		srv.Close()
	} else {
		fmt.Println("\nNothing pending review.")
	}

	fmt.Println("\nDone.")
}

// sessionIDFor derives a stable session id from the input file path, so
// re-running the tool on the same recording reuses the same session (and
// therefore the same idempotent job cache) instead of minting a new one
// every time.
func sessionIDFor(inputFile string) string {
	abs, err := filepath.Abs(inputFile)
	if err != nil {
		abs = inputFile
	}
	sum := sha256.Sum256([]byte(abs))
	return hex.EncodeToString(sum[:])[:16]
}

func toScoringEvents(events []signal.Event) []scoring.Event {
	out := make([]scoring.Event, len(events))
	for i, e := range events {
		out[i] = scoring.Event{Kind: e.Kind, T: e.T, Confidence: e.Confidence, Payload: e.Payload, Source: e.Source}
	}
	return out
}

// toHUDRegions translates the profile's keep_visible block into the renderer's
// own region type, the same way toScoringConfig translates archetypes for the
// scorer — so pkg/cutter never has to depend on the profile loader.
func toHUDRegions(p *profile.Profile) []cutter.HUDRegion {
	if p.Reel == nil {
		return nil
	}
	out := make([]cutter.HUDRegion, 0, len(p.Reel.KeepVisible))
	for _, kv := range p.Reel.KeepVisible {
		out = append(out, cutter.HUDRegion{
			Name:   kv.Name,
			X:      kv.Region.X,
			Y:      kv.Region.Y,
			W:      kv.Region.W,
			H:      kv.Region.H,
			Place:  kv.Place,
			Scale:  kv.Scale,
			Signal: kv.Signal,
			HoldS:  kv.HoldS,
		})
	}
	return out
}

// toCutterSignals hands the renderer the detector hits it needs to gate
// transient HUD bands. A hand-picked --include window has none, which leaves
// every band up for the whole clip — the right fallback when nothing is known
// about when the element was on screen.
func toCutterSignals(fired []scoring.FiredSignal) []cutter.FiredSignal {
	out := make([]cutter.FiredSignal, len(fired))
	for i, s := range fired {
		out[i] = cutter.FiredSignal{Kind: s.Kind, T: s.T}
	}
	return out
}

// defaultFormatsFor picks which files a candidate's review card starts with
// ticked. reel is the vertical default everywhere except the archetypes a
// profile marks as blur-pad — wide cinematic framing that cropping would
// destroy rather than reframe.
func defaultFormatsFor(p *profile.Profile) func(string) []cutter.Format {
	return func(archetype string) []cutter.Format {
		if p.Reel.PrefersBlurpad(archetype) {
			return []cutter.Format{cutter.FormatClip, cutter.FormatShort}
		}
		return cutter.DefaultFormats
	}
}

func toScoringConfig(p *profile.Profile) scoring.Config {
	cfg := scoring.Config{RoundBoundarySignal: p.RoundBoundarySignal, NMSRadiusS: p.NMSRadiusS}
	for _, s := range p.Signals {
		cfg.Signals = append(cfg.Signals, scoring.SignalSpec{
			Name: s.Name, IsBoundary: s.Kind == profile.KindRoundBoundary,
			Weight: s.Weight, BoundaryRole: s.BoundaryRole, DecaySeconds: s.DecaySeconds,
		})
	}
	for _, a := range p.Archetypes {
		cfg.Archetypes = append(cfg.Archetypes, scoring.ArchetypeSpec{
			Name: a.Name, Threshold: a.Threshold,
			MinLengthS: a.MinLengthS, MaxLengthS: a.MaxLengthS,
			LeadPadS: a.LeadPadS, TrailPadS: a.TrailPadS,
			StreakMergeGapS: a.StreakMergeGapS,
		})
	}
	return cfg
}

// cutManualWindows renders hand-picked --include windows. They skip the review
// queue (a human already chose them) but still get LLM metadata for a title and
// thumbnail frame when a key is available; a metadata failure is logged and the
// clip is still cut, since the video is the point and the title is a nicety.
func cutManualWindows(inputFile, proxyFile, gameName, apiKey string, windows []scoring.Candidate, opts cutter.RenderOpts) {
	if len(windows) == 0 {
		return
	}
	var clips []cutter.Clip
	for i, w := range windows {
		var m *llm.ClipMetadata
		if apiKey != "" {
			var err error
			m, err = llm.RankAndMetadata(proxyFile, gameName, w, apiKey)
			if err != nil {
				log.Printf("Failed to generate metadata for manual window %d: %v (cutting it without a title or thumbnail)", i+1, err)
			}
		}
		clips = append(clips, cutter.Clip{Start: w.Start, End: w.End, Metadata: m, Signals: toCutterSignals(w.SignalsFired)})
	}

	fmt.Printf("\nProcessing %d manual clip(s)...\n", len(clips))
	if err := cutter.CutAndReformat(inputFile, gameName, clips, opts); err != nil {
		log.Fatalf("Cut and reformat failed: %v", err)
	}
}

// parseManualWindows parses the --include ranges and bounds them against the
// real video duration. Out-of-range windows are caught here rather than handed
// to ffmpeg, which would silently emit a short or empty clip instead of failing.
func parseManualWindows(flagStr string, duration float64) ([]scoring.Candidate, error) {
	var windows []scoring.Candidate
	parts := strings.Split(flagStr, ",")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		times := strings.Split(p, "-")
		if len(times) != 2 {
			return nil, fmt.Errorf("invalid range format: %s (expected MM:SS-MM:SS)", p)
		}
		start, err := parseMMSS(times[0])
		if err != nil {
			return nil, err
		}
		end, err := parseMMSS(times[1])
		if err != nil {
			return nil, err
		}
		if start < 0 {
			return nil, fmt.Errorf("start time must not be negative: %s", p)
		}
		if start >= end {
			return nil, fmt.Errorf("start time must be before end time: %s", p)
		}
		if start >= duration {
			return nil, fmt.Errorf("window %s starts at %.1fs, past the end of the %.1fs video", p, start, duration)
		}
		if end > duration {
			fmt.Printf("Warning: window %s ends at %.1fs, past the end of the %.1fs video — trimming to the last frame.\n", p, end, duration)
			end = duration
		}
		windows = append(windows, scoring.Candidate{Archetype: "manual", Start: start, End: end})
	}
	return windows, nil
}

// parseReelSpan reads the --reel-span override, "x0-x1" in frame fractions.
// An empty string means "no override" and defers to the LLM's crop_span.
// Rejected rather than clamped, since a typo here silently reframes every clip
// in the run and there is no cheap way to notice from the output.
func parseReelSpan(s string) (float64, float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, nil
	}
	parts := strings.Split(s, "-")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid span %q (expected two frame fractions, e.g. 0.14-0.87)", s)
	}
	x0, err := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	if err != nil {
		return 0, 0, fmt.Errorf("bad start %q: %w", parts[0], err)
	}
	x1, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err != nil {
		return 0, 0, fmt.Errorf("bad end %q: %w", parts[1], err)
	}
	if x0 < 0 || x1 > 1 {
		return 0, 0, fmt.Errorf("span %q runs outside the frame: both ends must be between 0 and 1", s)
	}
	if x1 <= x0 {
		return 0, 0, fmt.Errorf("span %q ends before it starts", s)
	}
	return x0, x1, nil
}

func parseMMSS(s string) (float64, error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) == 1 {
		return strconv.ParseFloat(parts[0], 64)
	}
	if len(parts) != 2 {
		return 0, fmt.Errorf("invalid time format: %s", s)
	}
	m, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return 0, err
	}
	sec, err := strconv.ParseFloat(parts[1], 64)
	if err != nil {
		return 0, err
	}
	return m*60 + sec, nil
}
