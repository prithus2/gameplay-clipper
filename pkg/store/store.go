// Package store is the pipeline's job/candidate database: SQLite (via
// modernc.org/sqlite, pure Go, no cgo) so a session's state survives a
// crash or a re-run, and re-running after a profile/config change only
// recomputes what actually changed — keyed by (session, stage, config
// hash), not by re-running the whole pipeline from scratch every time.
package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS session (
	id              TEXT PRIMARY KEY,
	game            TEXT NOT NULL,
	input_path      TEXT NOT NULL,
	proxy_path      TEXT,
	duration_s      REAL,
	profile_version INTEGER,
	created_at      TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS job (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id  TEXT NOT NULL,
	stage       TEXT NOT NULL,
	config_hash TEXT NOT NULL,
	status      TEXT NOT NULL, -- queued|running|done|failed
	attempts    INTEGER NOT NULL DEFAULT 0,
	error       TEXT,
	result_json TEXT,
	updated_at  TEXT NOT NULL,
	UNIQUE(session_id, stage, config_hash)
);

CREATE TABLE IF NOT EXISTS candidate (
	id                 INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id         TEXT NOT NULL,
	archetype          TEXT NOT NULL,
	start_s            REAL NOT NULL,
	end_s              REAL NOT NULL,
	det_score          REAL NOT NULL,
	signals_fired_json TEXT,
	llm_is_highlight   INTEGER,
	llm_score          REAL,
	llm_title          TEXT,
	llm_reason         TEXT,
	llm_metadata_json  TEXT,
	status             TEXT NOT NULL DEFAULT 'pending_review', -- pending_review|approved|rejected
	reject_reason      TEXT,
	created_at         TEXT NOT NULL,
	updated_at         TEXT NOT NULL
);
`

type Store struct {
	db *sql.DB
}

// Open creates/migrates the SQLite database at path (use ":memory:" for
// tests) and returns a ready Store.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	db.SetMaxOpenConns(1) // SQLite + WAL is fine single-writer; avoid lock contention
	if _, err := db.Exec("PRAGMA journal_mode=WAL;"); err != nil {
		return nil, fmt.Errorf("enable WAL: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("migrate schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// HashConfig deterministically hashes anything JSON-serializable (a
// profile, a stage's parameters) into a short string suitable as
// config_hash — so a job is only considered "the same" if its inputs
// actually match, and changing a profile's weights or thresholds naturally
// invalidates cached jobs for affected stages.
func HashConfig(v interface{}) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("hash config: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16], nil
}

type Session struct {
	ID             string
	Game           string
	InputPath      string
	ProxyPath      string
	DurationS      float64
	ProfileVersion int
}

func (s *Store) UpsertSession(sess Session) error {
	_, err := s.db.Exec(`
		INSERT INTO session (id, game, input_path, proxy_path, duration_s, profile_version, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			proxy_path=excluded.proxy_path, duration_s=excluded.duration_s, profile_version=excluded.profile_version`,
		sess.ID, sess.Game, sess.InputPath, sess.ProxyPath, sess.DurationS, sess.ProfileVersion, nowRFC3339())
	if err != nil {
		return fmt.Errorf("upsert session: %w", err)
	}
	return nil
}

// RunJob is the idempotency seam: if (sessionID, stage, configHash) already
// completed successfully, its cached result is returned and fn is never
// called. Otherwise fn runs, and its result (or error) is persisted so a
// future run with the same key is a cache hit, not a recompute.
func (s *Store) RunJob(sessionID, stage, configHash string, fn func() (json.RawMessage, error)) (json.RawMessage, error) {
	var existingStatus string
	var existingResult sql.NullString
	err := s.db.QueryRow(
		`SELECT status, result_json FROM job WHERE session_id=? AND stage=? AND config_hash=?`,
		sessionID, stage, configHash,
	).Scan(&existingStatus, &existingResult)
	if err == nil && existingStatus == "done" {
		return json.RawMessage(existingResult.String), nil
	}

	_, err = s.db.Exec(`
		INSERT INTO job (session_id, stage, config_hash, status, attempts, updated_at)
		VALUES (?, ?, ?, 'running', 1, ?)
		ON CONFLICT(session_id, stage, config_hash) DO UPDATE SET
			status='running', attempts=attempts+1, updated_at=excluded.updated_at`,
		sessionID, stage, configHash, nowRFC3339())
	if err != nil {
		return nil, fmt.Errorf("mark job running: %w", err)
	}

	result, runErr := fn()
	if runErr != nil {
		s.db.Exec(`UPDATE job SET status='failed', error=?, updated_at=? WHERE session_id=? AND stage=? AND config_hash=?`,
			runErr.Error(), nowRFC3339(), sessionID, stage, configHash)
		return nil, runErr
	}

	_, err = s.db.Exec(`UPDATE job SET status='done', result_json=?, error=NULL, updated_at=? WHERE session_id=? AND stage=? AND config_hash=?`,
		string(result), nowRFC3339(), sessionID, stage, configHash)
	if err != nil {
		return nil, fmt.Errorf("persist job result: %w", err)
	}
	return result, nil
}

type Candidate struct {
	ID              int64
	SessionID       string
	Archetype       string
	Start           float64
	End             float64
	DetScore        float64
	SignalsFired    json.RawMessage
	LLMIsHighlight  sql.NullBool
	LLMScore        sql.NullFloat64
	LLMTitle        sql.NullString
	LLMReason       sql.NullString
	LLMMetadataJSON json.RawMessage
	Status          string
	RejectReason    string
}

func (s *Store) InsertCandidate(c Candidate) (int64, error) {
	res, err := s.db.Exec(`
		INSERT INTO candidate (session_id, archetype, start_s, end_s, det_score, signals_fired_json, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 'pending_review', ?, ?)`,
		c.SessionID, c.Archetype, c.Start, c.End, c.DetScore, string(c.SignalsFired), nowRFC3339(), nowRFC3339())
	if err != nil {
		return 0, fmt.Errorf("insert candidate: %w", err)
	}
	return res.LastInsertId()
}

func (s *Store) SetCandidateLLMVerdict(id int64, isHighlight bool, score float64, title, reason string, metadataJSON []byte) error {
	_, err := s.db.Exec(`
		UPDATE candidate SET llm_is_highlight=?, llm_score=?, llm_title=?, llm_reason=?, llm_metadata_json=?, updated_at=?
		WHERE id=?`,
		isHighlight, score, title, reason, string(metadataJSON), nowRFC3339(), id)
	if err != nil {
		return fmt.Errorf("set llm verdict: %w", err)
	}
	return nil
}

func (s *Store) SetCandidateReviewStatus(id int64, status, rejectReason string) error {
	_, err := s.db.Exec(`UPDATE candidate SET status=?, reject_reason=?, updated_at=? WHERE id=?`,
		status, rejectReason, nowRFC3339(), id)
	if err != nil {
		return fmt.Errorf("set review status: %w", err)
	}
	return nil
}

func (s *Store) ListCandidatesByStatus(sessionID, status string) ([]Candidate, error) {
	rows, err := s.db.Query(`
		SELECT id, session_id, archetype, start_s, end_s, det_score, signals_fired_json,
		       llm_is_highlight, llm_score, llm_title, llm_reason, llm_metadata_json, status, reject_reason
		FROM candidate WHERE session_id=? AND status=? ORDER BY start_s ASC`, sessionID, status)
	if err != nil {
		return nil, fmt.Errorf("list candidates: %w", err)
	}
	defer rows.Close()

	var out []Candidate
	for rows.Next() {
		var c Candidate
		var signals, meta sql.NullString
		var rejectReason sql.NullString
		if err := rows.Scan(&c.ID, &c.SessionID, &c.Archetype, &c.Start, &c.End, &c.DetScore, &signals,
			&c.LLMIsHighlight, &c.LLMScore, &c.LLMTitle, &c.LLMReason, &meta, &c.Status, &rejectReason); err != nil {
			return nil, fmt.Errorf("scan candidate: %w", err)
		}
		c.SignalsFired = json.RawMessage(signals.String)
		c.LLMMetadataJSON = json.RawMessage(meta.String)
		c.RejectReason = rejectReason.String
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) GetCandidate(id int64) (*Candidate, error) {
	var c Candidate
	var signals, meta, rejectReason sql.NullString
	err := s.db.QueryRow(`
		SELECT id, session_id, archetype, start_s, end_s, det_score, signals_fired_json,
		       llm_is_highlight, llm_score, llm_title, llm_reason, llm_metadata_json, status, reject_reason
		FROM candidate WHERE id=?`, id,
	).Scan(&c.ID, &c.SessionID, &c.Archetype, &c.Start, &c.End, &c.DetScore, &signals,
		&c.LLMIsHighlight, &c.LLMScore, &c.LLMTitle, &c.LLMReason, &meta, &c.Status, &rejectReason)
	if err != nil {
		return nil, fmt.Errorf("get candidate: %w", err)
	}
	c.SignalsFired = json.RawMessage(signals.String)
	c.LLMMetadataJSON = json.RawMessage(meta.String)
	c.RejectReason = rejectReason.String
	return &c, nil
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }
