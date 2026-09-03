package main

import (
	"strings"
	"testing"
)

// TestR50BothBasesBecomeTheMapWhenItKnowsNeither.
//
// `--canonical-base` and `--variant-base` moved only the crawl; the rewriting
// map still came from `-C`/`--slug`, which in a worktree resolves to the
// worktree's own DDEV hostnames. Those appear on neither side of the
// comparison, so the canonical body was compared unrewritten and the leak scan
// looked for an origin that could not occur — 0 leaks and "no canonical origin
// reached the browser" over four pages carrying 193 of them, on the invocation
// README documents for worktrees.
func TestR50BothBasesBecomeTheMapWhenItKnowsNeither(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".ddev/config.yaml", "name: acme-wt-a\n")

	args := append([]string{"-C", dir, "--slug", "wt-a", "-n", "1",
		"--canonical-base", "https://acme.ddev.site",
		"--variant-base", "https://wt-a--acme.ddev.site"},
		noNetwork("acme.ddev.site", "wt-a--acme.ddev.site")...)
	_, _, errOut := run(t, "", cmdDiff, args...)

	if !strings.Contains(errOut, "neither base is in the map") {
		t.Errorf("the run did not say the map it was given is irrelevant:\n%s", errOut)
	}
	if !strings.Contains(errOut, "corpus diff: https://acme.ddev.site vs https://wt-a--acme.ddev.site") {
		t.Errorf("the announced pair is wrong:\n%s", errOut)
	}

	// And a map that *does* know a base is left alone — production-canonical,
	// where the baseline is deliberately a third hostname and the variant is in
	// the map.
	pc := t.TempDir()
	writeFile(t, pc, ".ddev/config.yaml", "name: acme-wt-a\n")
	writeFile(t, pc, "hostshift.yaml",
		"sites:\n  - canonical: https://www.acme.fi\n    variant: https://wt-a--acme.ddev.site\n")
	args = append([]string{"-C", pc, "--slug", "wt-a", "-n", "1",
		"--canonical-base", "https://acme-wt-a.ddev.site",
		"--variant-base", "https://wt-a--acme.ddev.site"},
		noNetwork("acme-wt-a.ddev.site", "wt-a--acme.ddev.site")...)
	_, _, errOut = run(t, "", cmdDiff, args...)
	if strings.Contains(errOut, "neither base is in the map") {
		t.Errorf("the production-canonical map was replaced, and it is the right one:\n%s", errOut)
	}
}

// TestR50AnUnrewrittenTypeSaysSo: `--type text/htm` — one character from
// `text/html` — printed the input back with an empty counter block and exit 0,
// which reads as "the engine found nothing here" rather than "that is not a type
// I rewrite". Tier 2 still passes through, by design; it just says why.
func TestR50AnUnrewrittenTypeSaysSo(t *testing.T) {
	css := "body{background:url(https://www.acme.fi/bg.png)}"
	code, out, errOut := run(t, css, cmdRewrite,
		"--map", "https://www.acme.fi=https://wt-a--acme.ddev.site", "--type", "text/css")
	if code != exitOK || out != css {
		t.Errorf("a Tier 2 type must still stream through: exit %d, %q", code, out)
	}
	if !strings.Contains(errOut, "outside the rewritable set") {
		t.Errorf("nothing said the type was not rewritten:\n%s", errOut)
	}
	// --quiet is the machine-readable mode and stays silent.
	_, _, errOut = run(t, css, cmdRewrite,
		"--map", "https://www.acme.fi=https://wt-a--acme.ddev.site", "--type", "text/css", "--quiet")
	if strings.Contains(errOut, "outside the rewritable set") {
		t.Errorf("--quiet should say nothing:\n%s", errOut)
	}
}
