package main

import (
	"strings"
	"testing"
)

// Round 42. `check` gained a note in 94d2cd0 naming the hostname DDEV
// advertises, because under production-canonical `web` answers on it with the
// shared production database unrewritten — the URL `ddev start` prints, `ddev
// describe` lists and `ddev launch` opens is the one page on the machine where
// every link is the client's live site.
//
// The first version keyed on "this project's own hostname is a canonical of the
// map", which is not the production-canonical condition — it is *also* true of
// every stock DDEV project, where the canonicals are the `.ddev.site` hostnames
// by construction. `ddev hostshift check` is the post-start hook, so it printed
// a paragraph about production URLs on every `ddev start` of every project that
// has none. A warning that fires on healthy runs is the failure mode the other
// half of that same commit was written to fix.
//
// The condition is two things at once: some canonical is not a hostname of this
// project (the database holds URLs that dereference off the machine), and this
// project has hostnames that route to `web` rather than to the proxy (there is
// somewhere for them to be served from).

// productionCanonical: a worktree with its own DDEV project, mapping the
// client's live domain onto a variant. This is the shape round 41 found: `ddev
// describe` hands over acme-wt-a.ddev.site, which is neither canonical nor
// variant, and web serves the shared database on it.
func productionCanonical(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, ".ddev/config.yaml", "name: acme-wt-a\n")
	writeFile(t, dir, "hostshift.yaml",
		"sites:\n  - canonical: https://www.acme.fi\n    variant: https://wt-a--acme.ddev.site\n")
	return dir
}

func TestCheckNamesTheHostnameDDEVAdvertises(t *testing.T) {
	_, _, errOut := run(t, "", cmdCheck, "-C", productionCanonical(t), "--slug", "wt-a")
	for _, want := range []string{
		"canonical-on-production",
		"www.acme.fi",
		"https://acme-wt-a.ddev.site",
		"https://wt-a--acme.ddev.site",
	} {
		if !strings.Contains(errOut, want) {
			t.Errorf("the note does not mention %q:\n%s", want, errOut)
		}
	}
}

// TestCheckIsSilentOnAStockProject: no hostshift.yaml, so the map is the DDEV
// defaults and every canonical is one of this project's own hostnames. The
// database holds `.ddev.site` URLs; nothing on that page leaves the machine.
// This is the ordinary `ddev start`, and it must say nothing.
func TestCheckIsSilentOnAStockProject(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".ddev/config.yaml", "name: stock\nadditional_hostnames:\n  - nat\n")
	_, _, errOut := run(t, "", cmdCheck, "-C", dir, "--slug", "wt-b")
	if strings.Contains(errOut, "canonical-on-production") {
		t.Errorf("a stock DDEV project is not canonical-on-production:\n%s", errOut)
	}
}

// TestCheckNamesEveryDirectlyServedHostname: DDEV registers every
// additional_hostname, `ddev describe` lists all of them, and each one is served
// by web. Naming only the first leaves the developer a live-production URL the
// note said nothing about — and DDEV_HOSTNAME's order is not something to rely
// on for which one that is.
func TestCheckNamesEveryDirectlyServedHostname(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".ddev/config.yaml", "name: acme-wt-a\nadditional_hostnames:\n  - staff\n")
	writeFile(t, dir, "hostshift.yaml",
		"sites:\n  - canonical: https://www.acme.fi\n    variant: https://wt-a--acme.ddev.site\n")
	_, _, errOut := run(t, "", cmdCheck, "-C", dir, "--slug", "wt-a")
	for _, want := range []string{"https://acme-wt-a.ddev.site", "https://staff.ddev.site"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("the note does not name %q:\n%s", want, errOut)
		}
	}
}

