package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kodelyx/flow-go/flow-go/internal/app"
)

// One account failing must not stop the others — that is the whole point of
// running them separately — and the failure has to land on its own row.
func TestRunAccountsIsolatesFailures(t *testing.T) {
	paths := []string{"a.json", "b.json", "c.json"}

	var mu sync.Mutex
	started := map[string]bool{}

	runs := runAccounts(paths, func(path string) accountRun {
		mu.Lock()
		started[path] = true
		mu.Unlock()

		if path == "b.json" {
			return accountRun{AccountID: "b", Status: runFailed, Err: "the session expired"}
		}
		return accountRun{AccountID: path, Status: runOK}
	})

	if len(runs) != len(paths) {
		t.Fatalf("got %d runs, want %d", len(runs), len(paths))
	}
	if runs[1].Status != runFailed || runs[1].Err != "the session expired" {
		t.Errorf("the failing account's row is %+v, want its own failure", runs[1])
	}
	if runs[0].Status != runOK || runs[2].Status != runOK {
		t.Error("a failure in one account stopped another")
	}
	if len(started) != len(paths) {
		t.Errorf("only %d of %d accounts ran at all", len(started), len(paths))
	}
}

// The results come back in the order the paths were given, whatever order the
// goroutines happen to finish in.
//
// The summary is read against the file list, so a table that reshuffles itself
// between runs cannot be compared with the previous one — and it is what lets the
// layout tests assert positions rather than search for values.
func TestRunAccountsKeepsTheInputOrder(t *testing.T) {
	paths := []string{"slow.json", "fast.json", "medium.json"}

	runs := runAccounts(paths, func(path string) accountRun {
		switch path {
		case "slow.json":
			time.Sleep(30 * time.Millisecond)
		case "medium.json":
			time.Sleep(15 * time.Millisecond)
		}
		return accountRun{AccountID: path, Status: runOK}
	})

	for i, path := range paths {
		if runs[i].AccountID != path {
			t.Errorf("runs[%d] is %q, want %q — the order is not the input order",
				i, runs[i].AccountID, path)
		}
	}
}

// They genuinely run at the same time, which is the point of the flag.
//
// The proof is a barrier rather than a stopwatch: every run waits until all of
// them have started, which can only be satisfied if they overlap. A sequential
// runner would deadlock here and the test would time out — a far more reliable
// signal than a timing threshold on a loaded machine, where a slow scheduler
// could fail a correct implementation.
func TestRunAccountsRunsConcurrently(t *testing.T) {
	const accounts = 3

	var arrived sync.WaitGroup
	arrived.Add(accounts)
	release := make(chan struct{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		runAccounts([]string{"a", "b", "c"}, func(string) accountRun {
			arrived.Done()
			<-release
			return accountRun{Status: runOK}
		})
	}()

	allStarted := make(chan struct{})
	go func() { arrived.Wait(); close(allStarted) }()

	select {
	case <-allStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("only some accounts started; they are not running concurrently")
	}
	close(release)
	<-done
}

func TestRunAccountsWithNoPaths(t *testing.T) {
	runs := runAccounts(nil, func(string) accountRun {
		t.Error("the runner was called with no paths to run")
		return accountRun{}
	})
	if len(runs) != 0 {
		t.Errorf("got %d runs, want none", len(runs))
	}
}

// A skipped account has no duration.
//
// It never ran, so measuring the file checks and printing "0s" would read as "ran
// instantly" — the opposite of what happened, and the reason the column is not
// simply a stopwatch. This is the live behaviour that a bare formatDuration test
// would not have caught: the measured time is a few microseconds, not zero, so
// the "-" branch never fired on its own.
func TestASkippedAccountReportsNoDuration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "account_x.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	run := runForAccount(context.Background(), commonFlags{}, path,
		func(context.Context, *app.App) ([]string, error) {
			t.Error("a skipped account reached the generator")
			return nil, nil
		})

	if run.Status != runSkipped {
		t.Fatalf("status = %q, want %q", run.Status, runSkipped)
	}
	if run.Duration != 0 {
		t.Errorf("duration = %v, want none", run.Duration)
	}
	if got := formatDuration(run.Duration); got != "-" {
		t.Errorf("the table would show %q for a skipped account, want \"-\"", got)
	}
	if run.Err == "" {
		t.Error("the skip reason is not recorded, so the table would say nothing about why")
	}
}

func TestRenderAccountRunsLayout(t *testing.T) {
	runs := []accountRun{
		{AccountID: "acct-aaaa", Duration: 22 * time.Second, Status: runOK, Files: []string{"output/a.jpg"}},
		{AccountID: "acct-bbbb", Duration: 1500 * time.Millisecond, Status: runFailed, Err: "the session expired"},
		{AccountID: "acct-cccc", Status: runSkipped, Err: "no credential cookies in the file"},
	}

	out := renderAccountRuns(runs)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")

	if len(lines) != 2+len(runs) {
		t.Fatalf("got %d lines, want %d:\n%s", len(lines), 2+len(runs), out)
	}
	for _, want := range []string{"#", "Account ID", "Duration", "Status", "Output"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("the header does not name %q: %q", want, lines[0])
		}
	}
	for i, line := range lines[1:] {
		if len(line) != len(lines[0]) {
			t.Errorf("line %d is %d wide and the header is %d, so the columns do not line up",
				i+1, len(line), len(lines[0]))
		}
	}

	// The failure's own words are in the table, so the table is the whole record
	// and a reader does not have to scroll back through interleaved logs.
	if !strings.Contains(out, "the session expired") {
		t.Errorf("the failure is not named in the table:\n%s", out)
	}
	if !strings.Contains(out, "output/a.jpg") {
		t.Errorf("the output path is missing:\n%s", out)
	}
	for _, status := range []string{runOK, runFailed, runSkipped} {
		if !strings.Contains(out, status) {
			t.Errorf("the status %q does not appear:\n%s", status, out)
		}
	}
}

// A run that wrote several files names the first and counts the rest, rather
// than pushing the table out to the width of a whole list.
func TestRenderAccountRunsSummarisesMultipleFiles(t *testing.T) {
	out := renderAccountRuns([]accountRun{{
		AccountID: "acct-a",
		Status:    runOK,
		Files:     []string{"output/first.jpg", "output/second.jpg", "output/third.jpg"},
	}})

	if !strings.Contains(out, "output/first.jpg (+2 more)") {
		t.Errorf("the extra files are not summarised:\n%s", out)
	}
	if strings.Contains(out, "second.jpg") {
		t.Errorf("every path was printed, which is what the summary is for avoiding:\n%s", out)
	}
}

// A run that produced nothing says so rather than leaving the cell blank — and
// that is not the same as a failure, because --no-download legitimately writes
// no files.
func TestRenderAccountRunsMarksAnEmptyOutput(t *testing.T) {
	out := renderAccountRuns([]accountRun{{AccountID: "acct-a", Status: runOK}})

	if !strings.Contains(out, "-") {
		t.Errorf("an empty output cell is blank rather than marked:\n%s", out)
	}
}

// A skipped account never ran, so its duration is not "0s" — which would read as
// "ran instantly" and is the opposite of what happened.
func TestFormatDurationDistinguishesNotRunFromInstant(t *testing.T) {
	cases := map[time.Duration]string{
		0:                       "-",
		-1:                      "-",
		22 * time.Second:        "22s",
		1500 * time.Millisecond: "1.5s",
		62 * time.Second:        "1m2s",
	}
	for in, want := range cases {
		if got := formatDuration(in); got != want {
			t.Errorf("formatDuration(%v) = %q, want %q", in, got, want)
		}
	}
}
