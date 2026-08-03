// Package review is the safety net between LLM judgment and actually
// cutting a clip: candidates land here as pending_review, not published
// automatically, and a human approves/rejects/nudges before the renderer
// ever touches the master file. This replaces the old "score >= 0.5 ->
// auto-cut" behavior, which shipped false positives like a resting
// cutscene scored as a highlight straight to disk.
//
// Deliberately minimal: one Go binary serving plain HTML + vanilla JS on
// localhost, no separate frontend build. Keyboard-first (J/K move, A
// approve, R reject) so reviewing a session's candidates stays fast.
package review

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gameplay-clipper/pkg/cutter"
	"github.com/gameplay-clipper/pkg/llm"
	"github.com/gameplay-clipper/pkg/scoring"
	"github.com/gameplay-clipper/pkg/store"
)

type Server struct {
	st           *store.Store
	sessionID    string
	proxyFile    string
	originalFile string
	gameName     string
	apiKey       string
	previewDir   string
	render       cutter.RenderOpts

	cutQueue chan cutJob
	cutWG    sync.WaitGroup
}

type cutJob struct {
	id      int64
	cand    store.Candidate
	formats []cutter.Format
}

// New prepares a review server for one session's pending candidates.
// proxyFile is used to render on-demand preview frames and to sample the
// frames the LLM sees; originalFile and gameName are needed to cut each clip
// the moment it's approved, rather than batching renders until the whole
// review pass is done. apiKey lets the server generate a title/thumbnail via
// the LLM at approval time, only for clips a human actually approves — an
// empty key just means approved clips are cut without that metadata. render
// carries the run-wide encode settings (CRF, preset, short canvas height); its
// Formats field is ignored, because which outputs to write is chosen per
// candidate by the reviewer's checkboxes.
//
// Approvals are rendered by a single background worker started in RunUntilDone
// (see cutWorker), not one goroutine per approval: firing off a dozen
// concurrent ffmpeg pipelines starves the CPU badly enough that the review
// page's own thumbnail extraction stalls out and looks like a blank/black UI.
// The worker belongs to RunUntilDone rather than to New so that constructing a
// server never leaves a goroutine running behind the caller's back.
func New(st *store.Store, sessionID, proxyFile, originalFile, gameName, apiKey string, render cutter.RenderOpts) (*Server, error) {
	dir, err := os.MkdirTemp("", "review-previews-*")
	if err != nil {
		return nil, fmt.Errorf("create preview dir: %w", err)
	}
	s := &Server{
		st: st, sessionID: sessionID, proxyFile: proxyFile,
		originalFile: originalFile, gameName: gameName, apiKey: apiKey, previewDir: dir,
		render:   render,
		cutQueue: make(chan cutJob, 256),
	}
	return s, nil
}

// cutWorker processes approved candidates one at a time in the background,
// so ffmpeg's own internal multithreading gets full use of the machine
// instead of competing against several other ffmpeg processes at once.
func (s *Server) cutWorker() {
	for job := range s.cutQueue {
		s.cutApproved(job.id, job.cand, job.formats)
		s.cutWG.Done()
	}
}

func (s *Server) Close() error { return os.RemoveAll(s.previewDir) }

// RunUntilDone serves the review UI at addr and blocks until the reviewer
// clicks "Finish review" (POST /api/done), then shuts the server down.
func (s *Server) RunUntilDone(addr string) error {
	go s.cutWorker()

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/candidates", s.handleList)
	mux.HandleFunc("/api/candidates/", s.handleCandidateAction)

	done := make(chan struct{})
	mux.HandleFunc("/api/done", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		close(done)
	})

	httpServer := &http.Server{Addr: addr, Handler: mux}
	errCh := make(chan error, 1)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	fmt.Printf("\nReview queue ready: open http://%s in a browser.\n", addr)
	fmt.Println("Approve/reject each candidate, then click \"Finish review\" to continue.")

	select {
	case err := <-errCh:
		return err
	case <-done:
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		return err
	}

	// Shutdown only returns once every in-flight handler (including any
	// approve request) has completed, so it's now safe to close the queue:
	// nothing more will be sent to it. Wait for whatever's still
	// cutting/queued rather than letting main() return and kill it mid-render.
	close(s.cutQueue)
	fmt.Println("\nFinishing background clip renders...")
	s.cutWG.Wait()
	return nil
}

