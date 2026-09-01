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

// TestR45ASingleSiteWorktreeIsWarnedAboutAnUnmatchedBase.
//
// README's worktree recipe is `diff --slug <slug> --canonical-base
// https://<parent>.ddev.site`. On a worktree whose map comes from its *own* DDEV
// config, the variant is derived from the worktree's name — so that command
// compared the parent against `wt-a--acme-wt-a.ddev.site`, a hostname nothing
// serves, and every row was a 404.
//
// The pairing warning existed but was gated on multisite. The reason for
// tolerating an unmatched canonical base is production-canonical, where the
// documented baseline is deliberately not a canonical of the map; that reason
// does not apply to a map with no external canonical at all.
func TestR45ASingleSiteWorktreeIsWarnedAboutAnUnmatchedBase(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".ddev/config.yaml", "name: acme-wt-a\n")
	args := append([]string{"-C", dir, "--slug", "wt-a", "-n", "1",
		"--canonical-base", "https://acme.ddev.site"},
		noNetwork("acme.ddev.site", "wt-a--acme-wt-a.ddev.site")...)
	_, _, errOut := run(t, "", cmdDiff, args...)
	if !strings.Contains(errOut, "is not a canonical of this") {
		t.Errorf("a single-site worktree got no warning about an unmatched base:\n%s", errOut)
	}

	// And production-canonical still does not warn: there the baseline is the
	// project's own hostname by design.
	pc := t.TempDir()
	writeFile(t, pc, ".ddev/config.yaml", "name: acme-wt-a\n")
	writeFile(t, pc, "hostshift.yaml",
		"sites:\n  - canonical: https://www.acme.fi\n    variant: https://wt-a--acme.ddev.site\n")
	args = append([]string{"-C", pc, "--slug", "wt-a", "-n", "1",
		"--canonical-base", "https://acme-wt-a.ddev.site"},
		noNetwork("acme-wt-a.ddev.site", "wt-a--acme.ddev.site")...)
	_, _, errOut = run(t, "", cmdDiff, args...)
	if strings.Contains(errOut, "is not a canonical of this") {
		t.Errorf("the documented production-canonical baseline must not warn:\n%s", errOut)
	}
}

// TestR45ARequestTypeIsRefusedRatherThanPassedThrough.
//
// `rewrite --type application/x-www-form-urlencoded` printed the input back with
// an empty counter block and no diagnostic, which reads as "the engine found
// nothing in your body" when it means "this command never looked" — request
// bodies are rewritten by the proxy, in the other direction.
//
// Only the request types. Tier 2 pass-through is deliberate (§5.2), and piping a
// stylesheet through to see it unchanged is a real question with a true answer.
func TestR45ARequestTypeIsRefusedRatherThanPassedThrough(t *testing.T) {
	in := "url=https%3A%2F%2Fwww.acme.fi%2Fx"
	for _, ct := range []string{"application/x-www-form-urlencoded", "multipart/form-data"} {
		code, out, errOut := run(t, in, cmdRewrite,
			"--map", "https://www.acme.fi=https://wt-a--acme.ddev.site", "--type", ct)
		if code != exitConfig {
			t.Errorf("%s: exit %d, want %d", ct, code, exitConfig)
		}
		if strings.Contains(out, "url=") {
			t.Errorf("%s: the body was passed through: %q", ct, out)
		}
		if !strings.Contains(errOut, "rewrites") {
			t.Errorf("%s: no diagnostic: %q", ct, errOut)
		}
	}
	// And a Tier 2 type still passes through untouched, which is documented.
	css := "body{background:url(https://www.acme.fi/bg.png)}"
	code, out, _ := run(t, css, cmdRewrite,
		"--map", "https://www.acme.fi=https://wt-a--acme.ddev.site", "--type", "text/css", "--quiet")
	if code != exitOK || out != css {
		t.Errorf("text/css must stream through untouched: exit %d, %q", code, out)
	}
}