// TestAVariantIsNotDirectlyServed: a map may point a canonical at a hostname the
// project already registers — `additional_hostnames: [preview]` with
// `variant: https://preview.ddev.site` is an ordinary way to ask for the preview
// at a fixed name rather than a slug-derived one. DDEV registers it, so it is in
// the project's hostnames, but the compose file's VIRTUAL_HOST narrowing routes
// it to the proxy rather than to web.
//
// It is therefore the one hostname on the project that is *not* serving the
// database unrewritten, and listing it under "every link on them points at the
// live site" would send the developer away from the only safe URL there is.
func TestAVariantIsNotDirectlyServed(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".ddev/config.yaml", "name: acme\nadditional_hostnames:\n  - preview\n")
	writeFile(t, dir, "hostshift.yaml",
		"sites:\n  - canonical: https://www.acme.fi\n    variant: https://preview.ddev.site\n")
	_, _, errOut := run(t, "", cmdCheck, "-C", dir, "--slug", "wt-a")

	note, _, _ := strings.Cut(errOut, "The variant(s) this map resolves to:")
	if strings.Contains(note, "https://preview.ddev.site") {
		t.Errorf("the variant is routed to the proxy, not to web:\n%s", errOut)
	}
	// The project's own name still is directly served, so the note stands.
	if !strings.Contains(note, "https://acme.ddev.site") {
		t.Errorf("the note should still name the project's own hostname:\n%s", errOut)
	}
}

// noNetwork points every host a diff test would fetch at a closed local port, so
// the crawl fails instantly instead of resolving and dialling the real internet.
// Without it these tests spend a second and a half each on a DNS lookup for a
// client's actual domain — from a unit suite, which is the wrong place to be
// making that request at all.
func noNetwork(hosts ...string) []string {
	var out []string
	for _, h := range hosts {
		out = append(out, "--resolve", h+":443:127.0.0.1:1", "--resolve", h+":80:127.0.0.1:1")
	}
	return append(out, "--timeout", "2s")
}

// TestDiffPairsTheBasesBySite: on a multisite, --canonical-base names one site
// and the variant side has to follow it. It used to override the canonical
// alone, so `--canonical-base <site 2>` was crawled against site 1's variant —
// two different sites, every page differing for the obvious reason, and the run
// printed GREEN. `diff` is the command the README calls the check that validates
// a deployment against reality.
//
// The crawl itself cannot reach anything here; the announced pair is what this
// asserts, and it is printed before the first fetch.
func TestDiffPairsTheBasesBySite(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".ddev/config.yaml", "name: acme\n")
	writeFile(t, dir, "hostshift.yaml", ""+
		"sites:\n"+
		"  - canonical: https://www.acme.fi\n    variant: https://wt-a--acme.ddev.site\n"+
		"  - canonical: https://shop.acme.fi\n    variant: https://wt-a--shop.ddev.site\n")

	args := append([]string{"-C", dir, "--slug", "wt-a", "-n", "1",
		"--canonical-base", "https://shop.acme.fi"},
		noNetwork("shop.acme.fi", "wt-a--shop.ddev.site")...)
	_, _, errOut := run(t, "", cmdDiff, args...)
	if !strings.Contains(errOut, "corpus diff: https://shop.acme.fi vs https://wt-a--shop.ddev.site") {
		t.Errorf("the second site's base was not paired with its own variant:\n%s", errOut)
	}
}

// TestDiffSaysWhenABaseBelongsToNoSite: the production-canonical baseline is the
// project's own `<project>.ddev.site`, which is deliberately not a canonical of
// the map, so an unmatched base cannot be an error. On a multisite it is still
// ambiguous — there is no site for it to pair with — and the pair being compared
// has to be visible rather than "whichever site was written first".
func TestDiffSaysWhenABaseBelongsToNoSite(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".ddev/config.yaml", "name: acme\n")
	writeFile(t, dir, "hostshift.yaml", ""+
		"sites:\n"+
		"  - canonical: https://www.acme.fi\n    variant: https://wt-a--acme.ddev.site\n"+
		"  - canonical: https://shop.acme.fi\n    variant: https://wt-a--shop.ddev.site\n")

	args := append([]string{"-C", dir, "--slug", "wt-a", "-n", "1",
		"--canonical-base", "https://acme-wt-b.ddev.site"},
		noNetwork("acme-wt-b.ddev.site", "wt-a--acme.ddev.site")...)
	_, _, errOut := run(t, "", cmdDiff, args...)
	if !strings.Contains(errOut, "is not a canonical of this 2-site") {
		t.Errorf("an unrelated base was accepted in silence:\n%s", errOut)
	}
}

