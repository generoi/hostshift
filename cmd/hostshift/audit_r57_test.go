package main

import (
	"strings"
	"testing"
)

// Round 57, on d017b64 ("Draw the grid, and see the states check could not
// see"), auditing the map/config layer where it crosses what `init` writes.

// TestMapPairsPreservesTheDeclaredSpelling.
//
// `ddev hostshift init` cannot hand the container a directory it cannot see, so
// for a project with no hostshift.yaml it resolves the map on the host and hands
// it over flat:
//
//	while IFS='=' read -r c v; do hsmap="$hsmap --from $c --to $v"
//	done <<<"$(hostshift map --slug "$slug" --pairs)"
//
// `--pairs` prints `s.Canonical.String()`, and String() renders HostPort() —
// the punycode comparison form. That is the one thing this package's own
// origin.go says String() is not for: "This is for building a *replacement*, and
// for diagnostics. It is never used to round-trip input."
//
// So Origin.Display, added by acce8c6 ("Preserve the spelling an IDN was
// declared with, on the way back") for exactly this failure, does not survive
// the handoff: `--from https://xn--hmeen-gra.ddev.site` re-parses inside the
// container to an all-ASCII declaration, Display is empty, and the *request*
// direction then splices the A-label. A block-editor save on the preview writes
// `xn--hmeen-gra.ddev.site` into the shared database where every other row holds
// the U-label — §4.3, through the supported install path, and the exact defect
// acce8c6 fixed one layer up.
//
// DisplayHostPort is what the map's own replacements use; the pairs line must
// use it too, or the two halves of the same tool disagree about the same host.
func TestMapPairsPreservesTheDeclaredSpelling(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".ddev/config.yaml", "name: acme\nadditional_hostnames:\n  - hämeen\n")

	_, out, _ := run(t, "", cmdMap, "-C", dir, "--slug", "wt", "--pairs")
	if !strings.Contains(out, "hämeen.ddev.site=") {
		t.Errorf("`map --pairs` dropped the spelling the hostname was declared with,\n"+
			"so the --from/--to line `init` writes hands the proxy an origin with no\n"+
			"Display and the request direction splices punycode into the shared\n"+
			"database (§4.3):\n%s", out)
	}
}

// TestCheckDoesNotCallACoveredHostUncovered.
//
// annotate() builds `covered` and `variant` from origin.Host — punycode — and
// then tests DDEV's registered hostnames against them, which are the strings the
// developer wrote in .ddev/config.yaml. For an IDN hostname the two spellings
// never meet, so the host is reported as one "this map does not cover" while it
// is in fact that map's own canonical, and it is also added to DirectlyServed,
// where it feeds the canonical-on-production note.
//
// Both are printed by `ddev hostshift check`, which is the post-start hook — so
// this is a warning on every single `ddev start` of a correctly configured
// project, which is how a warning stops being read. This file's own comments
// make that argument twice.
func TestCheckDoesNotCallACoveredHostUncovered(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".ddev/config.yaml", "name: acme\nadditional_hostnames:\n  - hämeen\n")

	_, _, errOut := run(t, "", cmdCheck, "-C", dir, "--slug", "wt")
	if strings.Contains(errOut, "does not cover") {
		t.Errorf("`check` reported its own canonical as uncovered, because the census\n"+
			"is keyed on punycode and DDEV's hostnames are not:\n%s", errOut)
	}
}
