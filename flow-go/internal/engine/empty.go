package engine

import (
	"fmt"

	"github.com/kodelyx/flow-go/flow-go/internal/batchexecute"
)

// EmptyResultError reports that the transport accepted a generation and returned
// no media.
//
// It exists because the alternative was a 200. The submission was understood —
// the RPC answered, no error was raised, the job row was written — so the
// handler had nothing to branch on and returned success with `status: "empty"`
// and `media: null`. A caller reading the status code saw a generation that
// worked; a caller reading the body saw an empty array. Seven of the fourteen
// jobs in the live database were recorded that way, and from the outside they
// were indistinguishable from a success that had not finished yet.
//
// The request did not succeed. Flow accepted it and produced nothing, which is a
// bad response from a dependency rather than a bad request from the caller — so
// it is an error, it is a 502, and the body still carries the outcome so the
// diagnostic detail is not thrown away in the process of reporting it.
//
// The error and the outcome are returned together. Go permits a non-nil value
// alongside a non-nil error for exactly this case — io.Reader returns n and
// io.EOF — and it is the only way to report the failure and keep what came back.
type EmptyResultError struct {
	// Kind is "video" or "image".
	Kind string
	// Model is the model key the request was submitted with.
	Model string
	// JobID is the row in generations, so the attempt can be found later.
	JobID string
	// Account is the account the job ran on.
	Account string
	// Frames is how many raw frames came back. Zero means the transport said
	// nothing at all; non-zero means it said something that carried no media
	// id, which is a different problem with a different cause.
	Frames int
	// Rejected is the reason the server gave for refusing the request, when it
	// gave one, e.g. "PUBLIC_ERROR_UNUSUAL_ACTIVITY".
	//
	// A refusal and a silent no-op both arrive as HTTP 200 with a frame that
	// carries no media, and until this field existed they were reported as the
	// same event — which is how a submission the reCAPTCHA assessment had
	// thrown out came back with a hint about the model enum and the project.
	// Both were correct. The reason was in the response the whole time, in a
	// sibling slot the parser did not read.
	Rejected string
	// Hint is the most likely explanation, from the engine's own knowledge of
	// the transport.
	Hint string
}

func (e *EmptyResultError) Error() string {
	msg := fmt.Sprintf("engine: the transport accepted the %s request and returned no %s",
		e.Kind, e.Kind)
	switch {
	case e.Rejected != "":
		// Not "no frames": the server answered, and said why.
		msg += fmt.Sprintf(" — the server refused it: %s", e.Rejected)
	case e.Frames == 0:
		msg += " (it answered with no frames at all)"
	default:
		msg += fmt.Sprintf(" (it answered with %d frame(s) carrying no media id)", e.Frames)
	}
	if e.Model != "" {
		msg += ", model " + e.Model
	}
	if e.JobID != "" {
		msg += ", job " + shortID(e.JobID)
	}
	if e.Account != "" {
		msg += ", account " + e.Account
	}
	if e.Hint != "" {
		msg += ". " + e.Hint
	}
	return msg
}

// emptyImageHint explains the likely cause of an image request that came back
// with nothing.
//
// Separate from emptySubmissionHint because the two transports answer
// differently: an image call returns the asset inline, so an empty result means
// the request was understood and declined, whereas a video submission can also
// come back empty because the render was refused. Naming the wrong one sends the
// reader after the wrong cause, which is what the video hint's own comment
// records happening twice already.
func (e *Engine) emptyImageHint() string {
	return "The image call returns the asset inline, so an empty frame means the request " +
		"was understood and declined rather than queued. Check that the model enum is one " +
		"this account can reach, and that the project belongs to the account in use — a " +
		"request sent into another account's project is accepted and answered with nothing."
}

// reasonUnusualActivity is the reason Flow gives when it will not accept the
// reCAPTCHA token.
//
// Aliased to the transport's constant rather than spelled out again: the value
// is what a refusal carries in its frame, so the package that parses the frame
// owns it, and two literals that must stay equal are two chances to drift.
const reasonUnusualActivity = batchexecute.ReasonUnusualActivity

// refusedHint explains a submission the server refused by name.
//
// It exists so that a refusal stops arriving with the empty-result hint. The
// two are not the same problem: the empty hint names the model enum and the
// project, and both were correct when this was diagnosed — the assessment was
// what declined the request. A hint that sends the reader after two things that
// are already right is worse than no hint, because it costs the time spent
// checking them.
func refusedHint(reason string) string {
	if batchexecute.IsUnusualActivity(reason) {
		return "The server declined the reCAPTCHA assessment, not the model and not the project — " +
			reasonUnusualActivity + " is what Flow answers when it will not accept the token, and the " +
			"same enum and the same project generate normally once it does. Mint the token from a real " +
			"page instead: load the extension, sign in to Flow, and run with --captcha broker. The " +
			"browserless HTTP token scores lower, and an assessment that has started refusing this " +
			"client usually accepts it again later."
	}
	return "The server refused this request and named the reason: " + reason + ". That is a stated " +
		"reason rather than an empty answer, so it is the one to act on — it is not a wrong model " +
		"enum or a project belonging to another account."
}

// hintForRejection picks the explanation for a refusal, falling back to the
// empty-result hint when the server refused without naming a reason.
func hintForRejection(reason, fallback string) string {
	if reason == "" {
		return fallback
	}
	return refusedHint(reason)
}

// videoEmptyError builds the outcome for a video submission that produced
// nothing, carrying the server's reason when it gave one.
//
// The three video RPCs share it so that the refusal handling cannot drift
// between them: they answer in the same frame shape, and a reason the server
// stated to one of them is a reason it stated to all three.
func (e *Engine) videoEmptyError(kind, model, jobID string, frames []batchexecute.Frame) *EmptyResultError {
	reason := batchexecute.UpstreamError(frames)
	return &EmptyResultError{
		Kind:     kind,
		Model:    model,
		JobID:    jobID,
		Account:  e.AccountID(),
		Frames:   len(frames),
		Rejected: reason,
		Hint:     hintForRejection(reason, e.emptySubmissionHint()),
	}
}
