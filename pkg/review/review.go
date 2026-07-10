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
	"github.com/gameplay-clipper/pkg/store"
)

type Server struct {
	st           *store.Store
	sessionID    string
	proxyFile    string
	originalFile string
	gameName     string
	previewDir   string

	cutQueue chan cutJob
	cutWG    sync.WaitGroup
}

type cutJob struct {
	id   int64
	cand store.Candidate
}

// New prepares a review server for one session's pending candidates.
// proxyFile is used to render on-demand preview frames; originalFile and
// gameName are needed to cut each clip the moment it's approved, rather
// than batching renders until the whole review pass is done. Cutting runs
// on a single background worker (see cutWorker) rather than one goroutine
// per approval: firing off a dozen concurrent ffmpeg cut/reformat pipelines
// starves the CPU badly enough that the review page's own thumbnail
// extraction stalls out and looks like a blank/black UI.
func New(st *store.Store, sessionID, proxyFile, originalFile, gameName string) (*Server, error) {
	dir, err := os.MkdirTemp("", "review-previews-*")
	if err != nil {
		return nil, fmt.Errorf("create preview dir: %w", err)
	}
	s := &Server{
		st: st, sessionID: sessionID, proxyFile: proxyFile,
		originalFile: originalFile, gameName: gameName, previewDir: dir,
		cutQueue: make(chan cutJob, 256),
	}
	go s.cutWorker()
	return s, nil
}

// cutWorker processes approved candidates one at a time in the background,
// so ffmpeg's own internal multithreading gets full use of the machine
// instead of competing against several other ffmpeg processes at once.
func (s *Server) cutWorker() {
	for job := range s.cutQueue {
		s.cutApproved(job.id, job.cand)
		s.cutWG.Done()
	}
}

func (s *Server) Close() error { return os.RemoveAll(s.previewDir) }

// RunUntilDone serves the review UI at addr and blocks until the reviewer
// clicks "Finish review" (POST /api/done), then shuts the server down.
func (s *Server) RunUntilDone(addr string) error {
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
		s.cutQueue <- cutJob{id: id, cand: *cand}

	case "reject":
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

// cutApproved renders one just-approved candidate into
// out/<gameName>/clip_<id>/. Runs off the HTTP goroutine so the browser
// isn't left waiting on ffmpeg before it can move on to reviewing the next
// candidate.
func (s *Server) cutApproved(id int64, c store.Candidate) {
	var meta *llm.ClipMetadata
	if len(c.LLMMetadataJSON) > 0 {
		meta = &llm.ClipMetadata{}
		if err := json.Unmarshal(c.LLMMetadataJSON, meta); err != nil {
			log.Printf("review: candidate %d: failed to decode LLM metadata, cutting without it: %v", id, err)
			meta = nil
		}
	}
	name := fmt.Sprintf("clip_%d", id)
	fmt.Printf("\nApproved candidate %d — cutting %s now...\n", id, name)
	clip := cutter.Clip{Start: c.Start, End: c.End, Metadata: meta}
	if err := cutter.CutOne(s.originalFile, s.gameName, name, clip); err != nil {
		log.Printf("review: candidate %d: cut failed: %v", id, err)
		return
	}
	fmt.Printf("Candidate %d done: out/%s/%s\n", id, s.gameName, name)
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
