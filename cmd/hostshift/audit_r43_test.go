package main

import (
	"strings"
	"testing"
)

// Round 43, on 1e4f62f ("Check that loopback containment exists; pair diff's
// bases by site").

// TestDiffPairsTheBasesBySiteFromTheVariantSideToo.
//
// 1e4f62f fixed `--canonical-base <site 2>` leaving the variant side at site 1's
// — "on a multisite that compares two different sites ... and the run printed
// GREEN". The pairing it added is guarded by
//
//	if *canonicalBase != "" && *variantBase == ""
//
// so it runs in one direction only. `--variant-base <site 2>` on its own leaves
// the *canonical* side at site 1's, which is the same two-different-sites
// comparison with the flags swapped, and it is not even warned about: the
// "is not a canonical of this N-site map" message is inside the same guard.
//
// The asymmetry has no justification in the commit's own reasoning. That
// reasoning is about why an *unmatched* base must stay legal — the documented
// production-canonical baseline is the project's own `<project>.ddev.site`,
// which is deliberately not a canonical. Every variant, by contrast, is in the
// map by construction, so the variant side is the one that can always be paired.
//
// The crawl reaches nothing here; the announced pair is printed before the first
// fetch and is the whole assertion.
func TestDiffPairsTheBasesBySiteFromTheVariantSideToo(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".ddev/config.yaml", "name: acme\n")
	writeFile(t, dir, "hostshift.yaml", ""+
		"sites:\n"+
		"  - canonical: https://www.acme.fi\n    variant: https://wt-a--acme.ddev.site\n"+
		"  - canonical: https://shop.acme.fi\n    variant: https://wt-a--shop.ddev.site\n")

	args := append([]string{"-C", dir, "--slug", "wt-a", "-n", "1",
		"--variant-base", "https://wt-a--shop.ddev.site"},
		noNetwork("shop.acme.fi", "www.acme.fi", "wt-a--shop.ddev.site")...)
	_, _, errOut := run(t, "", cmdDiff, args...)

	// The premise: the mirror flag really does name site 2, and site 2 really is
	// not site 1 — so anything that pairs would pair them.
	if !strings.Contains(errOut, "vs https://wt-a--shop.ddev.site") {
		t.Fatalf("fixture: --variant-base was not honoured at all:\n%s", errOut)
	}
	if strings.Contains(errOut, "corpus diff: https://www.acme.fi vs") &&
		!strings.Contains(errOut, "is not a variant of this 2-site") {
		t.Errorf("site 1's canonical was crawled against site 2's variant, in silence:\n%s", errOut)
	}
}

// TestACanonicalUnderTestIsNotOnProduction.
//
// `ExternalCanonicals` decides two things: whether `check` prints the
// canonical-on-production note ("every link on them points at the live site"),
// and which hostnames the add-on demands loopback containment for. 1e4f62f
// changed its test from "is not one of this project's own hostnames" to
//
//	!strings.HasSuffix(o.Host, "."+proj.TLD)
//
// on the reasoning that "how a name resolves is the question, not who owns it".
// The new test answers that question only for the project TLD. It has no answer
// for `additional_fqdns`, which DDEV registers in /etc/hosts and which
// `hostshift hosts` prints verbatim — the add-on's own variant check spends a
// paragraph on exactly this ("DDEV registers only the *exact* FQDN in
// /etc/hosts") and consults the hostname list for it.
//
// So a project with `additional_fqdns: [acme.test]` and no hostshift.yaml — a
// stock DDEV-canonical project, its database holding local URLs and nothing on
// any page leaving the machine — is told its map is canonical-on-production and
// that web can reach `acme.test` for real.
//
// `.test` is reserved by RFC 6761 and is never delegated, so it cannot be a live
// site by definition; and this same binary already says so — `isLoopbackHost` in
// cmd/hostshift/main.go returns true for `.test`, and `diff` uses it to decide
// whether a crawl would hit production. Two functions in one binary answering
// the same question two ways, which is the shape the commit message itself
// calls out as "the shape of half this project's bugs".
//
// This is the failure mode e37c0b0 spent half a commit fixing: a warning about
// production URLs on `ddev start` of a project that has none, on the post-start
// hook, which is how a warning stops being read.
func TestACanonicalUnderTestIsNotOnProduction(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".ddev/config.yaml",
		"name: acme\nadditional_fqdns:\n  - acme.test\n")

	// Premise, and a check on my own fixture: the binary's own loopback test
	// says this hostname stays on the machine.
	if !isLoopbackHost("acme.test") {
		t.Fatal("fixture: isLoopbackHost disagrees, so this test is about something else")
	}

	_, out, _ := run(t, "", cmdMap, "-C", dir, "--slug", "wt-a", "--external-canonical-hosts")
	if strings.Contains(out, "acme.test") {
		t.Errorf("a hostname DDEV registers in /etc/hosts is in the containment set:\n%s", out)
	}

	_, _, errOut := run(t, "", cmdCheck, "-C", dir, "--slug", "wt-a")
	if strings.Contains(errOut, "canonical-on-production") {
		t.Errorf("a stock project with an additional_fqdn is called canonical-on-production:\n%s",
			errOut)
	}
}

// TestTheNoteNamesAVariantYouCanActuallyReach.
//
// The note e37c0b0 added ends by telling the developer where to go instead, and
// it renders that with
//
//	fmt.Fprintf(os.Stderr, "  https://%s\n", st.Variant.Host)
//
// — a hardcoded scheme and `Host`, which on an Origin is the hostname with the
// port in a separate field. The map is origin→origin, scheme, host *and* port
// (PLAN §5.5), and `corpus.redirectsToItself` carries a comment about this exact
// mistake ("HostPort, not Host: an Origin keeps its port in a separate field").
// The diff warning added in the very next commit renders its variant with
// `st.Variant.String()` and gets all three.
//
// So on a variant that is not https on 443, the one line in the note that tells
// the developer where to go points at a URL nothing is listening on — inside a
// paragraph whose entire purpose is to steer them off the page that serves live
// production links. The page they were warned away from is the one that works.
func TestTheNoteNamesAVariantYouCanActuallyReach(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".ddev/config.yaml", "name: acme-wt-a\n")
	writeFile(t, dir, "hostshift.yaml",
		"sites:\n  - canonical: https://www.acme.fi\n"+
			"    variant: http://wt-a--acme.ddev.site:8080\n")

	_, _, errOut := run(t, "", cmdCheck, "-C", dir, "--slug", "wt-a")
	// Premise: this is the paragraph under test.
	if !strings.Contains(errOut, "Preview through the variant(s) instead") {
		t.Fatalf("fixture: the note did not fire:\n%s", errOut)
	}
	if !strings.Contains(errOut, "http://wt-a--acme.ddev.site:8080") {
		t.Errorf("the note points the developer at a URL that is not the variant:\n%s", errOut)
	}
}