// TestExternalCanonicalHostsIsTheContainmentSet: loopback containment exists for
// the canonical hostnames that leave the machine. Under DDEV-canonical there are
// none and the add-on must not ask for a containment file; under
// production-canonical it is exactly the client's domains, aliases included.
// The add-on asks this rather than deriving it, so the two cannot drift.
func TestExternalCanonicalHostsIsTheContainmentSet(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".ddev/config.yaml", "name: acme\n")
	writeFile(t, dir, "hostshift.yaml", ""+
		"sites:\n"+
		"  - canonical: https://www.acme.fi\n"+
		"    aliases:\n      - https://acme.staging.example\n"+
		"    variant: https://wt-a--acme.ddev.site\n")
	_, out, _ := run(t, "", cmdMap, "-C", dir, "--slug", "wt-a", "--external-canonical-hosts")
	for _, want := range []string{"www.acme.fi", "acme.staging.example"} {
		if !strings.Contains(out, want) {
			t.Errorf("the containment set omits %q:\n%s", want, out)
		}
	}

	stock := t.TempDir()
	writeFile(t, stock, ".ddev/config.yaml", "name: stock\nadditional_hostnames:\n  - nat\n")
	_, out, _ = run(t, "", cmdMap, "-C", stock, "--slug", "wt-a", "--external-canonical-hosts")
	if strings.TrimSpace(out) != "" {
		t.Errorf("a stock project has nothing to contain, got:\n%s", out)
	}
}

// TestAFlatMapGetsTheSameDiagnostics: `--from/--to` returned before the DDEV
// project was loaded, on the reasoning that an explicit map needs no files. True
// of the map; false of everything that describes how the map sits against the
// project. A production-canonical map handed over as flags — which is how the
// add-on hands one over whenever it is not mounting a hostshift.yaml — got no
// note about the hostname DDEV advertises and no containment set, so both
// guardrails were switched off by how the map was spelled.
func TestAFlatMapGetsTheSameDiagnostics(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".ddev/config.yaml", "name: acme-wt-a\n")
	flags := []string{"-C", dir,
		"--from", "https://www.acme.fi", "--to", "https://wt-a--acme.ddev.site"}

	_, _, errOut := run(t, "", cmdCheck, flags...)
	if !strings.Contains(errOut, "canonical-on-production") {
		t.Errorf("a flat production-canonical map got no note:\n%s", errOut)
	}
	if !strings.Contains(errOut, "https://acme-wt-a.ddev.site") {
		t.Errorf("the note does not name the directly-served hostname:\n%s", errOut)
	}

	_, out, _ := run(t, "", cmdMap, append(flags, "--external-canonical-hosts")...)
	if !strings.Contains(out, "www.acme.fi") {
		t.Errorf("a flat map has an empty containment set:\n%s", out)
	}
}

// TestAnOrdinaryWorktreeIsNotCanonicalOnProduction: the canonicals of a
// DDEV-canonical worktree are the *parent project's* `.ddev.site` hostnames.
// They are not this project's own, and the first version of the condition
// tested exactly that — so every ordinary worktree was told its links pointed at
// a live site. `*.ddev.site` is a public record pointing at the loopback; what
// decides whether a name can leave the machine is the TLD, which is also the
// test `ddev hostshift loopback` applies when it writes the containment file.
func TestAnOrdinaryWorktreeIsNotCanonicalOnProduction(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".ddev/config.yaml", "name: acme-wt-a\n")
	flags := []string{"-C", dir,
		"--from", "https://acme.ddev.site", "--to", "https://wt-a--acme.ddev.site"}

	_, _, errOut := run(t, "", cmdCheck, flags...)
	if strings.Contains(errOut, "canonical-on-production") {
		t.Errorf("a DDEV-canonical worktree is not canonical-on-production:\n%s", errOut)
	}
	_, out, _ := run(t, "", cmdMap, append(flags, "--external-canonical-hosts")...)
	if strings.TrimSpace(out) != "" {
		t.Errorf("a local hostname needs no containment, got:\n%s", out)
	}
}
