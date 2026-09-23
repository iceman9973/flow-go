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
	"errors"
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

// addedColumns are columns introduced after the first release.
//
// They deliberately do NOT live in schema.sql as ALTER statements. migrate runs
// on every Open, and ALTER TABLE ADD COLUMN is not idempotent: the second run
// fails with "duplicate column name" and the process refuses to start. A
// database created before a column existed gets it added here, guarded by
// PRAGMA table_info; a fresh one already has it from the CREATE TABLE, so this
// is a no-op for it.
//
// The table and column names are compile-time constants, never caller input, so
// interpolating them into the PRAGMA and ALTER is safe — SQLite does not accept
// bound parameters in either position.
var addedColumns = []struct{ table, column, decl string }{
	{"accounts", "identity_key", "TEXT"},
	{"accounts", "identity_source", "TEXT"},
	{"accounts", "superseded_by", "TEXT"},
	{"accounts", "last_authuser", "INTEGER"},
	{"accounts", "sapisid_fingerprint", "TEXT"},
}

func migrate(db *sql.DB) error {
	// modernc.org/sqlite executes a multi-statement script when it is passed as
	// a single Exec, but it stops at the first error, so run statements
	// individually to get a useful message on failure.
	for _, stmt := range splitStatements(schemaSQL) {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("store: migration failed on %q: %w", firstLine(stmt), err)
		}
	}

	for _, c := range addedColumns {
		if err := ensureColumn(db, c.table, c.column, c.decl); err != nil {
			return err
		}
	}

	// Indexes on columns that addedColumns may have just created. These cannot
	// live in schema.sql: that script runs first, and an index on a column a
	// pre-existing table does not have yet fails the whole migration — which
	// would make the engine unable to open its own database after an upgrade.
	for _, idx := range postColumnIndexes {
		if _, err := db.Exec(idx); err != nil {
			return fmt.Errorf("store: index after column migration: %w", err)
		}
	}

	return backfillIdentity(db)
}

// postColumnIndexes are created after addedColumns has run, for the reason given
// on the loop above.
var postColumnIndexes = []string{
	`CREATE INDEX IF NOT EXISTS idx_accounts_identity ON accounts(identity_key)`,
}

// ensureColumn adds a column when it is missing and does nothing when it is
// already present, which is what makes repeated migrations safe.
func ensureColumn(db *sql.DB, table, column, decl string) error {
	present, err := hasColumn(db, table, column)
	if err != nil {
		return err
	}
	if present {
		return nil
	}
	if _, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, decl)); err != nil {
		return fmt.Errorf("store: add %s.%s: %w", table, column, err)
	}
	return nil
}

// hasColumn reports whether a table already has a column.
func hasColumn(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, fmt.Errorf("store: table_info(%s): %w", table, err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid, notNull, pk int
			name, columnType string
			defaultValue     sql.NullString
		)
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			return false, fmt.Errorf("store: scan table_info(%s): %w", table, err)
		}
		if strings.EqualFold(name, column) {
			return true, nil
		}
	}
	return false, rows.Err()
}

// backfillIdentity seeds the identity columns for rows written before they
// existed.
//
// The old identity *was* the jar hash, so that is the honest starting anchor for
// those rows — labelled legacy-jar-hash rather than cookie-core, because calling
// it a core anchor would be a claim this code cannot support. The authuser index
// is seeded to 0 because every row this database holds came from a browser with
// a single account signed in at index 0. Leaving it NULL would hide those rows
// from the re-anchor rule and guarantee a fresh duplicate on the next boot.
func backfillIdentity(db *sql.DB) error {
	_, err := db.Exec(`
		UPDATE accounts
		   SET identity_key    = COALESCE(identity_key, 'legacy:' || COALESCE(cookie_hash, account_id)),
		       identity_source = COALESCE(identity_source, 'legacy-jar-hash'),
		       last_authuser   = COALESCE(last_authuser, 0)
		 WHERE identity_key IS NULL
		    OR identity_source IS NULL
		    OR last_authuser IS NULL
	`)
	if err != nil {
		return fmt.Errorf("store: backfill identity columns: %w", err)
	}
	return nil
}

