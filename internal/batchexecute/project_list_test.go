package batchexecute

import (
	"encoding/json"
	"testing"
	"time"
)

// liveProjectList is a real response from RPCIDProjectList, taken from a signed-in
// account with two projects. One has assets and so carries a poster and a
// trailing id; the other has none and stops after the timestamp. That difference
// is the reason the optional fields exist.
const liveProjectList = `[[
  ["e5d6409a-7c57-47ed-a2eb-25221c92ee23",
   ["12 Jul, 18:11", null, [1783860064, 805889000],
    "https://lh3.googleusercontent.com/asb/AB-nOUao52qwKks-ZU_hVo_KlA56JaeMOAR4DOLhzymKx39hDoCCXt6algsDpdBfnTioAL0Fmp8ykqQNGIeANmd8dqK8Bv3IKwEO9MtARun7yND8RgBAjG9PJoGSYQcKW6VxDbTnVwe3NmoDazizT0jRkFTXMJaU9b8gWhkdv2NA",
    "15ad781c-bec5-42b1-be09-d53ecc95d605"]],
  ["faafc69f-29b1-459a-ae67-c44a50bc0015",
   ["11 Jul, 11:38", null, [1783750119, 847856000]]]
]]`

func TestParseProjectListReadsBothRowShapes(t *testing.T) {
	var payload any
	if err := json.Unmarshal([]byte(liveProjectList), &payload); err != nil {
		t.Fatalf("the fixture is not valid JSON: %v", err)
	}

	projects := parseProjectList(payload)
	if len(projects) != 2 {
		t.Fatalf("got %d projects, want 2: %+v", len(projects), projects)
	}

	full := projects[0]
	if full.ID != "e5d6409a-7c57-47ed-a2eb-25221c92ee23" {
		t.Errorf("id = %q", full.ID)
	}
	if !full.Modified.Equal(time.Unix(1783860064, 805889000)) {
		t.Errorf("modified = %v, want the captured instant", full.Modified)
	}
	if full.Thumbnail == "" {
		t.Error("the thumbnail was dropped from a row that carries one")
	}
	if full.LastAssetID != "15ad781c-bec5-42b1-be09-d53ecc95d605" {
		t.Errorf("last asset id = %q", full.LastAssetID)
	}

	// The second row is a project with no assets. Its absence of a poster must
	// not be mistaken for a parse failure, and the id must survive.
	empty := projects[1]
	if empty.ID != "faafc69f-29b1-459a-ae67-c44a50bc0015" {
		t.Errorf("id = %q", empty.ID)
	}
	if empty.Thumbnail != "" || empty.LastAssetID != "" {
		t.Errorf("a project with no assets reported %q / %q", empty.Thumbnail, empty.LastAssetID)
	}
	if !empty.Modified.Equal(time.Unix(1783750119, 847856000)) {
		t.Errorf("modified = %v", empty.Modified)
	}
}

// An account with no projects answers with an empty list rather than an error,
// and that has to stay distinguishable from a malformed response.
func TestParseProjectListAcceptsAnEmptyAccount(t *testing.T) {
	for name, payload := range map[string]any{
		"empty outer": []any{},
		"empty inner": []any{[]any{}},
		"null":        nil,
	} {
		t.Run(name, func(t *testing.T) {
			if got := parseProjectList(payload); len(got) != 0 {
				t.Fatalf("got %d projects, want none", len(got))
			}
		})
	}
}

// Rows are selected by shape, so a wrapper or a stray scalar must not be
// reported as a project.
func TestParseProjectListSkipsRowsThatAreNotProjects(t *testing.T) {
	payload := []any{
		[]any{
			"not-a-uuid",
			[]any{"12 Jul, 18:11", nil, []any{float64(1783860064), float64(0)}},
		},
		[]any{
			"e5d6409a-7c57-47ed-a2eb-25221c92ee23",
			[]any{"12 Jul, 18:11", nil, []any{float64(1783860064), float64(0)}},
		},
		"a bare string",
	}

	projects := parseProjectList(payload)
	if len(projects) != 1 {
		t.Fatalf("got %d projects, want only the uuid-shaped one: %+v", len(projects), projects)
	}
	if projects[0].ID != "e5d6409a-7c57-47ed-a2eb-25221c92ee23" {
		t.Errorf("id = %q", projects[0].ID)
	}
}

func TestLooksLikeUUID(t *testing.T) {
	cases := map[string]bool{
		"e5d6409a-7c57-47ed-a2eb-25221c92ee23":  true,
		"E5D6409A-7C57-47ED-A2EB-25221C92EE23":  true,
		"e5d6409a7c5747eda2eb25221c92ee23":      false, // no dashes
		"e5d6409a-7c57-47ed-a2eb-25221c92ee2":   false, // one short
		"e5d6409a-7c57-47ed-a2eb-25221c92ee234": false, // one long
		"g5d6409a-7c57-47ed-a2eb-25221c92ee23":  false, // not hex
		"":                                      false,
	}
	for input, want := range cases {
		if got := looksLikeUUID(input); got != want {
			t.Errorf("looksLikeUUID(%q) = %v, want %v", input, got, want)
		}
	}
}
