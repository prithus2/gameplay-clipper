package review

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

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

	srv, err := New(st, "sess1", "unused.mp4", "unused_orig.mp4", "valo")
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
