package batchexecute

import (
	"encoding/json"
	"testing"
)

// profileFrame is a real o30O0e response, trimmed of the fields this engine does
// not read but left in its true shape: the identity sits at indices 2, 3 and 9 of
// one run, each a one-element array wrapping a [<flags...>, "<value>", ...] pair.
//
// It is kept verbatim rather than hand-simplified because the whole difficulty
// of this RPC is that the values are not adjacent and are not keyed — a tidy
// fixture would test a shape the server never sends.
const profileFrame = `[[[["me",1,["100694712991212656733",
[null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,[null,["me"]]],
[[[true,0,true,null,null,null,null,null,"100694712991212656733",null,null,true,null,null,1],"Akash Yadav",null,"Akash","Yadav",null,null,null,null,null,null,null,"Yadav, Akash",null,null,"Akash Yadav"]],
[[[true,0,true,null,null,0,null,null,"100694712991212656733",null,null,null,null,null,1],"https://lh3.googleusercontent.com/a/ACg8ocIoRxcMJvwpGq8oUULmxtYlnuA64NCLa0uFJCkP61iulAoktT4",null,"EhUxMDA2OTQ3MTI5OTEyMTI2NTY3MzMoATDnoavz-f____8B"]],
null,null,null,null,null,
[[[null,0,true,null,null,null,null,null,"100694712991212656733",null,null,true,null,null,1],"infotecha189@gmail.com",null,null,null,null,null,null,1,[true]]],
null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,"%EgMCAwkaAgEH"],null,[]]]]]`

func TestFindProfileReadsIdentityFromItsTrueShape(t *testing.T) {
	var payload any
	if err := json.Unmarshal([]byte(profileFrame), &payload); err != nil {
		t.Fatalf("the fixture is not valid JSON: %v", err)
	}

	p, ok := findProfile(payload)
	if !ok {
		t.Fatal("findProfile found no identity in a real profile response")
	}
	if p.email != "infotecha189@gmail.com" {
		t.Errorf("email = %q, want infotecha189@gmail.com", p.email)
	}
	if p.name != "Akash Yadav" {
		t.Errorf("name = %q, want Akash Yadav", p.name)
	}
	wantPhoto := "https://lh3.googleusercontent.com/a/ACg8ocIoRxcMJvwpGq8oUULmxtYlnuA64NCLa0uFJCkP61iulAoktT4"
	if p.photo != wantPhoto {
		t.Errorf("photo = %q, want %q", p.photo, wantPhoto)
	}
}

// The name is not optional. A response that carried an address but no name is
// half-read, and accepting it would put a real address beside an empty label —
// the exact confusion this RPC was added to end.
func TestFindProfileRejectsAnAddressWithoutAName(t *testing.T) {
	var payload any
	body := `[[[["me",1,[["id"],null,[[[1],"infotecha189@gmail.com",null,null,null,1]],null,[]]]]]]`
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("the fixture is not valid JSON: %v", err)
	}
	if _, ok := findProfile(payload); ok {
		t.Error("findProfile accepted a response with an address but no name")
	}
}

// A bare string is not a field. The run ends with a checksum-like token, and the
// id appears as a plain string; reading either as the display name would label
// the account with a number.
func TestProfileFieldIgnoresUnwrappedStrings(t *testing.T) {
	for _, entry := range []any{
		"100694712991212656733",
		"%EgMCAwkaAgEH",
		[]any{"me"},
		[]any{nil, []any{"me"}},
		[]any{[]any{"only-flags"}},
	} {
		if got, ok := profileField(entry); ok {
			t.Errorf("profileField(%#v) = %q, true; want no match", entry, got)
		}
	}
}
