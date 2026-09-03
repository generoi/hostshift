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

// Both sides of the pair, and both loops of the census.
//
// Round 58's mutation survey found each of these fixes pinned on one half only:
// `--pairs` was asserted on the canonical side, so the variant could go back to
// HostPort(); and `annotate` was asserted on its `covered` loop, so the
// `variant` loop could go back to comparing spellings that never meet — which
// puts the map's own variant into DirectlyServed and fires the
// canonical-on-production note on a correct project, verbatim the failure round
// 57 fixed one loop over.
func TestR58BothSidesOfTheIDNSpelling(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".ddev/config.yaml", "name: acme\nadditional_hostnames:\n  - hämeen\n")

	// The canonical side of *every* pair, not just the first. The database holds
	// canonicals, so this is the side §4.3 is about; the variant is derived and
	// never appears in the database, so its punycode spelling is a difference in
	// rendering rather than a round-trip defect, and is left alone deliberately.
	_, out, _ := run(t, "", cmdMap, "-C", dir, "--slug", "wt", "--pairs")
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		canonical, _, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if strings.Contains(canonical, "xn--") {
			t.Errorf("`map --pairs` emitted punycode for a canonical, so the\n"+
				"--from/--to line `init` writes hands the proxy an origin with no\n"+
				"Display and the request direction splices an A-label into a database\n"+
				"whose every other row holds the U-label (§4.3):\n%s", out)
		}
	}

}

// The census's *second* loop, on a fixture that actually arms the note.
//
// The first version of this assertion used an all-`.ddev.site` map, where
// ExternalCanonicals is empty and the canonical-on-production note never fires
// — so it passed whatever the loop did. The note needs an external canonical to
// arm, and the defect needs a DDEV hostname that *is* a variant, declared in the
// IDN spelling DDEV keeps. Then comparing an ACE variant against a declared one
// puts the map's own variant into DirectlyServed and the note tells the
// developer to avoid the very hostname the preview is served on.
func TestR58TheVariantCensusNormalisesToo(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".ddev/config.yaml",
		"name: acme\nadditional_hostnames:\n  - wt--hämeen\n")
	writeFile(t, dir, "hostshift.yaml",
		"sites:\n  - canonical: https://www.acme.example\n"+
			"    variant: https://wt--hämeen.ddev.site\n")

	_, _, errOut := run(t, "", cmdCheck, "-C", dir, "--slug", "wt")
	// Premise: the paragraph under test fired at all.
	if !strings.Contains(errOut, "canonical-on-production") {
		t.Fatalf("fixture: the note did not fire, so this asserts nothing:\n%s", errOut)
	}
	_, after, _ := strings.Cut(errOut, "canonical-on-production")
	note, _, _ := strings.Cut(after, "\nhostshift:")
	if strings.Contains(note, "hämeen") || strings.Contains(note, "xn--hmeen") {
		t.Errorf("the note names the map's own variant as a hostname that serves\n"+
			"the database unrewritten, because the variant census compares an ACE\n"+
			"hostname against the spelling DDEV keeps:\n%s", errOut)
	}
}
