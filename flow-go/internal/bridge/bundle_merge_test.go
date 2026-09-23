package bridge

import (
	"testing"

	"github.com/kodelyx/flow-go/flow-go/internal/cookiejar"
)

// A sync must never be able to erase what an earlier one learned. A profile with
// no Flow tab open reports no fingerprint, no page tokens and no project in its
// URL, and those are exactly the fields that cost a browser to obtain — a project
// id in particular takes an editor tab to relearn.

func mergeCookie(name, value string) cookiejar.Cookie {
	return cookiejar.Cookie{Domain: ".google.com", Path: "/", Name: name, Value: value}
}

func knownBundle() *cookiejar.Bundle {
	return &cookiejar.Bundle{
		AccountID:   "acct-hb4069beef31e",
		ProjectID:   "72c23f1e-22c5-45ac-a143-36f4c8f3625d",
		At:          "AIQ-original",
		Fsid:        "933442678266528993",
		Fingerprint: &cookiejar.Fingerprint{UserAgent: "UA-original", SecChUa: "CH", Platform: "macOS"},
		Cookies:     []cookiejar.Cookie{mergeCookie("SAPISID", "old")},
	}
}

func TestASyncWithNothingToReadKeepsWhatTheFileHeld(t *testing.T) {
	// The shape of a sync from a profile with no Flow tab open: cookies arrive,
	// nothing else does.
	got := mergeBundle(knownBundle(), []cookiejar.Cookie{mergeCookie("SAPISID", "new")}, nil, "", "", "")

	if got.ProjectID != "72c23f1e-22c5-45ac-a143-36f4c8f3625d" {
		t.Errorf("project = %q, want the one already on record", got.ProjectID)
	}
	if got.At != "AIQ-original" || got.Fsid != "933442678266528993" {
		t.Errorf("tokens = %q/%q, want the ones already on record", got.At, got.Fsid)
	}
	if got.Fingerprint.Empty() || got.Fingerprint.UserAgent != "UA-original" {
		t.Errorf("fingerprint = %+v, want the one already on record", got.Fingerprint)
	}
	if got.AccountID != "acct-hb4069beef31e" {
		t.Errorf("account = %q, want the one already on record", got.AccountID)
	}

	// The credential is the one thing that is replaced, because it is the one
	// thing this sync actually measured.
	if len(got.Cookies) != 1 || got.Cookies[0].Value != "new" {
		t.Errorf("cookies = %+v, want the freshly synced one", got.Cookies)
	}
}

func TestASyncThatReadSomethingReplacesIt(t *testing.T) {
	fp := &cookiejar.Fingerprint{UserAgent: "UA-fresh", Platform: "macOS"}

	got := mergeBundle(knownBundle(), nil, fp, "AIQ-fresh", "111", "")

	if got.Fingerprint.UserAgent != "UA-fresh" {
		t.Errorf("fingerprint = %+v, want the freshly read one", got.Fingerprint)
	}
	if got.At != "AIQ-fresh" || got.Fsid != "111" {
		t.Errorf("tokens = %q/%q, want the freshly read ones", got.At, got.Fsid)
	}
	// A project id is only ever filled in, never overwritten: a sync cannot know
	// that the project on record is wrong, only that it did not see one.
	if got.ProjectID != "72c23f1e-22c5-45ac-a143-36f4c8f3625d" {
		t.Errorf("project = %q, want the one already on record", got.ProjectID)
	}
}

func TestAProjectIsLearnedOnceAndKept(t *testing.T) {
	got := mergeBundle(&cookiejar.Bundle{}, nil, nil, "", "", "project-fresh")

	if got.ProjectID != "project-fresh" {
		t.Errorf("project = %q, want project-fresh", got.ProjectID)
	}

	// And the next sync, which sees no project, does not take it away again.
	again := mergeBundle(got, nil, nil, "", "", "")
	if again.ProjectID != "project-fresh" {
		t.Errorf("project = %q after a blind sync, want it kept", again.ProjectID)
	}
}

func TestMergingIntoNothingIsFine(t *testing.T) {
	got := mergeBundle(nil, []cookiejar.Cookie{mergeCookie("SAPISID", "v")}, nil, "", "", "")

	if len(got.Cookies) != 1 {
		t.Fatalf("cookies = %d, want 1", len(got.Cookies))
	}
	if !got.Fingerprint.Empty() || got.ProjectID != "" || got.At != "" {
		t.Errorf("bundle = %+v, want only cookies", got)
	}
}

// TestAnEmptyTokenDoesNotHalfReplaceThePair keeps the two page tokens together.
// They are used as a pair, so a sync that read one and not the other would leave
// a mixture that belongs to no page.
func TestAnEmptyTokenDoesNotHalfReplaceThePair(t *testing.T) {
	got := mergeBundle(knownBundle(), nil, nil, "AIQ-only-at", "", "")

	if got.At != "AIQ-only-at" {
		t.Errorf("at = %q, want the fresh one", got.At)
	}
	if got.Fsid != "933442678266528993" {
		t.Errorf("fsid = %q, want the one already on record", got.Fsid)
	}
}
