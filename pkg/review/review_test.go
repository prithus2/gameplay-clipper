package review

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/gameplay-clipper/pkg/cutter"
	"github.com/gameplay-clipper/pkg/store"
)

func itoa(i int64) string { return strconv.FormatInt(i, 10) }

func newTestServer(t *testing.T) (*Server, *store.Store, int64) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	if err := st.UpsertSession(store.Session{ID: "sess1", Game: "valo"}); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	id, err := st.InsertCandidate(store.Candidate{
		SessionID: "sess1", Archetype: "ace", Start: 8, End: 103, DetScore: 3.38,
		SignalsFired: json.RawMessage(`[{"Kind":"ace_banner","T":100}]`),
	})
	if err != nil {
		t.Fatalf("InsertCandidate: %v", err)
	}
	if err := st.SetCandidateLLMVerdict(id, true, 0.9, "Clean Ace", "great sequence", []byte(`{}`)); err != nil {
		t.Fatalf("SetCandidateLLMVerdict: %v", err)
	}

	srv, err := New(st, "sess1", "unused.mp4", "unused_orig.mp4", "valo", "", cutter.DefaultRenderOpts())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv, st, id
}

func TestHandleListReturnsPendingCandidates(t *testing.T) {
	srv, _, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/candidates", nil)
	w := httptest.NewRecorder()
	srv.handleList(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var views []candidateView
	if err := json.Unmarshal(w.Body.Bytes(), &views); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("expected 1 pending candidate, got %d", len(views))
	}
	if views[0].LLMTitle != "Clean Ace" || !views[0].LLMIsHighlight {
		t.Errorf("unexpected candidate view: %+v", views[0])
	}
}

func TestApproveMovesCandidateOutOfPendingReview(t *testing.T) {
	srv, st, id := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/candidates/"+itoa(id)+"/approve", nil)
	w := httptest.NewRecorder()
	srv.handleCandidateAction(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("approve status = %d, body=%s", w.Code, w.Body.String())
	}

	pending, _ := st.ListCandidatesByStatus("sess1", "pending_review")
	if len(pending) != 0 {
		t.Errorf("expected 0 pending after approve, got %d", len(pending))
	}
	approved, _ := st.ListCandidatesByStatus("sess1", "approved")
	if len(approved) != 1 {
		t.Errorf("expected 1 approved, got %d", len(approved))
	}
}

