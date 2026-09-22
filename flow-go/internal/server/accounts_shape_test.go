package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/kodelyx/flow-go/flow-go/internal/store"
)

/* ------------------------------------------------------------------ *
 * Fixtures
 * ------------------------------------------------------------------ */

// getJSON reads a GET response as an object.
//
// It takes the app rather than building one: a test that seeds a row into a
// store has to ask *that* store's router, and testApp() would hand back a
// freshly-migrated empty one, making the seeded row invisible and the test pass
// for the wrong reason.
func getJSON(t *testing.T, app *fiber.App, path string) map[string]any {
	t.Helper()

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, path, nil))
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s returned %d: %s", path, resp.StatusCode, raw)
	}

	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("the response is not JSON: %v", err)
	}
	return decoded
}

// pascalCaseKeys returns the keys that look like Go field names, which is the
// signature of a struct that lost its JSON tags.
func pascalCaseKeys(keys []string) []string {
	var out []string
	for _, k := range keys {
		if k == "" {
			continue
		}
		// A snake_case key is lowercase; anything with an uppercase letter at the
		// start is a Go field name that leaked through.
		if k[0] >= 'A' && k[0] <= 'Z' {
			out = append(out, k)
		}
	}
	return out
}

func keysOf(row map[string]any) []string {
	var out []string
	for k := range row {
		out = append(out, k)
	}
	return out
}

/* ------------------------------------------------------------------ *
 * The endpoint contract
 * ------------------------------------------------------------------ */

func TestAccountsEndpointEmitsSnakeCase(t *testing.T) {
	// The bug this pins: store.Account had no JSON tags, so the handler's
	// `c.JSON(fiber.Map{"accounts": accounts})` serialised it with Go field names
	// — AccountID, CookieHash, SapisidFingerprint — while every other endpoint in
	// the API is snake_case. A consumer reading `account_id` got nothing, and got
	// it silently.
	app, st := testAppWithStore(t)

	credits := 1042
	authuser := 0
	if err := st.UpsertAccount(store.Account{
		AccountID:          "acct-0c960ba36880",
		CookieHash:         "41285e797f70",
		Credits:            &credits,
		Status:             "active",
		IdentityKey:        "core:cbd8fb99a068",
		IdentitySource:     "cookie-core",
		LastAuthuser:       &authuser,
		SapisidFingerprint: "9eed074c15eb7a3d",
	}); err != nil {
		t.Fatalf("seeding the account: %v", err)
	}

	body := getJSON(t, app, "/v1/accounts")
	rows, ok := body["accounts"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("expected one account, got %v", body["accounts"])
	}
	row, ok := rows[0].(map[string]any)
	if !ok {
		t.Fatalf("the account row is %T, want an object", rows[0])
	}

	if leaked := pascalCaseKeys(keysOf(row)); len(leaked) != 0 {
		t.Errorf("the endpoint leaked Go field names: %v", leaked)
	}

	for key, want := range map[string]any{
		"account_id":          "acct-0c960ba36880",
		"cookie_hash":         "41285e797f70",
		"identity_key":        "core:cbd8fb99a068",
		"identity_source":     "cookie-core",
		"sapisid_fingerprint": "9eed074c15eb7a3d",
		"status":              "active",
		"credits":             float64(1042),
		"last_authuser":       float64(0),
	} {
		if row[key] != want {
			t.Errorf("%s = %v, want %v", key, row[key], want)
		}
	}
}

func TestAccountsEndpointKeepsNilDistinguishableFromZero(t *testing.T) {
	// `credits` is a pointer precisely so "could not read the balance" is not
	// reported as "no credits left", and `last_authuser` is a pointer because
	// index 0 is a real account. Both must survive as an explicit null rather
	// than being dropped by omitempty, which would put the two back to being the
	// same value.
	app, st := testAppWithStore(t)

	if err := st.UpsertAccount(store.Account{
		AccountID: "acct-unknown-balance", Status: "active",
	}); err != nil {
		t.Fatalf("seeding the account: %v", err)
	}

	body := getJSON(t, app, "/v1/accounts")
	rows := body["accounts"].([]any)
	row := rows[0].(map[string]any)

	for _, key := range []string{"credits", "last_authuser"} {
		value, present := row[key]
		if !present {
			t.Errorf("%s is absent; a nil must be sent as null, not omitted", key)
			continue
		}
		if value != nil {
			t.Errorf("%s = %v, want null", key, value)
		}
	}
}

func TestAccountJSONOmitsWhatEmptyMeansNothingFor(t *testing.T) {
	// The other half of the rule: a field whose empty value *is* the answer —
	// no error, not superseded — is omitted rather than sent as "".
	data, err := json.Marshal(store.Account{AccountID: "acct-1", Status: "active"})
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}

	var row map[string]any
	if err := json.Unmarshal(data, &row); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}

	for _, key := range []string{"last_error", "superseded_by", "last_used_at", "credits_checked_at"} {
		if _, present := row[key]; present {
			t.Errorf("%s is present on an account that has never had one: %s", key, data)
		}
	}
}

func TestAccountJSONKeepsEveryAlwaysMeaningfulField(t *testing.T) {
	// These are never absent, even when empty: a consumer reading `status` or
	// `identity_source` should not have to distinguish "missing" from "empty".
	data, err := json.Marshal(store.Account{AccountID: "acct-1"})
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}

	var row map[string]any
	if err := json.Unmarshal(data, &row); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}

	for _, key := range []string{
		"account_id", "cookie_hash", "sku", "credits", "status",
		"total_requests", "total_failures", "created_at",
		"identity_key", "identity_source", "last_authuser",
	} {
		if _, present := row[key]; !present {
			t.Errorf("%s is absent from %s", key, data)
		}
	}
}

func TestAccountRefIsTaggedLikeAccount(t *testing.T) {
	// Nothing serialises AccountRef today. It is tagged anyway because it is the
	// other half of the same identity record, and the next endpoint that returns
	// it would otherwise inherit the identical bug.
	data, err := json.Marshal(store.AccountRef{
		AccountID: "acct-1", IdentityKey: "core:abc", SapisidFingerprint: "9eed",
	})
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}

	var row map[string]any
	if err := json.Unmarshal(data, &row); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}
	if leaked := pascalCaseKeys(keysOf(row)); len(leaked) != 0 {
		t.Errorf("AccountRef leaked Go field names: %v", leaked)
	}
	for _, key := range []string{"account_id", "identity_key", "sapisid_fingerprint"} {
		if _, present := row[key]; !present {
			t.Errorf("%s is absent from %s", key, data)
		}
	}
}

func TestExportPayloadUsesTheSameAccountKeysAsTheEndpoint(t *testing.T) {
	// `flow-go export` marshals the same struct, so a divergence between the file
	// and the API is a real possibility worth closing. Both go through
	// store.Account, which is the point of tagging it there rather than building
	// a response DTO in the handler.
	endpointRow := func() map[string]any {
		app, st := testAppWithStore(t)
		if err := st.UpsertAccount(store.Account{
			AccountID: "acct-1", Status: "active", CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("seeding: %v", err)
		}
		return getJSON(t, app, "/v1/accounts")["accounts"].([]any)[0].(map[string]any)
	}()

	exported := map[string]any{}
	blob, err := json.Marshal(store.Account{AccountID: "acct-1", Status: "active"})
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	if err := json.Unmarshal(blob, &exported); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}

	for key := range exported {
		if _, present := endpointRow[key]; !present {
			t.Errorf("export has %q but the endpoint does not", key)
		}
	}
}