// splitStatements splits a SQL script on statement boundaries.
//
// This is not a plain strings.Split on ";" because a semicolon inside a line
// comment or a string literal is not a boundary. Getting that wrong is not
// cosmetic: the fragment left behind is handed to SQLite, which rejects it as
// "incomplete input" and the process refuses to start. The schema comments are
// English prose and do contain semicolons.
func splitStatements(script string) []string {
	var (
		out     []string
		current strings.Builder
		inLine  bool // inside a -- comment
		inQuote bool // inside a '...' literal
	)

	for i := 0; i < len(script); i++ {
		ch := script[i]

		if inLine {
			current.WriteByte(ch)
			if ch == '\n' {
				inLine = false
			}
			continue
		}

		if inQuote {
			current.WriteByte(ch)
			if ch == '\'' {
				// A doubled quote is an escaped quote, not the end of the
				// literal.
				if i+1 < len(script) && script[i+1] == '\'' {
					current.WriteByte('\'')
					i++
					continue
				}
				inQuote = false
			}
			continue
		}

		switch {
		case ch == '-' && i+1 < len(script) && script[i+1] == '-':
			inLine = true
			current.WriteByte(ch)
		case ch == '\'':
			inQuote = true
			current.WriteByte(ch)
		case ch == ';':
			if stmt := strings.TrimSpace(current.String()); stmt != "" && !isOnlyComments(stmt) {
				out = append(out, stmt)
			}
			current.Reset()
		default:
			current.WriteByte(ch)
		}
	}

	if stmt := strings.TrimSpace(current.String()); stmt != "" && !isOnlyComments(stmt) {
		out = append(out, stmt)
	}
	return out
}

