package cli

import (
	"strings"
	"testing"
)

// A balance nobody has read is not zero.
//
// The store keeps the two apart on purpose — nil is "nobody has asked", 0 is
// "asked, and the wallet is empty" — and rendering both as 0 would put them back
// to being the same number.
func TestFormatCreditsKeepsUnreadDistinctFromZero(t *testing.T) {
	zero, forty := 0, 42

	cases := []struct {
		name    string
		credits *int
		want    string
	}{
		{"unread", nil, "not read"},
		{"genuinely empty", &zero, "0"},
		{"a balance", &forty, "42"},
	}
	for _, tc := range cases {
		if got := formatCredits(tc.credits); got != tc.want {
			t.Errorf("%s: formatCredits = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestAccountHashFromFile(t *testing.T) {
	cases := map[string]string{
		"cookies/account_2a91422b663a.json": "2a91422b663a",
		"/abs/path/account_deadbeef.json":   "deadbeef",
		"account_x.json":                    "x",
		// A name that does not follow the convention is still reported as
		// itself rather than mangled into something unrecognisable.
		"cookies.json":  "cookies",
		"cookies/weird": "weird",
		"account_.json": "",
	}
	for in, want := range cases {
		if got := accountHashFromFile(in); got != want {
			t.Errorf("accountHashFromFile(%q) = %q, want %q", in, got, want)
		}
	}
}

// The table is the deliverable, so its shape is worth pinning: the header, a
// rule, one line per account, then the total.
func TestRenderBalanceTableLayout(t *testing.T) {
	zero, forty := 0, 42
	rows := []balanceRow{
		{AccountID: "acct-aaaa", ShortHash: "aaaa", ProjectID: "proj-1", Credits: &forty, Status: "active"},
		{AccountID: "acct-bbbb", ShortHash: "bbbb", ProjectID: "proj-2", Credits: &zero, Status: "active"},
		{AccountID: "acct-cccc", ShortHash: "cccc", ProjectID: "proj-3", Credits: nil, Status: "unknown"},
	}

	out := renderBalanceTable(rows)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")

	if len(lines) != 2+len(rows)+2 { // header, rule, rows, blank, total
		t.Fatalf("got %d lines, want %d:\n%s", len(lines), 2+len(rows)+2, out)
	}

	for _, want := range []string{"#", "Account ID", "Short Hash", "Project ID", "Credits", "Status"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("the header does not name %q: %q", want, lines[0])
		}
	}

	for _, row := range rows {
		found := false
		for _, line := range lines[2 : 2+len(rows)] {
			if strings.Contains(line, row.AccountID) && strings.Contains(line, row.ProjectID) {
				found = true
			}
		}
		if !found {
			t.Errorf("no line carries %s / %s:\n%s", row.AccountID, row.ProjectID, out)
		}
	}

	// The point of padding every line to the same width: the header, the rule and
	// each row line up, and that is checkable rather than a matter of opinion.
	for i, line := range lines[1 : 2+len(rows)] {
		if len(line) != len(lines[0]) {
			t.Errorf("line %d is %d wide and the header is %d, so the columns do not line up",
				i+1, len(line), len(lines[0]))
		}
	}
}

// The total counts only what was read, and says how much it is missing.
//
// Folding an unread balance in as zero would understate the total by exactly the
// accounts nobody has checked — the opposite of what a total is for.
func TestRenderBalanceTableTotalIgnoresUnreadBalances(t *testing.T) {
	ten, thirtyTwo := 10, 32
	rows := []balanceRow{
		{AccountID: "a", ShortHash: "a", Credits: &ten},
		{AccountID: "b", ShortHash: "b", Credits: &thirtyTwo},
		{AccountID: "c", ShortHash: "c", Credits: nil},
	}

	out := renderBalanceTable(rows)

	if !strings.Contains(out, "42 credit(s)") {
		t.Errorf("the total is not the sum of the read balances:\n%s", out)
	}
	if !strings.Contains(out, "across 2 account(s)") {
		t.Errorf("the total counts accounts whose balance was never read:\n%s", out)
	}
	if !strings.Contains(out, "1 unread") {
		t.Errorf("an unread balance is not reported:\n%s", out)
	}
}

// With nothing unread there is no caveat to print — the total is the total.
func TestRenderBalanceTableTotalIsUnqualifiedWhenEverythingIsRead(t *testing.T) {
	five := 5
	out := renderBalanceTable([]balanceRow{{AccountID: "a", ShortHash: "a", Credits: &five}})

	if !strings.Contains(out, "5 credit(s) across 1 account(s)") {
		t.Errorf("the total is wrong:\n%s", out)
	}
	if strings.Contains(out, "unread") {
		t.Errorf("the total is qualified although every balance was read:\n%s", out)
	}
}

// A file whose identity was never recorded still has a balance worth reporting,
// and the columns that are unknown say so rather than going blank.
func TestRenderBalanceTableFillsMissingColumns(t *testing.T) {
	out := renderBalanceTable([]balanceRow{{AccountID: "acct-x", ShortHash: "x"}})

	if !strings.Contains(out, "-") {
		t.Errorf("a missing project id is not marked:\n%s", out)
	}
	if !strings.Contains(out, "unknown") {
		t.Errorf("a missing status is not marked:\n%s", out)
	}
	if !strings.Contains(out, "not read") {
		t.Errorf("a missing balance is not marked:\n%s", out)
	}
}

func TestRenderBalanceTableWithNoAccounts(t *testing.T) {
	out := renderBalanceTable(nil)

	if !strings.Contains(out, "Account ID") {
		t.Errorf("the header is missing when there are no rows:\n%s", out)
	}
	if !strings.Contains(out, "0 credit(s) across 0 account(s)") {
		t.Errorf("the empty total is wrong:\n%s", out)
	}
}