type candidateView struct {
	ID           int64           `json:"id"`
	Archetype    string          `json:"archetype"`
	Start        float64         `json:"start"`
	End          float64         `json:"end"`
	DetScore     float64         `json:"det_score"`
	SignalsFired json.RawMessage `json:"signals_fired"`
	LLMIsHighlight bool          `json:"llm_is_highlight"`
	LLMScore     float64         `json:"llm_score"`
	LLMTitle     string          `json:"llm_title"`
	LLMReason    string          `json:"llm_reason"`
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	cands, err := s.st.ListCandidatesByStatus(s.sessionID, "pending_review")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	views := make([]candidateView, 0, len(cands))
	for _, c := range cands {
		views = append(views, candidateView{
			ID: c.ID, Archetype: c.Archetype, Start: c.Start, End: c.End, DetScore: c.DetScore,
			SignalsFired:   c.SignalsFired,
			LLMIsHighlight: c.LLMIsHighlight.Bool,
			LLMScore:       c.LLMScore.Float64,
			LLMTitle:       c.LLMTitle.String,
			LLMReason:      c.LLMReason.String,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(views)
}

// handleCandidateAction dispatches /api/candidates/{id}/{approve|reject|preview.jpg}.
func (s *Server) handleCandidateAction(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/candidates/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.Error(w, "invalid candidate id", http.StatusBadRequest)
		return
	}

	switch parts[1] {
	case "approve":
		// The reviewer's format checkboxes ride along with the approval.
		// Validated before the status flips, so a bad selection leaves the
		// candidate pending and re-reviewable rather than approved-but-unrendered.
		formats, err := requestedFormats(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		cand, err := s.st.GetCandidate(id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if cand.Status == "approved" {
			w.WriteHeader(http.StatusOK) // already approved (and cutting/cut) — idempotent no-op
			return
		}
		if err := s.st.SetCandidateReviewStatus(id, "approved", ""); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		// Queue this clip to cut in the background instead of waiting for
		// every remaining candidate to be reviewed — so approving clip 3 of
		// 40 doesn't sit idle until clip 40 has been judged and reviewed
		// too. Queued (not fired off immediately) so a burst of approvals
		// cuts one at a time instead of saturating the CPU and starving the
		// review page's own thumbnail requests.
		s.cutWG.Add(1)
		s.cutQueue <- cutJob{id: id, cand: *cand, formats: formats}

	case "reject":
		// Nothing is queued for cutting here, deliberately: a discarded
		// candidate never costs an encode, and no clip.mp4 or short.mp4 is
		// written for it.
		var body struct {
			Reason string `json:"reason"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if err := s.st.SetCandidateReviewStatus(id, "rejected", body.Reason); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)

	case "preview.jpg":
		path, err := s.previewFrame(id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		http.ServeFile(w, r, path)

	default:
		http.NotFound(w, r)
	}
}

// requestedFormats reads the reviewer's output selection off an approve
// request. An absent "formats" key means the caller never offered a choice
// (curl, a keyboard shortcut on an older page) and falls back to rendering
// everything; an explicitly empty list is a mistake worth surfacing, since
// approving something and asking for no files is never what was meant.
func requestedFormats(r *http.Request) ([]cutter.Format, error) {
	var body struct {
		Formats *[]string `json:"formats"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Formats == nil {
		return cutter.AllFormats, nil
	}
	return cutter.ParseFormats(*body.Formats)
}

// cutApproved renders one just-approved candidate into
// out/<gameName>/clip_<id>/. Runs off the HTTP goroutine so the browser
// isn't left waiting on ffmpeg before it can move on to reviewing the next
// candidate. The LLM title/thumbnail metadata is generated here, at approval
// time, only for clips a human actually approved — not up front for every
// scored candidate. A prior run may have already stored it (LLMMetadataJSON),
// in which case that's reused instead of re-spending a Gemini call.
func (s *Server) cutApproved(id int64, c store.Candidate, formats []cutter.Format) {
	meta := s.metadataFor(id, c)
	name := fmt.Sprintf("clip_%d", id)
	fmt.Printf("\nApproved candidate %d — rendering %s (%s) now...\n", id, name, formatList(formats))
	clip := cutter.Clip{Start: c.Start, End: c.End, Metadata: meta}
	if err := cutter.CutOne(s.originalFile, s.gameName, name, clip, s.render.WithFormats(formats)); err != nil {
		log.Printf("review: candidate %d: cut failed: %v", id, err)
		return
	}
	fmt.Printf("Candidate %d done: out/%s/%s\n", id, s.gameName, name)
}

// formatList renders a format selection for the console line, e.g. "clip.mp4 + short.mp4".
func formatList(formats []cutter.Format) string {
	names := make([]string, len(formats))
	for i, f := range formats {
		names[i] = string(f) + ".mp4"
	}
	return strings.Join(names, " + ")
}

// metadataFor resolves the LLM metadata used to cut an approved clip. It
// prefers a verdict already persisted by an earlier run; failing that, and
// only if a key is configured, it asks the LLM now and persists the result so
// a re-approval doesn't pay for the call again. Any failure (no key, LLM
// error, bad JSON) returns nil — the clip is still cut, just without a title
// or thumbnail, matching the --include manual-window fallback.
func (s *Server) metadataFor(id int64, c store.Candidate) *llm.ClipMetadata {
	if len(c.LLMMetadataJSON) > 0 {
		meta := &llm.ClipMetadata{}
		if err := json.Unmarshal(c.LLMMetadataJSON, meta); err != nil {
			log.Printf("review: candidate %d: failed to decode stored LLM metadata, regenerating: %v", id, err)
		} else {
			return meta
		}
	}
	if s.apiKey == "" {
		log.Printf("review: candidate %d: GEMINI_API_KEY not set — cutting without a title or thumbnail", id)
		return nil
	}
	fmt.Printf("Generating title/thumbnail for approved candidate %d...\n", id)
	meta, err := llm.RankAndMetadata(s.proxyFile, s.gameName, candToScoring(c), s.apiKey)
	if err != nil {
		log.Printf("review: candidate %d: LLM metadata failed, cutting without it: %v", id, err)
		return nil
	}
	metaJSON, _ := json.Marshal(meta)
	if err := s.st.SetCandidateLLMVerdict(id, meta.IsHighlight, meta.Score, meta.Title, meta.Reason, metaJSON); err != nil {
		log.Printf("review: candidate %d: failed to persist LLM verdict (metadata still used for this cut): %v", id, err)
	}
	return meta
}

// candToScoring rebuilds the scoring.Candidate that llm.RankAndMetadata needs
// from a persisted store row: the deterministic evidence it passes to the LLM.
// Malformed/empty signals JSON degrades to "no signals" rather than failing,
// so a bad row still cuts.
func candToScoring(c store.Candidate) scoring.Candidate {
	var fired []scoring.FiredSignal
	if len(c.SignalsFired) > 0 {
		if err := json.Unmarshal(c.SignalsFired, &fired); err != nil {
			log.Printf("review: candidate %d: could not decode signals for LLM evidence, proceeding without them: %v", c.ID, err)
			fired = nil
		}
	}
	return scoring.Candidate{
		Archetype:    c.Archetype,
		Start:        c.Start,
		End:          c.End,
		Score:        c.DetScore,
		SignalsFired: fired,
	}
}

// previewFrame extracts (and caches) one frame near the start of a
// candidate's window, for the review card's thumbnail.
func (s *Server) previewFrame(id int64) (string, error) {
	cached := filepath.Join(s.previewDir, fmt.Sprintf("%d.jpg", id))
	if _, err := os.Stat(cached); err == nil {
		return cached, nil
	}
	c, err := s.st.GetCandidate(id)
	if err != nil {
		return "", err
	}
	ts := c.Start + 1
	if ts > c.End {
		ts = c.Start
	}
	cmd := exec.Command("ffmpeg", "-y", "-ss", fmt.Sprintf("%.3f", ts), "-i", s.proxyFile, "-frames:v", "1", cached)
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("extract preview frame: %w", err)
	}
	return cached, nil
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := w.Write([]byte(indexHTML)); err != nil {
		log.Printf("review: write index: %v", err)
	}
}
