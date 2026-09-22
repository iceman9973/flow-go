package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// openTemp opens a store in a private temporary file.
//
// Every test uses this. The Python version's worker-pool tests wrote directly to
// the production data/flow.db because they had no isolation, which is how
// /stats ended up reporting 1600 credits of fixtures as live account analytics.
// There is no equivalent path here: Open always takes an explicit path.
func openTemp(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "flow.db"))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestEmptyDatabaseReportsZeros is the direct regression test for the reporting
// defect: a database with no engine activity must report zero, not a total
// inherited from someone else's unit tests.
func TestEmptyDatabaseReportsZeros(t *testing.T) {
	st := openTemp(t)

	stats, err := st.Stats()
	if err != nil {
		t.Fatalf("Stats failed: %v", err)
	}

	if stats.TotalGenerations != 0 {
		t.Errorf("TotalGenerations = %d, want 0", stats.TotalGenerations)
	}
	if stats.TotalMedia != 0 {
		t.Errorf("TotalMedia = %d, want 0", stats.TotalMedia)
	}
	if stats.TotalRequests != 0 {
		t.Errorf("TotalRequests = %d, want 0", stats.TotalRequests)
	}
	if stats.TrackedAccounts != 0 {
		t.Errorf("TrackedAccounts = %d, want 0", stats.TrackedAccounts)
	}
	if stats.CreditsKnown != 0 {
		t.Errorf("CreditsKnown = %d, want 0", stats.CreditsKnown)
	}
	if stats.Succeeded != 0 || stats.Failed != 0 || stats.InFlight != 0 {
		t.Errorf("status counts should be zero: %+v", stats)
	}
}

func TestGenerationLifecycle(t *testing.T) {
	st := openTemp(t)

	rowID, err := st.RecordGeneration(Generation{
		JobID:    "job-1",
		Kind:     "video",
		Prompt:   "a paper boat",
		Model:    "abra_t2v_8s",
		Duration: 8,
		Aspect:   "landscape",
		Count:    1,
		Status:   "submitted",
	})
	if err != nil {
		t.Fatalf("RecordGeneration failed: %v", err)
	}
	if rowID == 0 {
		t.Error("expected a non-zero row ID")
	}

	stats, _ := st.Stats()
	if stats.TotalGenerations != 1 {
		t.Errorf("TotalGenerations = %d, want 1", stats.TotalGenerations)
	}
	if stats.InFlight != 1 {
		t.Errorf("InFlight = %d, want 1", stats.InFlight)
	}
	if stats.VideosGenerated != 1 {
		t.Errorf("VideosGenerated = %d, want 1", stats.VideosGenerated)
	}

	credits := 12
	if err := st.FinishGeneration("job-1", "succeeded", &credits, 4200, "", ""); err != nil {
		t.Fatalf("FinishGeneration failed: %v", err)
	}

	job, err := st.GenerationByJob("job-1")
	if err != nil {
		t.Fatalf("GenerationByJob failed: %v", err)
	}
	if job.Status != "succeeded" {
		t.Errorf("status = %q, want succeeded", job.Status)
	}
	if job.CreditsSpent == nil || *job.CreditsSpent != 12 {
		t.Errorf("CreditsSpent = %v, want 12", job.CreditsSpent)
	}
	if job.ElapsedMS == nil || *job.ElapsedMS != 4200 {
		t.Errorf("ElapsedMS = %v, want 4200", job.ElapsedMS)
	}

	stats, _ = st.Stats()
	if stats.Succeeded != 1 || stats.InFlight != 0 {
		t.Errorf("after completion: succeeded=%d in_flight=%d, want 1 and 0", stats.Succeeded, stats.InFlight)
	}
	if stats.CreditsSpent != 12 {
		t.Errorf("CreditsSpent = %d, want 12", stats.CreditsSpent)
	}
	if stats.AvgElapsedMS != 4200 {
		t.Errorf("AvgElapsedMS = %f, want 4200", stats.AvgElapsedMS)
	}
}

// TestRecordGenerationIsIdempotent covers the asynchronous submit path: the
// handler creates the row, then the worker writes it again.
func TestRecordGenerationIsIdempotent(t *testing.T) {
	st := openTemp(t)

	first, err := st.RecordGeneration(Generation{JobID: "job-1", Kind: "video", Prompt: "p", Status: "submitted"})
	if err != nil {
		t.Fatalf("first RecordGeneration failed: %v", err)
	}
	second, err := st.RecordGeneration(Generation{JobID: "job-1", Kind: "video", Prompt: "p", Status: "submitted"})
	if err != nil {
		t.Fatalf("second RecordGeneration failed: %v", err)
	}

	if first != second {
		t.Errorf("re-recording the same job_id should return the same row: %d != %d", first, second)
	}

	stats, _ := st.Stats()
	if stats.TotalGenerations != 1 {
		t.Errorf("TotalGenerations = %d, want 1", stats.TotalGenerations)
	}
}

func TestMediaRecording(t *testing.T) {
	st := openTemp(t)

	rowID, _ := st.RecordGeneration(Generation{JobID: "job-1", Kind: "video", Prompt: "p"})

	_, err := st.RecordMedia(Media{
		GenerationID: &rowID,
		MediaID:      "media-abc",
		Kind:         "video",
		Prompt:       "p",
		FileName:     "media-abc.mp4",
		FilePath:     "/tmp/media-abc.mp4",
		Resolution:   "1080p",
		Bytes:        5 * 1024 * 1024,
	})
	if err != nil {
		t.Fatalf("RecordMedia failed: %v", err)
	}

	stats, _ := st.Stats()
	if stats.TotalMedia != 1 {
		t.Errorf("TotalMedia = %d, want 1", stats.TotalMedia)
	}
	if stats.MediaBytes != 5*1024*1024 {
		t.Errorf("MediaBytes = %d, want %d", stats.MediaBytes, 5*1024*1024)
	}

	list, err := st.RecentMedia(10)
	if err != nil {
		t.Fatalf("RecentMedia failed: %v", err)
	}
	if len(list) != 1 || list[0].MediaID != "media-abc" {
		t.Errorf("unexpected media list: %+v", list)
	}
}

