package store

import (
	"encoding/json"
	"errors"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestRunJobCachesOnSameKey(t *testing.T) {
	s := openTestStore(t)
	calls := 0
	fn := func() (json.RawMessage, error) {
		calls++
		return json.RawMessage(`{"n":1}`), nil
	}

	r1, err := s.RunJob("sess1", "detect", "hash-a", fn)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	r2, err := s.RunJob("sess1", "detect", "hash-a", fn)
	if err != nil {
		t.Fatalf("RunJob (second): %v", err)
	}
	if calls != 1 {
		t.Errorf("expected fn to run once (cache hit on second call), ran %d times", calls)
	}
	if string(r1) != string(r2) {
		t.Errorf("cached result mismatch: %s vs %s", r1, r2)
	}
}

func TestRunJobRecomputesOnDifferentConfigHash(t *testing.T) {
	s := openTestStore(t)
	calls := 0
	fn := func() (json.RawMessage, error) {
		calls++
		return json.RawMessage(`{}`), nil
	}
	s.RunJob("sess1", "detect", "hash-a", fn)
	s.RunJob("sess1", "detect", "hash-b", fn) // different config_hash — must recompute
	if calls != 2 {
		t.Errorf("expected 2 calls (different config hash = different job identity), got %d", calls)
	}
}

func TestRunJobDoesNotCacheFailures(t *testing.T) {
	s := openTestStore(t)
	calls := 0
	fn := func() (json.RawMessage, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("boom")
		}
		return json.RawMessage(`{"ok":true}`), nil
	}
	if _, err := s.RunJob("sess1", "detect", "hash-a", fn); err == nil {
		t.Fatal("expected first call to return the injected error")
	}
	result, err := s.RunJob("sess1", "detect", "hash-a", fn)
	if err != nil {
		t.Fatalf("expected retry after failure to succeed, got: %v", err)
	}
	if calls != 2 {
		t.Errorf("expected a failed job to be retried (not cached), got %d calls", calls)
	}
	if string(result) != `{"ok":true}` {
		t.Errorf("unexpected result: %s", result)
	}
}

func TestHashConfigIsDeterministicAndSensitiveToChanges(t *testing.T) {
	h1, err := HashConfig(map[string]int{"threshold": 5})
	if err != nil {
		t.Fatalf("HashConfig: %v", err)
	}
	h2, _ := HashConfig(map[string]int{"threshold": 5})
	h3, _ := HashConfig(map[string]int{"threshold": 6})
	if h1 != h2 {
		t.Errorf("expected identical input to hash identically: %s vs %s", h1, h2)
	}
	if h1 == h3 {
		t.Errorf("expected different input to hash differently, both got %s", h1)
	}
}

func TestCandidateReviewLifecycle(t *testing.T) {
	s := openTestStore(t)
	if err := s.UpsertSession(Session{ID: "sess1", Game: "valo"}); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}

	id, err := s.InsertCandidate(Candidate{
		SessionID: "sess1", Archetype: "ace", Start: 8, End: 103, DetScore: 3.38,
		SignalsFired: json.RawMessage(`[{"kind":"ace_banner"}]`),
	})
	if err != nil {
		t.Fatalf("InsertCandidate: %v", err)
	}

	pending, err := s.ListCandidatesByStatus("sess1", "pending_review")
	if err != nil {
		t.Fatalf("ListCandidatesByStatus: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending candidate, got %d", len(pending))
	}

	if err := s.SetCandidateLLMVerdict(id, true, 0.9, "Ace!", "clean ace", []byte(`{"score":0.9}`)); err != nil {
		t.Fatalf("SetCandidateLLMVerdict: %v", err)
	}
	if err := s.SetCandidateReviewStatus(id, "approved", ""); err != nil {
		t.Fatalf("SetCandidateReviewStatus: %v", err)
	}

	approved, err := s.ListCandidatesByStatus("sess1", "approved")
	if err != nil {
		t.Fatalf("ListCandidatesByStatus(approved): %v", err)
	}
	if len(approved) != 1 {
		t.Fatalf("expected 1 approved candidate, got %d", len(approved))
	}
	if !approved[0].LLMIsHighlight.Bool || approved[0].LLMTitle.String != "Ace!" {
		t.Errorf("LLM verdict not persisted correctly: %+v", approved[0])
	}

	stillPending, err := s.ListCandidatesByStatus("sess1", "pending_review")
	if err != nil {
		t.Fatalf("ListCandidatesByStatus(pending_review after approve): %v", err)
	}
	if len(stillPending) != 0 {
		t.Errorf("expected 0 still pending after approval, got %d", len(stillPending))
	}
}
