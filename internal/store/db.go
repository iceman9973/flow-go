// Package store is the SQLite persistence layer.
//
// Two properties are load-bearing here, because the Python version got both
// wrong and the resulting numbers were actively misleading:
//
//  1. Every write comes from the running engine. There is no seeding, no fixture
//     loader, and no path by which test data can reach a production database.
//     Tests call Open with a path inside t.TempDir().
//
//  2. Reported statistics are computed from the rows actually present. If the
//     database is empty, the numbers are zero rather than a plausible-looking
//     total inherited from someone else's unit tests.
package store

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, no cgo
)

//go:embed schema.sql
var schemaSQL string

// Store wraps the database handle.
type Store struct {
	db   *sql.DB
	path string
}

// Open opens (and migrates) the database at path.
//
// Passing ":memory:" gives a private in-memory database, which is what tests
// should use. Passing a file path creates parent directories as needed.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("store: a database path is required")
	}

	if path != ":memory:" && !strings.HasPrefix(path, "file:") {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("store: create data directory: %w", err)
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}

	// A single writer is the correct configuration for SQLite with WAL: it
	// removes SQLITE_BUSY under concurrent generation without any application
	// level locking.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: ping %s: %w", path, err)
	}

	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, err
	}

	return &Store{db: db, path: path}, nil
}

// Close releases the database.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Path returns the database file path.
func (s *Store) Path() string { return s.path }

func migrate(db *sql.DB) error {
	// modernc.org/sqlite executes a multi-statement script when it is passed as
	// a single Exec, but it stops at the first error, so run statements
	// individually to get a useful message on failure.
	for _, stmt := range splitStatements(schemaSQL) {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("store: migration failed on %q: %w", firstLine(stmt), err)
		}
	}
	return nil
}

func splitStatements(script string) []string {
	var out []string
	for _, chunk := range strings.Split(script, ";") {
		trimmed := strings.TrimSpace(chunk)
		if trimmed == "" {
			continue
		}
		out = append(out, trimmed)
	}
	return out
}

func firstLine(stmt string) string {
	if i := strings.IndexByte(stmt, '\n'); i > 0 {
		return stmt[:i]
	}
	if len(stmt) > 60 {
		return stmt[:60]
	}
	return stmt
}

/* ------------------------------------------------------------------ *
 * Accounts
 * ------------------------------------------------------------------ */

// Account is a credential set the pool can route to.
type Account struct {
	AccountID        string
	CookieHash       string
	SKU              string
	Credits          *int
	CreditsCheckedAt *time.Time
	Status           string
	LastError        string
	TotalRequests    int64
	TotalFailures    int64
	CreatedAt        time.Time
	LastUsedAt       *time.Time
}

// UpsertAccount records or refreshes an account row.
func (s *Store) UpsertAccount(a Account) error {
	_, err := s.db.Exec(`
		INSERT INTO accounts (account_id, cookie_hash, sku, credits, credits_checked_at, status, last_error)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(account_id) DO UPDATE SET
			cookie_hash        = excluded.cookie_hash,
			sku                = COALESCE(NULLIF(excluded.sku, ''), accounts.sku),
			credits            = COALESCE(excluded.credits, accounts.credits),
			credits_checked_at = COALESCE(excluded.credits_checked_at, accounts.credits_checked_at),
			status             = excluded.status,
			last_error         = excluded.last_error
	`, a.AccountID, a.CookieHash, a.SKU, a.Credits, a.CreditsCheckedAt, defaultStatus(a.Status), a.LastError)
	return err
}

func defaultStatus(status string) string {
	if status == "" {
		return "active"
	}
	return status
}

// RecordAccountOutcome updates the per-account counters after a job.
func (s *Store) RecordAccountOutcome(accountID string, failed bool, errMessage string) error {
	if accountID == "" {
		return nil
	}
	if failed {
		_, err := s.db.Exec(`
			UPDATE accounts
			   SET total_requests = total_requests + 1,
			       total_failures = total_failures + 1,
			       last_error     = ?,
			       last_used_at   = CURRENT_TIMESTAMP
			 WHERE account_id = ?
		`, errMessage, accountID)
		return err
	}
	_, err := s.db.Exec(`
		UPDATE accounts
		   SET total_requests = total_requests + 1,
		       last_error     = NULL,
		       last_used_at   = CURRENT_TIMESTAMP
		 WHERE account_id = ?
	`, accountID)
	return err
}

