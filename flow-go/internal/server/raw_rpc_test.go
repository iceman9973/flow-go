package server

import (
	"encoding/json"
	"testing"
)

// TestReplacePlaceholder covers the substitution a replayed capture depends on.
//
// The token sits at a different depth in each captured payload — the video
// submissions carry it at [1][10][0], the edit capture at [1][10][0] as well but
// behind a differently shaped request — so the walk has to be shape-agnostic.
func TestReplacePlaceholder(t *testing.T) {
	var value any
	const raw = `[[[[null, null, [[["a prompt"]]]], "m", 2, null]], [null, 22, null, null, null, "p", null, null, null, null, ["__CAPTCHA__", 1]], ["u", 2]]`
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		t.Fatalf("fixture is not valid JSON: %v", err)
	}

	out := replacePlaceholder(value, "__CAPTCHA__", "FRESH")

	encoded, _ := json.Marshal(out)
	var want any
	if err := json.Unmarshal([]byte(`[[[[null, null, [[["a prompt"]]]], "m", 2, null]], [null, 22, null, null, null, "p", null, null, null, null, ["FRESH", 1]], ["u", 2]]`), &want); err != nil {
		t.Fatal(err)
	}
	wantEncoded, _ := json.Marshal(want)

	if string(encoded) != string(wantEncoded) {
		t.Errorf("got  %s\nwant %s", encoded, wantEncoded)
	}

	// A payload without the placeholder must come back untouched.
	plain := map[string]any{"a": []any{"x", float64(1), nil}}
	got, _ := json.Marshal(replacePlaceholder(plain, "__CAPTCHA__", "FRESH"))
	if string(got) != `{"a":["x",1,null]}` {
		t.Errorf("payload without the placeholder changed: %s", got)
	}

	// Only an exact match is substituted, so a token-shaped string in a prompt
	// is left alone.
	partial := []any{"see __CAPTCHA__ here"}
	got, _ = json.Marshal(replacePlaceholder(partial, "__CAPTCHA__", "FRESH"))
	if string(got) != `["see __CAPTCHA__ here"]` {
		t.Errorf("a partial match was substituted: %s", got)
	}
}