// isOnlyComments reports whether a chunk carries no SQL at all.
//
// A trailing block of comments is a perfectly ordinary thing for a schema file
// to end with, and SQLite treats a bare comment as an incomplete statement
// rather than as nothing to do.
func isOnlyComments(chunk string) bool {
	for _, line := range strings.Split(chunk, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		return false
	}
	return true
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
// Account is one tracked signed-in account.
//
// The JSON tags are part of a wire contract, not decoration: this struct is
// serialised directly by `flow-go export`, so without them it emitted Go field
// names — `AccountID`, `CookieHash`, `SapisidFingerprint` — while everything
// else this tool writes is snake_case. Nothing in this repo unmarshals into this
// type, which is why the tags are safe to add rather than a format change on both
// sides of a round trip.
type Account struct {
	AccountID  string `json:"account_id"`
	CookieHash string `json:"cookie_hash"`
	SKU        string `json:"sku"`
	// Credits is a pointer so a balance that could not be read is distinguishable
	// from a genuine zero. It is emitted without omitempty for the same reason:
	// `null` says "not read" and `0` says "empty", and hiding one of them would
	// put the two back to being the same value.
	Credits          *int       `json:"credits"`
	CreditsCheckedAt *time.Time `json:"credits_checked_at,omitempty"`
	Status           string     `json:"status"`
	LastError        string     `json:"last_error,omitempty"`
	TotalRequests    int64      `json:"total_requests"`
	TotalFailures    int64      `json:"total_failures"`
	CreatedAt        time.Time  `json:"created_at"`
	LastUsedAt       *time.Time `json:"last_used_at,omitempty"`

	// Identity. IdentityKey is the anchor the account is matched on, which is
	// what stays put when the cookies rotate. LastAuthuser is a pointer because
	// index 0 is a real value and must be distinguishable from "not supplied" —
	// otherwise a balance update would rewrite it.
	IdentityKey    string `json:"identity_key"`
	IdentitySource string `json:"identity_source"`
	// SupersededBy is set when this row was folded into another by the re-anchor
	// rule. Empty on a live row, so it is omitted rather than sent as "".
	SupersededBy       string `json:"superseded_by,omitempty"`
	LastAuthuser       *int   `json:"last_authuser"`
	SapisidFingerprint string `json:"sapisid_fingerprint,omitempty"`
}

// UpsertAccount records or refreshes an account row.
//
// Every column here is a partial update: a caller that supplies nothing for one
// leaves the stored value alone. Three of the six guard themselves with
// COALESCE(NULLIF(...)) and cookie_hash did not — it was assigned
// unconditionally, so any caller that omitted it wiped the hash.
//
// That matters because the hash is the account's identity: Bootstrap writes
// jar.Hash() and derives the account id from it. A writer that passes an empty
// hash — Engine.RefreshCredits does, since it is only updating a balance —
// would erase it and leave the row pointing at nothing. The guard is the same
// one the neighbouring columns already use.
func (s *Store) UpsertAccount(a Account) error {
	_, err := s.db.Exec(`
		INSERT INTO accounts (account_id, cookie_hash, sku, credits, credits_checked_at, status, last_error,
		                      identity_key, identity_source, superseded_by, last_authuser, sapisid_fingerprint)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(account_id) DO UPDATE SET
			cookie_hash        = COALESCE(NULLIF(excluded.cookie_hash, ''), accounts.cookie_hash),
			sku                = COALESCE(NULLIF(excluded.sku, ''), accounts.sku),
			credits            = COALESCE(excluded.credits, accounts.credits),
			credits_checked_at = COALESCE(excluded.credits_checked_at, accounts.credits_checked_at),
			status             = excluded.status,
			last_error         = excluded.last_error,
			identity_key        = COALESCE(NULLIF(excluded.identity_key, ''), accounts.identity_key),
			identity_source     = COALESCE(NULLIF(excluded.identity_source, ''), accounts.identity_source),
			superseded_by       = COALESCE(NULLIF(excluded.superseded_by, ''), accounts.superseded_by),
			last_authuser       = COALESCE(excluded.last_authuser, accounts.last_authuser),
			sapisid_fingerprint = COALESCE(NULLIF(excluded.sapisid_fingerprint, ''), accounts.sapisid_fingerprint)
	`, a.AccountID, a.CookieHash, a.SKU, a.Credits, a.CreditsCheckedAt, defaultStatus(a.Status), a.LastError,
		a.IdentityKey, a.IdentitySource, a.SupersededBy, a.LastAuthuser, a.SapisidFingerprint)
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
		       status, COALESCE(last_error,''), total_requests, total_failures, created_at, last_used_at,
		       COALESCE(identity_key,''), COALESCE(identity_source,''), COALESCE(superseded_by,''),
		       last_authuser, COALESCE(sapisid_fingerprint,'')
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
		var credits, lastAuthuser sql.NullInt64
		var checkedAt, lastUsed, createdAt sql.NullString
		if err := rows.Scan(&a.AccountID, &a.CookieHash, &a.SKU, &credits, &checkedAt,
			&a.Status, &a.LastError, &a.TotalRequests, &a.TotalFailures, &createdAt, &lastUsed,
			&a.IdentityKey, &a.IdentitySource, &a.SupersededBy, &lastAuthuser,
			&a.SapisidFingerprint); err != nil {
			return nil, err
		}
		if credits.Valid {
			v := int(credits.Int64)
			a.Credits = &v
		}
		if lastAuthuser.Valid {
			v := int(lastAuthuser.Int64)
			a.LastAuthuser = &v
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
 * Identity
 * ------------------------------------------------------------------ */

// AccountRef is the minimum an identity decision needs about an existing row.
//
// A deliberately small type rather than a half-filled Account: everything here
// is populated, so a caller cannot mistake an omitted field for a stored empty
// one.
// AccountRef is the minimum an identity decision needs about an existing row.
//
// Tagged to match Account even though nothing serialises it today: it is the
// other half of the same identity record, and leaving one of a pair untagged is
// how the next endpoint that returns it inherits the same bug.
type AccountRef struct {
	AccountID          string `json:"account_id"`
	IdentityKey        string `json:"identity_key"`
	SapisidFingerprint string `json:"sapisid_fingerprint"`
}

// AccountByAnchor returns the account already recorded for an identity anchor.
func (s *Store) AccountByAnchor(key string) (string, bool, error) {
	if key == "" {
		return "", false, nil
	}
	var accountID string
	err := s.db.QueryRow(`SELECT account_id FROM accounts WHERE identity_key = ?`, key).Scan(&accountID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return accountID, true, nil
}

// MostRecentActiveAccount returns the active row last seen at an authuser index.
//
// It is the candidate a rotated anchor re-anchors onto. Ordering is by last use
// and then creation, so a row that has actually served work wins over one that
// was only ever written at boot.
func (s *Store) MostRecentActiveAccount(index int) (AccountRef, bool, error) {
	var ref AccountRef
	err := s.db.QueryRow(`
		SELECT account_id, COALESCE(identity_key,''), COALESCE(sapisid_fingerprint,'')
		  FROM accounts
		 WHERE status = 'active' AND COALESCE(last_authuser, 0) = ?
		 ORDER BY COALESCE(last_used_at, created_at) DESC, created_at DESC
		 LIMIT 1
	`, index).Scan(&ref.AccountID, &ref.IdentityKey, &ref.SapisidFingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return AccountRef{}, false, nil
	}
	if err != nil {
		return AccountRef{}, false, err
	}
	return ref, true, nil
}

// ReAnchorAccount moves an account onto a new anchor while keeping its id, and
// so keeps its counters, balances and history attached.
//
// The anchor being left is recorded in account_anchors first, so the move is
// auditable after the fact — the old key is never simply overwritten.
func (s *Store) ReAnchorAccount(accountID, key, cookieHash, fingerprint, source string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var previous string
	if err := tx.QueryRow(
		`SELECT COALESCE(identity_key,'') FROM accounts WHERE account_id = ?`, accountID,
	).Scan(&previous); err != nil {
		return err
	}

	if _, err := tx.Exec(`
		INSERT INTO account_anchors (account_id, identity_key, cookie_hash, source)
		VALUES (?, ?, NULL, 'rotated-from')
		ON CONFLICT(account_id, identity_key) DO UPDATE SET last_seen = CURRENT_TIMESTAMP
	`, accountID, previous); err != nil {
		return err
	}

	if _, err := tx.Exec(`
		INSERT INTO account_anchors (account_id, identity_key, cookie_hash, source)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(account_id, identity_key) DO UPDATE SET
			cookie_hash = COALESCE(NULLIF(excluded.cookie_hash, ''), account_anchors.cookie_hash),
			last_seen   = CURRENT_TIMESTAMP
	`, accountID, key, cookieHash, source); err != nil {
		return err
	}

	if _, err := tx.Exec(`
		UPDATE accounts
		   SET identity_key        = ?,
		       identity_source     = ?,
		       cookie_hash         = COALESCE(NULLIF(?, ''), cookie_hash),
		       sapisid_fingerprint = COALESCE(NULLIF(?, ''), sapisid_fingerprint)
		 WHERE account_id = ?
	`, key, source, cookieHash, fingerprint, accountID); err != nil {
		return err
	}

	return tx.Commit()
}

// RecordAnchor notes one anchor/hash pair for an account.
//
// Called on the ordinary path too, not only on a rotation, so the table holds a
// complete account of which anchors and jars have been seen — which is what
// makes a later merge decision checkable rather than a matter of trust.
func (s *Store) RecordAnchor(accountID, key, cookieHash, source string) error {
	if accountID == "" || key == "" {
		return nil
	}
	_, err := s.db.Exec(`
		INSERT INTO account_anchors (account_id, identity_key, cookie_hash, source)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(account_id, identity_key) DO UPDATE SET
			cookie_hash = COALESCE(NULLIF(excluded.cookie_hash, ''), account_anchors.cookie_hash),
			last_seen   = CURRENT_TIMESTAMP
	`, accountID, key, cookieHash, source)
	return err
}

/* ------------------------------------------------------------------ *
 * Generations
 * ------------------------------------------------------------------ */

// Generation is one submitted job.
//
// The JSON tags are the wire contract for the `generations` array of `flow-go
// export`. Without them a struct field serialises with its Go field name, so the
// export emitted `JobID` and `MediaIDs` next to the snake_case everything else
// uses — a consumer reading `job_id` got nothing, silently.
//
// Two rules decide which fields carry omitempty, and they are the same rules
// store.Account follows:
//
//   - `error` is omitted when empty, because "" there is the *absence of a note* rather
//     than a value. Same reasoning as `last_error` on an account.
//   - Everything else stays present even when empty, because its emptiness is a fact
//     about the row that a consumer should be able to read. `aspect` is empty on every
//     row in practice and still ships as `""` — dropping it would make "no aspect
//     recorded" indistinguishable from "this server predates the field".
//
// Pointers never take omitempty. `credits_spent` is a pointer precisely so a NULL in the
// database — "not recorded", which is what every image row holds — survives as `null`
// instead of collapsing into `0`, which is a real cost. `elapsed_ms` is the same.
type Generation struct {
	JobID        string   `json:"job_id"`
	AccountID    string   `json:"account_id"`
	Kind         string   `json:"kind"`
	Prompt       string   `json:"prompt"`
	Model        string   `json:"model"`
	Duration     int      `json:"duration"`
	Aspect       string   `json:"aspect"`
	Count        int      `json:"count"`
	MediaIDs     []string `json:"media_ids"`
	Status       string   `json:"status"`
	CreditsSpent *int     `json:"credits_spent"`
	ElapsedMS    *int64   `json:"elapsed_ms"`
	Error        string   `json:"error,omitempty"`
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
//
// accountID is optional and fills in the account the job actually ran on. The
// row is inserted at submit time, before the pool has picked a worker, so the
// account is genuinely unknown then — which is why every generations row used to
// carry an empty account_id and per-account history was impossible. The guard is
// the same COALESCE(NULLIF(...)) the neighbouring writers use, so a caller with
// nothing to add leaves whatever is already recorded alone.
func (s *Store) FinishGeneration(jobID, status string, creditsSpent *int, elapsedMS int64, errMessage, accountID string) error {
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
		       account_id    = COALESCE(NULLIF(?, ''), account_id),
		       finished_at   = CURRENT_TIMESTAMP
		 WHERE job_id = ?
	`, status, creditsSpent, elapsedMS, errVal, accountID, jobID)
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
//
// Tagged for the same reason as Generation: the `media` array of `flow-go export`
// marshals this struct directly, and untagged it emitted `MediaID`, `FileName`,
// `FilePath` and `GenerationID`.
//
// No field here takes omitempty, including the ones that are empty on every row today
// (`resolution`). Each empty value is a fact about the asset — not downloaded yet, URL
// unknown — and a consumer is better served by reading the empty field than by having to
// infer its absence. `generation_id` is a pointer so an unlinked asset reads as `null`
// rather than `0`, which is a real row ID.
type Media struct {
	GenerationID *int64 `json:"generation_id"`
	MediaID      string `json:"media_id"`
	Kind         string `json:"kind"`
	Prompt       string `json:"prompt"`
	FileName     string `json:"file_name"`
	FilePath     string `json:"file_path"`
	URL          string `json:"url"`
	Resolution   string `json:"resolution"`
	Bytes        int64  `json:"bytes"`
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
 * Settings
 * ------------------------------------------------------------------ */

// SettingKeyAccountIndex names the signed-in Google account the engine acts as.
//
// It is stored because it is a deliberate operator choice with a real cost
// attached — accounts hold different balances, and a restart that quietly
// returns to the first one can spend a test run against an account with nothing
// left on it.
const SettingKeyAccountIndex = "account_index"

// The browser identity and the page tokens used to be settings here —
// `browser_fingerprint` and `page_tokens` — and are not any more. Neither is
// engine-wide: a fingerprint is the identity one signed-in profile's captcha
// tokens are valid for, and page tokens belong to the page that carried them,
// so a single row was the wrong shape the moment two accounts had to coexist.
// Both live in the account's own bundle now, written by the bridge on every
// sync, and are read from there. An older database may still hold the rows;
// nothing reads them, and nothing needs to delete them.

// Setting reads a stored value.
//
// The bool is false when the key has never been written, which is the normal
// state on a fresh database and is not an error. Only a real failure to read
// comes back as an error, so a caller can tell "unset" from "unreadable" — the
// difference between using a default and not knowing what the value is.
func (s *Store) Setting(key string) (string, bool, error) {
	var value string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: read setting %q: %w", key, err)
	}
	return value, true, nil
}

// SetSetting stores a value, replacing any previous one.
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`
		INSERT INTO settings (key, value, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP
	`, key, value)
	if err != nil {
		return fmt.Errorf("store: write setting %q: %w", key, err)
	}
	return nil
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
	DatabaseFile     string `json:"database_file"`
	Engine           string `json:"engine"`
	TotalGenerations int64  `json:"total_generations"`
	Succeeded        int64  `json:"generations_succeeded"`
	Failed           int64  `json:"generations_failed"`
	InFlight         int64  `json:"generations_in_flight"`
	// Empty counts jobs the transport accepted and produced nothing for.
	//
	// It has to be counted explicitly because it is a fourth status and the
	// other three are summed individually rather than TotalGenerations being
	// reconciled against them. Seven of the fourteen jobs in this database were
	// 'empty', so succeeded + failed + in_flight came to seven and the other
	// half of the table appeared in no bucket at all — which is how a run that
	// was half failing still looked like a run with nothing wrong with it.
	Empty           int64      `json:"generations_empty"`
	VideosGenerated int64      `json:"videos_generated"`
	ImagesGenerated int64      `json:"images_generated"`
	TotalMedia      int64      `json:"total_media"`
	MediaBytes      int64      `json:"media_bytes"`
	TotalRequests   int64      `json:"total_requests"`
	CreditsSpent    int64      `json:"credits_spent"`
	AvgElapsedMS    float64    `json:"avg_elapsed_ms"`
	TrackedAccounts int64      `json:"tracked_accounts"`
	ActiveAccounts  int64      `json:"active_accounts"`
	CreditsKnown    int64      `json:"credits_recorded"`
	FirstGeneration *time.Time `json:"first_generation,omitempty"`
	LastGeneration  *time.Time `json:"last_generation,omitempty"`
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
//
// The status buckets have to cover every value the status column can hold, and
// they did not. The column carries five values — submitted, succeeded, failed,
// empty, ready — and the query named three, so 'empty' and 'ready' both fell
// through. The live database held 15 rows, of which 9 were 'empty' and 1 was
// 'ready', and succeeded+failed+in_flight summed to 5: ten of fifteen jobs were
// in no bucket at all, which is how a run that was two-thirds failing still read
// as one with nothing wrong with it.
//
// 'ready' is counted with 'succeeded' because that is what it is — a video
// submission whose render resolved and was downloaded. It is a later, more
// specific terminal state, not a different outcome.
//
// TestStatsAccountForEveryRow asserts the reconciliation rather than the
// individual numbers, so a sixth status added later fails the test instead of
// silently going missing.
func (s *Store) Stats() (SystemStats, error) {
	stats := SystemStats{DatabaseFile: s.path, Engine: "sqlite (modernc, pure Go, WAL)"}

	row := s.db.QueryRow(`
		SELECT COUNT(*),
		       COALESCE(SUM(status IN ('succeeded', 'ready')), 0),
		       COALESCE(SUM(status = 'failed'), 0),
		       COALESCE(SUM(status = 'submitted'), 0),
		       COALESCE(SUM(status = 'empty'), 0),
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
		&stats.Empty, &stats.VideosGenerated, &stats.ImagesGenerated, &stats.CreditsSpent,
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
