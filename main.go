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
	reviewAddr := flag.String("review-addr", "127.0.0.1:8787", "address the local review UI listens on")
	profilesDir := flag.String("profiles-dir", "profiles", "directory of per-game profile YAML files")
	dbPath := flag.String("db", filepath.Join("out", "clipper.db"), "path to the job/candidate SQLite database")
	flag.Parse()

	args := flag.Args()
	if len(args) < 1 {
		log.Fatalf("Usage: %s [flags] <input_video>", os.Args[0])
	}
	inputFile := args[0]

	// Parse game name from filename (e.g., gow_2026-06-29.mp4 -> Game: "gow")
	filename := filepath.Base(inputFile)
	gameName := "unknown"
	if parts := strings.SplitN(filename, "_", 2); len(parts) > 0 && parts[0] != "" {
		gameName = parts[0]
	}
	fmt.Printf("Detected Game: %s\n", gameName)

	// Phase 0: Ingest & Normalize
	fmt.Println("Phase 0: Ingesting and normalizing to 720p proxy...")

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

	proxyFile, err := ingest.NormalizeToProxy(inputFile)
	if err != nil {
		log.Fatalf("Ingest failed: %v", err)
	}
	fmt.Printf("Proxy created: %s\n", proxyFile)

	prof, err := profile.LoadForGame(*profilesDir, gameName)
	if err != nil {
		log.Fatalf("Failed to load profile for %q: %v", gameName, err)
	}
	fmt.Printf("Loaded profile %q (version %d): %d signals, %d archetypes\n",
		prof.Game, prof.Version, len(prof.Signals), len(prof.Archetypes))

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

	var manualWindows []scoring.Candidate
	if *includeFlag != "" {
		manualWindows, err = parseManualWindows(*includeFlag)
		if err != nil {
			log.Fatalf("Failed to parse manual windows: %v", err)
		}
		fmt.Printf("Added %d manual window(s).\n", len(manualWindows))
	}

	// Phase 3: LLM judges every detected candidate, informed by the
	// deterministic evidence. The LLM is the final judge by design (per
	// project decision) — running without it would mean no accept/reject
	// gate at all, so this is a hard failure, not a silent "keep everything".
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" && len(candidates) > 0 {
		log.Fatalf("GEMINI_API_KEY is not set. The LLM is the judge for detected candidates in this pipeline — refusing to auto-publish %d unreviewed candidate(s) instead of silently keeping all of them. Set GEMINI_API_KEY, or use --include to hand-pick clips manually.", len(candidates))
	}

	fmt.Println("\nPhase 3: LLM judging candidates...")
	for i, c := range candidates {
		fmt.Printf("Judging candidate %d/%d (%s, %.1fs-%.1fs, det_score=%.2f)...\n",
			i+1, len(candidates), c.Archetype, c.Start, c.End, c.Score)

		// Store the candidate before judging it, not after: if the LLM call
		// fails outright (e.g. rate-limited past its retries), the candidate
		// still lands in the review queue as pending_review with no verdict,
		// instead of silently vanishing from the pipeline. Find-or-insert,
		// not insert: scoring is deterministic, so a re-run reproduces the
		// same windows, and each must resolve to its existing row instead of
		// duplicating it — and re-spending a Gemini call on it.
		signalsJSON, _ := json.Marshal(c.SignalsFired)
		cand, existed, err := st.FindOrInsertCandidate(store.Candidate{
			SessionID: sessionID, Archetype: c.Archetype, Start: c.Start, End: c.End,
			DetScore: c.Score, SignalsFired: signalsJSON,
		})
		if err != nil {
			log.Printf("Failed to store candidate %d: %v", i+1, err)
			continue
		}
		candID := cand.ID

		// A candidate that already has an LLM verdict, or that a human has
		// already approved/rejected, is done — only re-judge existing rows
		// whose previous judge attempt failed (no verdict, still pending).
		if existed && (cand.LLMIsHighlight.Valid || cand.Status != "pending_review") {
			fmt.Printf("  -> Already judged in a previous run (status=%s, title=%q) — skipping Gemini call.\n",
				cand.Status, cand.LLMTitle.String)
			continue
		}

		m, err := llm.RankAndMetadata(proxyFile, gameName, c, apiKey)
		if err != nil {
			log.Printf("Failed to judge candidate %d: %v (left pending_review with no LLM verdict — judge it manually)", i+1, err)
			continue
		}
		fmt.Printf("  -> Highlight=%v, Score=%.2f, Title=%q\n", m.IsHighlight, m.Score, m.Title)

		metaJSON, _ := json.Marshal(m)
		if err := st.SetCandidateLLMVerdict(candID, m.IsHighlight, m.Score, m.Title, m.Reason, metaJSON); err != nil {
			log.Printf("Failed to store LLM verdict for candidate %d: %v", i+1, err)
		}
	}

	// Manual windows still get LLM metadata (title/thumbnail), but bypass
	// the review gate entirely — a human already hand-picked them.
	var manualClips []cutter.Clip
	for i, w := range manualWindows {
		var m *llm.ClipMetadata
		if apiKey != "" {
			var err error
			m, err = llm.RankAndMetadata(proxyFile, gameName, w, apiKey)
			if err != nil {
				log.Printf("Failed to generate metadata for manual window %d: %v", i+1, err)
			}
		}
		manualClips = append(manualClips, cutter.Clip{Start: w.Start, End: w.End, Metadata: m})
	}

	// Manual windows bypass review entirely (a human already hand-picked
	// them), so render them now rather than waiting on the review pass below.
	if len(manualClips) > 0 {
		fmt.Printf("\nProcessing %d manual clip(s)...\n", len(manualClips))
		if err := cutter.CutAndReformat(inputFile, gameName, manualClips); err != nil {
			log.Fatalf("Cut and reformat failed: %v", err)
		}
	}

	// Review queue: nothing detected gets cut without a human approving it.
	// Each candidate is rendered as soon as it's approved (see
	// pkg/review.cutApproved), not batched until every candidate has been
	// judged and reviewed — so clip 1 lands on disk while clip 40 is still
	// waiting on Gemini.
	pending, err := st.ListCandidatesByStatus(sessionID, "pending_review")
	if err != nil {
		log.Fatalf("Failed to list pending candidates: %v", err)
	}
	if len(pending) > 0 {
		srv, err := review.New(st, sessionID, proxyFile, inputFile, gameName)
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

func parseManualWindows(flagStr string) ([]scoring.Candidate, error) {
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
		if start >= end {
			return nil, fmt.Errorf("start time must be before end time: %s", p)
		}
		windows = append(windows, scoring.Candidate{Archetype: "manual", Start: start, End: end})
	}
	return windows, nil
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
