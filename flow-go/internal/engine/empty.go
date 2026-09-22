package engine

import "fmt"

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
	// Hint is the most likely explanation, from the engine's own knowledge of
	// the transport.
	Hint string
}

func (e *EmptyResultError) Error() string {
	msg := fmt.Sprintf("engine: the transport accepted the %s request and returned no %s",
		e.Kind, e.Kind)
	if e.Frames == 0 {
		msg += " (it answered with no frames at all)"
	} else {
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