func TestApproveQueuesOnlyTheSelectedFormats(t *testing.T) {
	srv, _, id := newTestServer(t)

	body, _ := json.Marshal(map[string][]string{"formats": {"short"}})
	req := httptest.NewRequest(http.MethodPost, "/api/candidates/"+itoa(id)+"/approve", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleCandidateAction(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("approve status = %d, body=%s", w.Code, w.Body.String())
	}

	// cutWorker only starts in RunUntilDone, which these tests never call, so
	// the job is still sitting in the queue to be inspected.
	job := <-srv.cutQueue
	if len(job.formats) != 1 || job.formats[0] != cutter.FormatShort {
		t.Errorf("queued formats = %v, want only [short]", job.formats)
	}
}

func TestApproveWithNoFormatsIsRejectedAndLeavesCandidatePending(t *testing.T) {
	// Approving while asking for no files would mark the candidate done
	// without ever producing one — it must fail before the status flips.
	srv, st, id := newTestServer(t)

	body, _ := json.Marshal(map[string][]string{"formats": {}})
	req := httptest.NewRequest(http.MethodPost, "/api/candidates/"+itoa(id)+"/approve", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleCandidateAction(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an empty format selection", w.Code)
	}

	pending, _ := st.ListCandidatesByStatus("sess1", "pending_review")
	if len(pending) != 1 {
		t.Errorf("expected the candidate to stay pending, got %d pending", len(pending))
	}
}

func TestApproveRejectsUnknownFormat(t *testing.T) {
	srv, st, id := newTestServer(t)

	body, _ := json.Marshal(map[string][]string{"formats": {"clip", "gif"}})
	req := httptest.NewRequest(http.MethodPost, "/api/candidates/"+itoa(id)+"/approve", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleCandidateAction(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unknown format", w.Code)
	}
	pending, _ := st.ListCandidatesByStatus("sess1", "pending_review")
	if len(pending) != 1 {
		t.Errorf("expected the candidate to stay pending, got %d pending", len(pending))
	}
}

func TestRejectQueuesNoRender(t *testing.T) {
	// The point of rejecting is that it costs nothing: no encode is queued.
	srv, _, id := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/candidates/"+itoa(id)+"/reject", nil)
	w := httptest.NewRecorder()
	srv.handleCandidateAction(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("reject status = %d", w.Code)
	}
	if n := len(srv.cutQueue); n != 0 {
		t.Errorf("rejecting queued %d render job(s), want 0", n)
	}
}

func TestRejectStoresReason(t *testing.T) {
	srv, st, id := newTestServer(t)

	body, _ := json.Marshal(map[string]string{"reason": "not actually a highlight"})
	req := httptest.NewRequest(http.MethodPost, "/api/candidates/"+itoa(id)+"/reject", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleCandidateAction(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("reject status = %d, body=%s", w.Code, w.Body.String())
	}

	rejected, _ := st.ListCandidatesByStatus("sess1", "rejected")
	if len(rejected) != 1 {
		t.Fatalf("expected 1 rejected, got %d", len(rejected))
	}
	if rejected[0].RejectReason != "not actually a highlight" {
		t.Errorf("RejectReason = %q, want the submitted reason", rejected[0].RejectReason)
	}
}

func TestHandleCandidateActionUnknownVerbNotFound(t *testing.T) {
	srv, _, id := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/candidates/"+itoa(id)+"/frobnicate", nil)
	w := httptest.NewRecorder()
	srv.handleCandidateAction(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for unknown action", w.Code)
	}
}

func TestCandToScoringDecodesSignals(t *testing.T) {
	c := store.Candidate{
		ID: 7, Archetype: "ace", Start: 8, End: 103, DetScore: 3.38,
		SignalsFired: json.RawMessage(`[{"Kind":"ace_banner","T":100,"Confidence":0.9}]`),
	}
	got := candToScoring(c)
	if got.Archetype != "ace" || got.Start != 8 || got.End != 103 || got.Score != 3.38 {
		t.Errorf("scalar fields mismatch: %+v", got)
	}
	if len(got.SignalsFired) != 1 || got.SignalsFired[0].Kind != "ace_banner" {
		t.Fatalf("signals not decoded: %+v", got.SignalsFired)
	}
}

func TestCandToScoringToleratesBadSignals(t *testing.T) {
	// A malformed signals blob must degrade to "no signals", not panic — the
	// clip should still be cuttable.
	c := store.Candidate{ID: 9, Archetype: "manual", Start: 1, End: 5, SignalsFired: json.RawMessage(`not json`)}
	got := candToScoring(c)
	if len(got.SignalsFired) != 0 {
		t.Errorf("expected no signals from bad JSON, got %+v", got.SignalsFired)
	}
}

func TestMetadataForNoKeyNoStoredMetadataReturnsNil(t *testing.T) {
	// Without a key and without a previously stored verdict, metadata is nil
	// (the clip is cut without a title/thumbnail) rather than blocking on the LLM.
	srv, _, _ := newTestServer(t) // constructed with apiKey ""
	c := store.Candidate{ID: 42, Archetype: "ace", Start: 8, End: 103}
	if meta := srv.metadataFor(42, c); meta != nil {
		t.Errorf("expected nil metadata with no key and no stored verdict, got %+v", meta)
	}
}

func TestMetadataForReusesStoredVerdict(t *testing.T) {
	// A verdict persisted by an earlier run is reused verbatim — no LLM call,
	// even though this server has no key.
	srv, _, _ := newTestServer(t)
	c := store.Candidate{ID: 1, Archetype: "ace", Start: 8, End: 103,
		LLMMetadataJSON: json.RawMessage(`{"title":"Clean Ace","is_highlight":true}`)}
	meta := srv.metadataFor(1, c)
	if meta == nil || meta.Title != "Clean Ace" || !meta.IsHighlight {
		t.Fatalf("expected stored metadata reused, got %+v", meta)
	}
}
