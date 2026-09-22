package store

import (
	"path/filepath"
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
	if err := st.FinishGeneration("job-1", "succeeded", &credits, 4200, ""); err != nil {
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