// ListAccounts returns every account, newest first.
func (s *Store) ListAccounts() ([]Account, error) {
	rows, err := s.db.Query(`
		SELECT account_id, COALESCE(cookie_hash,''), COALESCE(sku,''), credits, credits_checked_at,
		       status, COALESCE(last_error,''), total_requests, total_failures, created_at, last_used_at
		  FROM accounts
		 ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Account
	for rows.Next() {
		var a Account
		var credits sql.NullInt64
		var checkedAt, lastUsed, createdAt sql.NullString
		if err := rows.Scan(&a.AccountID, &a.CookieHash, &a.SKU, &credits, &checkedAt,
			&a.Status, &a.LastError, &a.TotalRequests, &a.TotalFailures, &createdAt, &lastUsed); err != nil {
			return nil, err
		}
		if credits.Valid {
			v := int(credits.Int64)
			a.Credits = &v
		}
		if checkedAt.Valid {
			if t, ok := parseSQLiteTime(checkedAt.String); ok {
				a.CreditsCheckedAt = &t
			}
		}
		if lastUsed.Valid {
			if t, ok := parseSQLiteTime(lastUsed.String); ok {
				a.LastUsedAt = &t
			}
		}
		if createdAt.Valid {
			if t, ok := parseSQLiteTime(createdAt.String); ok {
				a.CreatedAt = t
			}
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

/* ------------------------------------------------------------------ *
 * Generations
 * ------------------------------------------------------------------ */

// Generation is one submitted job.
type Generation struct {
	JobID        string
	AccountID    string
	Kind         string
	Prompt       string
	Model        string
	Duration     int
	Aspect       string
	Count        int
	MediaIDs     []string
	Status       string
	CreditsSpent *int
	ElapsedMS    *int64
	Error        string
}

// RecordGeneration inserts a job row and returns its row ID.
//
// It is idempotent on job_id so an asynchronous submit path can create the row
// before the worker starts, and the worker can then write it again without a
// unique-constraint error.
func (s *Store) RecordGeneration(g Generation) (int64, error) {
	mediaJSON := "[]"
	if len(g.MediaIDs) > 0 {
		if data, err := json.Marshal(g.MediaIDs); err == nil {
			mediaJSON = string(data)
		}
	}

	_, err := s.db.Exec(`
		INSERT INTO generations
			(job_id, account_id, kind, prompt, model, duration, aspect, count, media_ids, status, credits_spent, elapsed_ms, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(job_id) DO UPDATE SET
			account_id    = COALESCE(NULLIF(excluded.account_id, ''), generations.account_id),
			model         = COALESCE(NULLIF(excluded.model, ''), generations.model),
			aspect        = COALESCE(NULLIF(excluded.aspect, ''), generations.aspect),
			media_ids     = excluded.media_ids,
			status        = excluded.status,
			credits_spent = COALESCE(excluded.credits_spent, generations.credits_spent),
			elapsed_ms    = COALESCE(excluded.elapsed_ms, generations.elapsed_ms),
			error         = excluded.error
	`, g.JobID, g.AccountID, g.Kind, g.Prompt, g.Model, nullInt(g.Duration), g.Aspect,
		defaultCount(g.Count), mediaJSON, defaultGenStatus(g.Status), g.CreditsSpent, g.ElapsedMS, g.Error)
	if err != nil {
		return 0, err
	}

	var rowID int64
	if err := s.db.QueryRow(`SELECT id FROM generations WHERE job_id = ?`, g.JobID).Scan(&rowID); err != nil {
		// Non-fatal: the caller only uses the row ID to link media rows.
		return 0, nil
	}
	return rowID, nil
}

func defaultCount(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

func defaultGenStatus(status string) string {
	if status == "" {
		return "submitted"
	}
	return status
}

func nullInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

// FinishGeneration marks a job terminal.
func (s *Store) FinishGeneration(jobID, status string, creditsSpent *int, elapsedMS int64, errMessage string) error {
	var errVal any
	if errMessage != "" {
		errVal = errMessage
	}
	_, err := s.db.Exec(`
		UPDATE generations
		   SET status        = ?,
		       credits_spent = COALESCE(?, credits_spent),
		       elapsed_ms    = ?,
		       error         = ?,
		       finished_at   = CURRENT_TIMESTAMP
		 WHERE job_id = ?
	`, status, creditsSpent, elapsedMS, errVal, jobID)
	return err
}

// GenerationByJob returns one job.
func (s *Store) GenerationByJob(jobID string) (*Generation, error) {
	var g Generation
	var mediaJSON string
	var duration, credits sql.NullInt64
	var elapsed sql.NullInt64
	var errText sql.NullString

	err := s.db.QueryRow(`
		SELECT job_id, COALESCE(account_id,''), kind, COALESCE(prompt,''), COALESCE(model,''),
		       duration, COALESCE(aspect,''), count, COALESCE(media_ids,'[]'), status,
		       credits_spent, elapsed_ms, error
		  FROM generations WHERE job_id = ?
	`, jobID).Scan(&g.JobID, &g.AccountID, &g.Kind, &g.Prompt, &g.Model, &duration,
		&g.Aspect, &g.Count, &mediaJSON, &g.Status, &credits, &elapsed, &errText)
	if err != nil {
		return nil, err
	}

	if duration.Valid {
		g.Duration = int(duration.Int64)
	}
	if credits.Valid {
		v := int(credits.Int64)
		g.CreditsSpent = &v
	}
	if elapsed.Valid {
		v := elapsed.Int64
		g.ElapsedMS = &v
	}
	if errText.Valid {
		g.Error = errText.String
	}
	_ = json.Unmarshal([]byte(mediaJSON), &g.MediaIDs)
	return &g, nil
}

// RecentGenerations lists the most recent jobs.
func (s *Store) RecentGenerations(limit int) ([]Generation, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`
		SELECT job_id, COALESCE(account_id,''), kind, COALESCE(prompt,''), COALESCE(model,''),
		       COALESCE(duration,0), COALESCE(aspect,''), count, COALESCE(media_ids,'[]'), status,
		       credits_spent, elapsed_ms, COALESCE(error,'')
		  FROM generations
		 ORDER BY id DESC
		 LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Generation
	for rows.Next() {
		var g Generation
		var mediaJSON string
		var credits, elapsed sql.NullInt64
		if err := rows.Scan(&g.JobID, &g.AccountID, &g.Kind, &g.Prompt, &g.Model, &g.Duration,
			&g.Aspect, &g.Count, &mediaJSON, &g.Status, &credits, &elapsed, &g.Error); err != nil {
			return nil, err
		}
		if credits.Valid {
			v := int(credits.Int64)
			g.CreditsSpent = &v
		}
		if elapsed.Valid {
			v := elapsed.Int64
			g.ElapsedMS = &v
		}
		_ = json.Unmarshal([]byte(mediaJSON), &g.MediaIDs)
		out = append(out, g)
	}
	return out, rows.Err()
}

/* ------------------------------------------------------------------ *
 * Media
 * ------------------------------------------------------------------ */

// Media is a produced or downloaded asset.
type Media struct {
	GenerationID *int64
	MediaID      string
	Kind         string
	Prompt       string
	FileName     string
	FilePath     string
	URL          string
	Resolution   string
	Bytes        int64
}

// RecordMedia inserts an asset row.
func (s *Store) RecordMedia(m Media) (int64, error) {
	res, err := s.db.Exec(`
		INSERT INTO media (generation_id, media_id, kind, prompt, file_name, file_path, url, resolution, bytes)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, m.GenerationID, m.MediaID, m.Kind, m.Prompt, m.FileName, m.FilePath, m.URL, m.Resolution, m.Bytes)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// RecentMedia lists the most recent assets.
func (s *Store) RecentMedia(limit int) ([]Media, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`
		SELECT generation_id, COALESCE(media_id,''), kind, COALESCE(prompt,''), COALESCE(file_name,''),
		       COALESCE(file_path,''), COALESCE(url,''), COALESCE(resolution,''), COALESCE(bytes,0)
		  FROM media
		 ORDER BY id DESC
		 LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Media
	for rows.Next() {
		var m Media
		var genID sql.NullInt64
		if err := rows.Scan(&genID, &m.MediaID, &m.Kind, &m.Prompt, &m.FileName,
			&m.FilePath, &m.URL, &m.Resolution, &m.Bytes); err != nil {
			return nil, err
		}
		if genID.Valid {
			v := genID.Int64
			m.GenerationID = &v
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

/* ------------------------------------------------------------------ *
 * Request logs
 * ------------------------------------------------------------------ */

// LogRequest records one upstream call.
func (s *Store) LogRequest(accountID, endpoint string, status int, elapsedMS float64, errMessage string) error {
	var errVal any
	if errMessage != "" {
		errVal = errMessage
	}
	_, err := s.db.Exec(`
		INSERT INTO request_logs (account_id, endpoint, status_code, elapsed_ms, error)
		VALUES (?, ?, ?, ?, ?)
	`, accountID, endpoint, status, elapsedMS, errVal)
	return err
}

/* ------------------------------------------------------------------ *
 * Statistics
 * ------------------------------------------------------------------ */

// SystemStats is the aggregate reported by /stats and `flow-go stats`.
//
// Every field is a real aggregate over the rows present. An empty database
// reports zeros; it never reports a total inherited from test fixtures, which is
// precisely the failure the Python version had.
type SystemStats struct {
	DatabaseFile     string     `json:"database_file"`
	Engine           string     `json:"engine"`
	TotalGenerations int64      `json:"total_generations"`
	Succeeded        int64      `json:"generations_succeeded"`
	Failed           int64      `json:"generations_failed"`
	InFlight         int64      `json:"generations_in_flight"`
	VideosGenerated  int64      `json:"videos_generated"`
	ImagesGenerated  int64      `json:"images_generated"`
	TotalMedia       int64      `json:"total_media"`
	MediaBytes       int64      `json:"media_bytes"`
	TotalRequests    int64      `json:"total_requests"`
	CreditsSpent     int64      `json:"credits_spent"`
	AvgElapsedMS     float64    `json:"avg_elapsed_ms"`
	TrackedAccounts  int64      `json:"tracked_accounts"`
	ActiveAccounts   int64      `json:"active_accounts"`
	CreditsKnown     int64      `json:"credits_recorded"`
	FirstGeneration  *time.Time `json:"first_generation,omitempty"`
	LastGeneration   *time.Time `json:"last_generation,omitempty"`
}

// sqliteTimeLayout is the format SQLite writes for CURRENT_TIMESTAMP.
const sqliteTimeLayout = "2006-01-02 15:04:05"

// parseSQLiteTime converts a datetime column value into a time.Time.
//
// Aggregates such as MIN(created_at) carry no declared column type, so the
// driver hands them back as text even though the underlying column is DATETIME.
// Reading them as time.Time directly fails; parsing the text works regardless of
// which shape the driver chooses.
func parseSQLiteTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{sqliteTimeLayout, time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, value); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// Stats computes the system snapshot.
func (s *Store) Stats() (SystemStats, error) {
	stats := SystemStats{DatabaseFile: s.path, Engine: "sqlite (modernc, pure Go, WAL)"}

	row := s.db.QueryRow(`
		SELECT COUNT(*),
		       COALESCE(SUM(status = 'succeeded'), 0),
		       COALESCE(SUM(status = 'failed'), 0),
		       COALESCE(SUM(status = 'submitted'), 0),
		       COALESCE(SUM(kind = 'video'), 0),
		       COALESCE(SUM(kind = 'image'), 0),
		       COALESCE(SUM(COALESCE(credits_spent, 0)), 0),
		       COALESCE(AVG(elapsed_ms), 0),
		       COALESCE(MIN(created_at), ''),
		       COALESCE(MAX(created_at), '')
		  FROM generations
	`)
	var firstRaw, lastRaw string
	if err := row.Scan(&stats.TotalGenerations, &stats.Succeeded, &stats.Failed, &stats.InFlight,
		&stats.VideosGenerated, &stats.ImagesGenerated, &stats.CreditsSpent,
		&stats.AvgElapsedMS, &firstRaw, &lastRaw); err != nil {
		return stats, err
	}
	if t, ok := parseSQLiteTime(firstRaw); ok {
		stats.FirstGeneration = &t
	}
	if t, ok := parseSQLiteTime(lastRaw); ok {
		stats.LastGeneration = &t
	}

	if err := s.db.QueryRow(`
		SELECT COUNT(*), COALESCE(SUM(COALESCE(bytes, 0)), 0) FROM media
	`).Scan(&stats.TotalMedia, &stats.MediaBytes); err != nil {
		return stats, err
	}

	if err := s.db.QueryRow(`SELECT COUNT(*) FROM request_logs`).Scan(&stats.TotalRequests); err != nil {
		return stats, err
	}

	if err := s.db.QueryRow(`
		SELECT COUNT(*), COALESCE(SUM(status = 'active'), 0), COUNT(credits)
		  FROM accounts
	`).Scan(&stats.TrackedAccounts, &stats.ActiveAccounts, &stats.CreditsKnown); err != nil {
		return stats, err
	}

	return stats, nil
}

// Reset deletes all rows. Exposed for tests and for an explicit operator action;
// it is never called by the engine itself.
func (s *Store) Reset() error {
	for _, table := range []string{"media", "generations", "request_logs", "accounts"} {
		if _, err := s.db.Exec("DELETE FROM " + table); err != nil {
			return fmt.Errorf("store: reset %s: %w", table, err)
		}
	}
	return nil
}
