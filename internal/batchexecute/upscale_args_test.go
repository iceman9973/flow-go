package batchexecute

import (
	"testing"
	"time"
)

// The SPrCad payload, pinned against the app's own request.
//
// Captured by hooking fetch in the page and clicking Download -> 2K Upscaled:
//
//	url    /_/AiSandboxAngularFrontend/data/batchexecute?rpcids=SPrCad
//	       &source-path=%2Fproject%2Fbe2b5220-…&bl=…&f.sid=…&hl=en-US&_reqid=…&rt=c
//	body   [[["SPrCad","[\"c80ea4ac-…\",1,[null,22,null,null,null,\"be2b5220-…\",
//	       null,null,null,null,[\"0cAFcWeA…\",1789878183099]]]","generic"]]]
//
// Three things in there are the opposite of the generation calls, and each of
// them was wrong here for a long time — two of them for the entire life of the
// transport path. They are pinned together because they were found together and
// because every one of them fails silently: a wrong position returns a null
// payload rather than an error.

func TestUpscaleArgsPutTheMediaIDFirst(t *testing.T) {
	req := ImageUpscaleRequest{
		ProjectID: "proj-1",
		MediaID:   "media-1",
		ContentID: "content-1",
	}

	args, _ := upscaleArgs(req, 1, nil)

	if len(args) != 3 {
		t.Fatalf("arg has %d positions, want 3", len(args))
	}
	if args[0] != "media-1" {
		t.Errorf("position 0 is %v, want the media id — the app sends the media id, "+
			"and the generation calls taking content ids is what made this look right",
			args[0])
	}
}

// A caller holding only the content id still gets a request rather than an empty
// position, because for an image the two are usually the same value.
func TestUpscaleArgsFallBackToTheContentID(t *testing.T) {
	args, _ := upscaleArgs(ImageUpscaleRequest{ProjectID: "p", ContentID: "content-1"}, 1, nil)
	if args[0] != "content-1" {
		t.Errorf("position 0 is %v, want the content id as a fallback", args[0])
	}
}

func TestUpscaleSourcePathNamesOnlyTheProject(t *testing.T) {
	got, path := "", ""
	_, path = upscaleArgs(ImageUpscaleRequest{
		ProjectID: "proj-1",
		MediaID:   "media-1",
	}, 1, nil)
	got = path

	if got != "/project/proj-1" {
		t.Errorf("source path is %q, want %q — the app does not append the editor "+
			"route, and the media id travels in position 0 instead", got, "/project/proj-1")
	}
}

// An explicit source path is still honoured; this is a diagnostic escape hatch.
func TestUpscaleSourcePathIsOverridable(t *testing.T) {
	_, path := upscaleArgs(ImageUpscaleRequest{
		ProjectID:  "proj-1",
		MediaID:    "media-1",
		SourcePath: "/project/proj-1/edit/media-1",
	}, 1, nil)

	if path != "/project/proj-1/edit/media-1" {
		t.Errorf("source path is %q, want the caller's", path)
	}
}

// The captcha pair carries a millisecond clock reading on this RPC. Generation
// sends a literal 1 and must keep doing so — see captchaPair.
func TestCaptchaPairCarriesATimestamp(t *testing.T) {
	before := time.Now().UnixMilli()
	pair := captchaPair("token-1")
	after := time.Now().UnixMilli()

	if len(pair) != 2 {
		t.Fatalf("pair has %d elements, want 2", len(pair))
	}
	if pair[0] != "token-1" {
		t.Errorf("element 0 is %v, want the token", pair[0])
	}

	ts, ok := pair[1].(int64)
	if !ok {
		t.Fatalf("element 1 is %T, want an int64 clock reading", pair[1])
	}
	if ts < before || ts > after {
		t.Errorf("element 1 is %d, outside the %d..%d window this call occupied — "+
			"a constant here decodes to 1970 and is the shape a forged request has",
			ts, before, after)
	}
}