func TestAccountOutcomes(t *testing.T) {
	st := openTemp(t)

	if err := st.UpsertAccount(Account{AccountID: "acct-1", CookieHash: "abc", Status: "active"}); err != nil {
		t.Fatalf("UpsertAccount failed: %v", err)
	}

	// Re-upserting must not duplicate.
	if err := st.UpsertAccount(Account{AccountID: "acct-1", CookieHash: "abc", SKU: "G1_TIER1", Status: "active"}); err != nil {
		t.Fatalf("second UpsertAccount failed: %v", err)
	}

	accounts, err := st.ListAccounts()
	if err != nil {
		t.Fatalf("ListAccounts failed: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("expected 1 account, got %d", len(accounts))
	}
	if accounts[0].SKU != "G1_TIER1" {
		t.Errorf("SKU = %q, want G1_TIER1", accounts[0].SKU)
	}

	_ = st.RecordAccountOutcome("acct-1", false, "")
	_ = st.RecordAccountOutcome("acct-1", true, "upstream 503")

	accounts, _ = st.ListAccounts()
	if accounts[0].TotalRequests != 2 {
		t.Errorf("TotalRequests = %d, want 2", accounts[0].TotalRequests)
	}
	if accounts[0].TotalFailures != 1 {
		t.Errorf("TotalFailures = %d, want 1", accounts[0].TotalFailures)
	}
	if accounts[0].LastError != "upstream 503" {
		t.Errorf("LastError = %q, want 'upstream 503'", accounts[0].LastError)
	}
}

// TestCreditsAreNotInvented checks that an account whose balance has never been
// checked reports NULL rather than a number.
func TestCreditsAreNotInvented(t *testing.T) {
	st := openTemp(t)

	if err := st.UpsertAccount(Account{AccountID: "acct-1", Status: "active"}); err != nil {
		t.Fatal(err)
	}

	accounts, _ := st.ListAccounts()
	if accounts[0].Credits != nil {
		t.Errorf("Credits = %v, want nil for an unchecked account", *accounts[0].Credits)
	}

	stats, _ := st.Stats()
	if stats.CreditsKnown != 0 {
		t.Errorf("CreditsKnown = %d, want 0", stats.CreditsKnown)
	}
}

// TestUpsertAccountKeepsTheCookieHashWhenOmitted pins the partial-update
// contract that cookie_hash used to break.
//
// The hash is the account's identity — Bootstrap writes jar.Hash() and derives
// the account id from it — so a writer that only means to record a balance has
// nothing to say about it and must not clear it. Engine.RefreshCredits passes an
// empty CookieHash for exactly that reason, and because the column was assigned
// unconditionally, that write erased the hash instead of leaving it alone. The
// sibling columns (sku, credits, credits_checked_at) already guarded themselves,
// which is what makes this an oversight rather than a decision.
func TestUpsertAccountKeepsTheCookieHashWhenOmitted(t *testing.T) {
	st := openTemp(t)

	if err := st.UpsertAccount(Account{
		AccountID:  "acct-1",
		CookieHash: "hash-from-bootstrap",
		SKU:        "G1_TIER1",
		Status:     "active",
	}); err != nil {
		t.Fatalf("first UpsertAccount failed: %v", err)
	}

	// A balance-only update: no hash, no sku, nothing about identity.
	credits := 1050
	if err := st.UpsertAccount(Account{
		AccountID: "acct-1",
		Credits:   &credits,
		Status:    "active",
	}); err != nil {
		t.Fatalf("second UpsertAccount failed: %v", err)
	}

	accounts, err := st.ListAccounts()
	if err != nil {
		t.Fatalf("ListAccounts failed: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("expected 1 account, got %d", len(accounts))
	}
	if accounts[0].CookieHash != "hash-from-bootstrap" {
		t.Errorf("CookieHash = %q, want it left alone — an omitted hash must not clear the stored one",
			accounts[0].CookieHash)
	}
	if accounts[0].SKU != "G1_TIER1" {
		t.Errorf("SKU = %q, want it left alone", accounts[0].SKU)
	}
	if accounts[0].Credits == nil || *accounts[0].Credits != 1050 {
		t.Errorf("Credits = %v, want 1050 — the update it was actually for", accounts[0].Credits)
	}
}

// TestUpsertAccountStillOverwritesASuppliedHash checks the guard did not make the
// column read-only: a supplied hash must still overwrite, or a re-signed-in
// account would keep reporting the previous session's hash.
func TestUpsertAccountStillOverwritesASuppliedHash(t *testing.T) {
	st := openTemp(t)

	if err := st.UpsertAccount(Account{AccountID: "acct-1", CookieHash: "old", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAccount(Account{AccountID: "acct-1", CookieHash: "new", Status: "active"}); err != nil {
		t.Fatal(err)
	}

	accounts, _ := st.ListAccounts()
	if accounts[0].CookieHash != "new" {
		t.Errorf("CookieHash = %q, want %q — a supplied hash must still overwrite",
			accounts[0].CookieHash, "new")
	}
}

// TestStoresAreIsolated proves that two stores never share data, which is the
// property the Python tests lacked.
func TestStoresAreIsolated(t *testing.T) {
	a := openTemp(t)
	b := openTemp(t)

	if _, err := a.RecordGeneration(Generation{JobID: "only-in-a", Kind: "video", Prompt: "p"}); err != nil {
		t.Fatal(err)
	}

	statsB, _ := b.Stats()
	if statsB.TotalGenerations != 0 {
		t.Errorf("store B sees %d generations, expected 0 — stores are not isolated", statsB.TotalGenerations)
	}

	statsA, _ := a.Stats()
	if statsA.TotalGenerations != 1 {
		t.Errorf("store A sees %d generations, expected 1", statsA.TotalGenerations)
	}
}

func TestRequestLogging(t *testing.T) {
	st := openTemp(t)

	if err := st.LogRequest("acct-1", "/v1/video:batchAsyncGenerateVideoText", 200, 812.5, ""); err != nil {
		t.Fatalf("LogRequest failed: %v", err)
	}
	if err := st.LogRequest("acct-1", "/v1/credits", 503, 12.0, "NO_FLOW_KEY"); err != nil {
		t.Fatalf("LogRequest failed: %v", err)
	}

	stats, _ := st.Stats()
	if stats.TotalRequests != 2 {
		t.Errorf("TotalRequests = %d, want 2", stats.TotalRequests)
	}
}

func TestReset(t *testing.T) {
	st := openTemp(t)

	_, _ = st.RecordGeneration(Generation{JobID: "job-1", Kind: "video", Prompt: "p"})
	_ = st.UpsertAccount(Account{AccountID: "acct-1"})

	if err := st.Reset(); err != nil {
		t.Fatalf("Reset failed: %v", err)
	}

	stats, _ := st.Stats()
	if stats.TotalGenerations != 0 || stats.TrackedAccounts != 0 {
		t.Errorf("Reset left data behind: %+v", stats)
	}
}

func TestOpenRejectsEmptyPath(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Error("Open with an empty path should fail rather than pick a default")
	}
}

func TestGenerationByJobMissing(t *testing.T) {
	st := openTemp(t)
	if _, err := st.GenerationByJob("nope"); err == nil {
		t.Error("expected an error for a missing job")
	}
}

/* ------------------------------------------------------------------ *
 * Migration
 * ------------------------------------------------------------------ */

// TestOpenIsIdempotentAcrossRestarts pins the property that makes migrate()
// safe to run on every Open.
//
// The engine opens the database on every start, so migrate runs on every start.
// A bare ALTER TABLE ADD COLUMN would therefore succeed once and then fail with
// "duplicate column name" forever after — the engine would be unable to start
// the second time it was ever run. That is the whole reason addedColumns exists
// and is guarded by PRAGMA table_info, and this is the regression test for it.
func TestOpenIsIdempotentAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flow.db")

	for i := 0; i < 4; i++ {
		st, err := Open(path)
		if err != nil {
			t.Fatalf("Open #%d failed — a restart must not fail the migration: %v", i+1, err)
		}
		// Write something on each pass so a later run is migrating a populated
		// database, which is the case that actually matters.
		if err := st.UpsertAccount(Account{AccountID: "acct-1", Status: "active"}); err != nil {
			t.Fatalf("UpsertAccount on pass %d: %v", i+1, err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("Close on pass %d: %v", i+1, err)
		}
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("final Open failed: %v", err)
	}
	defer func() { _ = st.Close() }()

	accounts, err := st.ListAccounts()
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if len(accounts) != 1 {
		t.Errorf("four restarts produced %d account rows, want 1", len(accounts))
	}
}

// TestMigrationUpgradesADatabaseFromBeforeTheIdentityColumns is the upgrade path
// against a database shaped exactly like the one on disk.
//
// This is not hypothetical: the live data/flow.db was created before these
// columns existed. The test builds that old shape directly, inserts a row the
// way the previous release would have, and then opens it with the current code.
//
// It also guards the ordering that broke first. CREATE INDEX on a column a
// pre-existing table does not have yet is not a no-op — SQLite rejects it, and
// because schema.sql runs before the ALTERs, having the identity index there
// made Open fail outright on any older database. An index creation that must
// follow a column addition cannot live in the schema script.
func TestMigrationUpgradesADatabaseFromBeforeTheIdentityColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	// The accounts table as the previous release created it: no identity_key,
	// no identity_source, no superseded_by, no last_authuser, no fingerprint.
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	if _, err := legacy.Exec(`
		CREATE TABLE accounts (
		    account_id          TEXT PRIMARY KEY,
		    cookie_hash         TEXT,
		    sku                 TEXT,
		    credits             INTEGER,
		    credits_checked_at  DATETIME,
		    status              TEXT NOT NULL DEFAULT 'active',
		    last_error          TEXT,
		    total_requests      INTEGER NOT NULL DEFAULT 0,
		    total_failures      INTEGER NOT NULL DEFAULT 0,
		    created_at          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		    last_used_at        DATETIME
		)
	`); err != nil {
		t.Fatalf("create legacy accounts table: %v", err)
	}
	// A row as the old Bootstrap would have written it: id and hash derived from
	// the jar, nothing else.
	if _, err := legacy.Exec(`
		INSERT INTO accounts (account_id, cookie_hash, sku, status)
		VALUES ('acct-8c4f11aa22bb', '8c4f11aa22bb99dd', 'G1_TIER1', 'active')
	`); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	// Now open it with the current code. Before the ordering fix this is where
	// it failed.
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open on a pre-identity database failed: %v", err)
	}
	defer func() { _ = st.Close() }()

	// The columns must have been added, not just the table tolerated.
	for _, column := range []string{
		"identity_key", "identity_source", "superseded_by", "last_authuser", "sapisid_fingerprint",
	} {
		present, err := hasColumn(st.db, "accounts", column)
		if err != nil {
			t.Fatalf("hasColumn(%s): %v", column, err)
		}
		if !present {
			t.Errorf("column %s was not added to the legacy table", column)
		}
	}

	// And the index that could not be created before the columns existed.
	var indexName string
	if err := st.db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type = 'index' AND name = 'idx_accounts_identity'`,
	).Scan(&indexName); err != nil {
		t.Errorf("idx_accounts_identity is missing after migration: %v", err)
	}

	accounts, err := st.ListAccounts()
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("expected the legacy row to survive, got %d rows", len(accounts))
	}

	got := accounts[0]
	if got.AccountID != "acct-8c4f11aa22bb" {
		t.Errorf("account id = %q, want the legacy row untouched", got.AccountID)
	}
	// The backfill must anchor on the old hash and say so, rather than
	// presenting a legacy row as a core-cookie anchor this code never verified.
	if got.IdentityKey != "legacy:8c4f11aa22bb99dd" {
		t.Errorf("identity_key = %q, want legacy:8c4f11aa22bb99dd", got.IdentityKey)
	}
	if got.IdentitySource != "legacy-jar-hash" {
		t.Errorf("identity_source = %q, want legacy-jar-hash", got.IdentitySource)
	}
	// last_authuser has to be 0 rather than NULL. NULL would hide the row from
	// the re-anchor rule, and the next boot would mint a duplicate beside it —
	// which is the churn this change exists to stop.
	if got.LastAuthuser == nil {
		t.Error("last_authuser is NULL; the row is invisible to the re-anchor rule and will be duplicated")
	} else if *got.LastAuthuser != 0 {
		t.Errorf("last_authuser = %d, want 0", *got.LastAuthuser)
	}
	// The legacy row carries no fingerprint, and must not be given a fabricated
	// one: "no evidence" and "proven to match" are different claims.
	if got.SapisidFingerprint != "" {
		t.Errorf("sapisid_fingerprint = %q, want empty for a row that never had one", got.SapisidFingerprint)
	}
}

// TestMigrationBackfillLeavesMigratedRowsAlone checks the backfill is written so
// that it does not keep rewriting a row once it has been migrated.
func TestMigrationBackfillLeavesMigratedRowsAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flow.db")

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := st.UpsertAccount(Account{
		AccountID:          "acct-1",
		IdentityKey:        "core-abc",
		IdentitySource:     "cookie-core",
		SapisidFingerprint: "fingerprint-1",
		Status:             "active",
	}); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	_ = st.Close()

	// A restart must not relabel a row that already has a real anchor.
	st, err = Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	accounts, _ := st.ListAccounts()
	if len(accounts) != 1 {
		t.Fatalf("expected 1 account, got %d", len(accounts))
	}
	if accounts[0].IdentityKey != "core-abc" {
		t.Errorf("identity_key = %q, want core-abc — the backfill overwrote a real anchor",
			accounts[0].IdentityKey)
	}
	if accounts[0].IdentitySource != "cookie-core" {
		t.Errorf("identity_source = %q, want cookie-core", accounts[0].IdentitySource)
	}
	if accounts[0].SapisidFingerprint != "fingerprint-1" {
		t.Errorf("sapisid_fingerprint = %q, want fingerprint-1", accounts[0].SapisidFingerprint)
	}
}

/* ------------------------------------------------------------------ *
 * Account attribution
 * ------------------------------------------------------------------ */

// TestFinishGenerationAttributesTheAccount is the reporting fix.
//
// The row is inserted at submit time, before the pool has picked a worker, so
// account_id is genuinely unknown then. It is only known once the job runs —
// which is when FinishGeneration is called. Every generations row in the live
// database was empty because nothing ever passed it, so per-account history was
// impossible to reconstruct after the fact.
func TestFinishGenerationAttributesTheAccount(t *testing.T) {
	st := openTemp(t)

	if _, err := st.RecordGeneration(Generation{
		JobID: "job-1", Kind: "video", Prompt: "a paper boat", Status: "submitted",
	}); err != nil {
		t.Fatalf("RecordGeneration: %v", err)
	}

	// Submitted, not yet attributed.
	before, err := st.GenerationByJob("job-1")
	if err != nil {
		t.Fatalf("GenerationByJob: %v", err)
	}
	if before.AccountID != "" {
		t.Errorf("account_id = %q at submit time, want empty — the pool has not picked yet", before.AccountID)
	}

	credits := 12
	if err := st.FinishGeneration("job-1", "succeeded", &credits, 4200, "", "acct-8c4f11aa22bb"); err != nil {
		t.Fatalf("FinishGeneration: %v", err)
	}

	after, err := st.GenerationByJob("job-1")
	if err != nil {
		t.Fatalf("GenerationByJob: %v", err)
	}
	if after.AccountID != "acct-8c4f11aa22bb" {
		t.Errorf("account_id = %q, want acct-8c4f11aa22bb — the job was not attributed", after.AccountID)
	}
	if after.Status != "succeeded" {
		t.Errorf("status = %q, want succeeded", after.Status)
	}
}

// TestFinishGenerationKeepsTheAccountWhenOmitted covers the other half of the
// partial-update contract: a caller with nothing to say about the account must
// not erase what is recorded.
//
// The failure paths call FinishGeneration without a resolved account, and an
// unconditional assignment would blank the attribution the success path had
// just written.
func TestFinishGenerationKeepsTheAccountWhenOmitted(t *testing.T) {
	st := openTemp(t)

	if _, err := st.RecordGeneration(Generation{
		JobID: "job-1", Kind: "video", Prompt: "p", Status: "submitted",
		AccountID: "acct-attributed-at-submit",
	}); err != nil {
		t.Fatalf("RecordGeneration: %v", err)
	}

	// A later write that knows nothing about the account.
	if err := st.FinishGeneration("job-1", "failed", nil, 900, "upstream 503", ""); err != nil {
		t.Fatalf("FinishGeneration: %v", err)
	}

	job, err := st.GenerationByJob("job-1")
	if err != nil {
		t.Fatalf("GenerationByJob: %v", err)
	}
	if job.AccountID != "acct-attributed-at-submit" {
		t.Errorf("account_id = %q, want it left alone by a caller that omitted it", job.AccountID)
	}
	if job.Error != "upstream 503" {
		t.Errorf("error = %q, want the failure recorded", job.Error)
	}
}

// TestFinishGenerationStillOverwritesASuppliedAccount checks the guard did not
// make the column read-only: a supplied account must still win, or a retried job
// that landed on a different account would keep reporting the first one.
func TestFinishGenerationStillOverwritesASuppliedAccount(t *testing.T) {
	st := openTemp(t)

	if _, err := st.RecordGeneration(Generation{
		JobID: "job-1", Kind: "video", Prompt: "p", AccountID: "acct-first",
	}); err != nil {
		t.Fatalf("RecordGeneration: %v", err)
	}
	if err := st.FinishGeneration("job-1", "succeeded", nil, 10, "", "acct-second"); err != nil {
		t.Fatalf("FinishGeneration: %v", err)
	}

	job, _ := st.GenerationByJob("job-1")
	if job.AccountID != "acct-second" {
		t.Errorf("account_id = %q, want acct-second — a supplied account must still overwrite", job.AccountID)
	}
}

/* ------------------------------------------------------------------ *
 * Statement splitting
 * ------------------------------------------------------------------ */

// TestSplitStatementsIgnoresSemicolonsInCommentsAndLiterals is a regression test
// for a startup crash, not a formatting preference.
//
// The schema comments are English prose and contain semicolons. A plain
// strings.Split on ";" cuts a statement in half at one of them; the fragment is
// then handed to SQLite, which rejects it as "incomplete input" — and because
// migrate runs on every Open, the engine could not open its own database at all.
func TestSplitStatementsIgnoresSemicolonsInCommentsAndLiterals(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  int
	}{
		{
			name:  "semicolon in a line comment",
			input: "CREATE TABLE a (x TEXT); -- one of two; the other is next\nCREATE TABLE b (y TEXT);",
			want:  2,
		},
		{
			name:  "semicolon in a string literal",
			input: "INSERT INTO a (x) VALUES ('one; two');\nINSERT INTO a (x) VALUES ('three');",
			want:  2,
		},
		{
			name:  "escaped quote inside a literal",
			input: "INSERT INTO a (x) VALUES ('it''s; fine');",
			want:  1,
		},
		{
			name:  "trailing comment block is not a statement",
			input: "CREATE TABLE a (x TEXT);\n-- a trailing note; with a semicolon\n-- and another line\n",
			want:  1,
		},
		{
			name:  "comment only",
			input: "-- nothing to do here; really\n",
			want:  0,
		},
		{
			name:  "blank input",
			input: "   \n\n",
			want:  0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitStatements(tc.input)
			if len(got) != tc.want {
				t.Fatalf("split into %d statements, want %d: %#v", len(got), tc.want, got)
			}
			// A fragment that is not valid SQL is the failure mode this guards,
			// so every piece must at least parse as a statement.
			for _, stmt := range got {
				if isOnlyComments(stmt) {
					t.Errorf("a comment-only chunk was emitted as a statement: %q", stmt)
				}
			}
		})
	}
}

// TestSchemaSplitsIntoExecutableStatements runs the real embedded schema through
// the splitter, which is what actually happens at startup.
func TestSchemaSplitsIntoExecutableStatements(t *testing.T) {
	stmts := splitStatements(schemaSQL)
	if len(stmts) == 0 {
		t.Fatal("the embedded schema split into no statements")
	}
	for _, stmt := range stmts {
		trimmed := strings.TrimSpace(stmt)
		if trimmed == "" {
			t.Error("the schema produced an empty statement")
		}
		if isOnlyComments(trimmed) {
			t.Errorf("the schema produced a comment-only statement: %q", trimmed)
		}
	}
}

// TestStatsAccountForEveryRow is the regression test for the invisible half of
// the empty-result defect.
//
// The status buckets are summed individually rather than reconciled against the
// total, so any status the query does not name appears in no bucket at all.
// 'empty' was not named — and seven of the fourteen jobs in the live database
// were 'empty', so succeeded + failed + in_flight came to seven and a run that
// was half failing looked like a run with nothing wrong with it.
//
// The assertion is the reconciliation rather than the individual numbers:
// whatever the statuses are, they have to add up to the rows that exist.
func TestStatsAccountForEveryRow(t *testing.T) {
	st := openTemp(t)

	// Every value the status column can hold, so a status that stops being
	// counted fails here rather than going missing quietly. 'ready' is the one
	// that caught this out the second time: it is a terminal success, and it was
	// in no bucket either.
	seed := []struct {
		jobID  string
		status string
	}{
		{"job-succeeded", "succeeded"},
		{"job-failed", "failed"},
		{"job-empty", "empty"},
		{"job-submitted", "submitted"},
		{"job-ready", "ready"},
	}
	for _, row := range seed {
		if _, err := st.RecordGeneration(Generation{
			JobID: row.jobID, Kind: "video", Prompt: "p", Status: "submitted",
		}); err != nil {
			t.Fatalf("RecordGeneration %s: %v", row.jobID, err)
		}
		if err := st.FinishGeneration(row.jobID, row.status, nil, 10, "", ""); err != nil {
			t.Fatalf("FinishGeneration %s: %v", row.jobID, err)
		}
	}

	stats, err := st.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}

	if stats.Empty != 1 {
		t.Errorf("Empty = %d, want 1 — an 'empty' job is a failure and has to be counted "+
			"somewhere", stats.Empty)
	}
	// 'ready' is a success, so it belongs in the succeeded count rather than
	// beside it. Two rows carry a terminal success here: 'succeeded' and 'ready'.
	if stats.Succeeded != 2 {
		t.Errorf("Succeeded = %d, want 2 — a 'ready' job is a render that completed and "+
			"was downloaded, which is a success", stats.Succeeded)
	}

	bucketed := stats.Succeeded + stats.Failed + stats.InFlight + stats.Empty
	if bucketed != stats.TotalGenerations {
		t.Errorf("status buckets sum to %d but there are %d generations: %d row(s) are in "+
			"no bucket and invisible to /stats (succeeded=%d failed=%d in_flight=%d empty=%d)",
			bucketed, stats.TotalGenerations, stats.TotalGenerations-bucketed,
			stats.Succeeded, stats.Failed, stats.InFlight, stats.Empty)
	}
}
