package main

import (
	"strings"
	"testing"
)

// Round 44, on 7cb756c ("Ask the size question in bytes; stop reports asserting
// what they skipped").

// TestADiffThatComparedNothingIsNotGreen.
//
// 7cb756c is a whole commit about a report claiming more than it checked: the
// verdict "no canonical origin reached the browser" printed two lines under a
// count of origins that had just reached it. `WriteReport` still prints the
// unqualified sentence — and exits 0 — over a run of zero pages.
//
// `corpus.Run` bounds its crawl with
//
//	for len(queue) > 0 && (o.N == 0 || len(out) < o.N)
//
// so any negative `-n` makes the condition false on the first iteration, the
// crawl returns no paths at all, and `WriteReport` walks an empty slice: `green`
// is never set false because nothing was ever compared. The summary line does
// say "0 pages", but the line under it is the one the README calls the check
// that validates a deployment against reality, and it asserts invariant 28
// about bytes the run never fetched. A CI job reads the exit status.
//
// The fix is a floor, not a guess about intent: a run that compared nothing
// verified nothing, and cannot be GREEN.
func TestADiffThatComparedNothingIsNotGreen(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".ddev/config.yaml", "name: acme\n")
	writeFile(t, dir, "hostshift.yaml", ""+
		"sites:\n"+
		"  - canonical: https://www.acme.fi\n    variant: https://wt-a--acme.ddev.site\n")

	args := append([]string{"-C", dir, "--slug", "wt-a", "-n", "-1"},
		noNetwork("www.acme.fi", "wt-a--acme.ddev.site")...)
	code, out, _ := run(t, "", cmdDiff, args...)

	// Premise: this really is the zero-page run, so the assertion below is
	// about the verdict and not about a crawl that quietly did something.
	if !strings.Contains(out, "0 pages,") {
		t.Fatalf("fixture: the run compared something after all:\n%s", out)
	}
	if strings.Contains(out, "GREEN") {
		t.Errorf("a run that fetched nothing printed the invariant-28 verdict:\n%s", out)
	}
	if code == 0 {
		t.Errorf("exit 0 from a run that compared no pages — a CI job reads this")
	}
}
