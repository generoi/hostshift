package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// Round 48, on 717ad9d.
//
// TestR48TheLiveSiteWarningStillMiscountsAPathListLongerThanN: 717ad9d moved the
// live-site warning below the `--paths` read so it could count what will
// actually be fetched, and its own comment says so —
//
//	// How many will actually be fetched: the supplied list if there is one, and
//	// otherwise the crawl's budget.
//
// — but a supplied list is not what will be fetched either. `corpus.Run` bounds
// it: `if !crawled && o.N > 0 && len(paths) > o.N { paths = paths[:o.N] }`. So
// the count is right for a list shorter than `-n` (the case the change was made
// for) and wrong for a list longer than it, by exactly the amount `-n` cuts off.
//
// The number is the whole content of the sentence. It is the one line printed to
// make a developer stop before a crawl of the client's live site, and the audit
// record already says of the old spelling that this is "the one sentence written
// to make a developer stop". Overstating is the safer direction of the two, but
// a guardrail that cannot say how many requests it is about to make to
// production is not one a reader can act on: the answer to "is 200 pages of the
// live site acceptable?" is different from the answer to "is 20?".
func TestR48TheLiveSiteWarningStillMiscountsAPathListLongerThanN(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".ddev/config.yaml", "name: acme\n")
	writeFile(t, dir, "hostshift.yaml",
		"sites:\n  - canonical: https://www.acme.invalid\n    variant: https://wt-a--acme.ddev.site\n")
	writeFile(t, dir, "paths.txt", "/one\n/two\n/three\n/four\n/five\n")

	// The control is the case 717ad9d fixed: a list shorter than -n is counted
	// as the list, so the sentence does read the paths file.
	writeFile(t, dir, "short.txt", "/one\n/two\n")
	short := append([]string{"-C", dir, "--slug", "wt-a", "-n", "20",
		"--paths", filepath.Join(dir, "short.txt")},
		noNetwork("wt-a--acme.ddev.site")...)
	if _, _, ctl := run(t, "", cmdDiff, short...); !strings.Contains(ctl, "crawling 2 page(s)") {
		t.Fatalf("the control case no longer holds, so this test measures nothing:\n%s", ctl)
	}

	args := append([]string{"-C", dir, "--slug", "wt-a", "-n", "2",
		"--paths", filepath.Join(dir, "paths.txt")},
		noNetwork("wt-a--acme.ddev.site")...)
	_, _, errOut := run(t, "", cmdDiff, args...)
	if !strings.Contains(errOut, "crawling 2 page(s)") {
		t.Errorf("the warning counts the whole --paths file, but corpus.Run cuts it "+
			"to -n, so it names a number of live-site requests that will not be made:\n%s",
			errOut)
	}
}
