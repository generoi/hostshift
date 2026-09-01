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

	note, _, _ := strings.Cut(errOut, "Preview through the variant(s) instead:")
	if strings.Contains(note, "https://preview.ddev.site") {
		t.Errorf("the variant is routed to the proxy, not to web:\n%s", errOut)
	}
	// The project's own name still is directly served, so the note stands.
	if !strings.Contains(note, "https://acme.ddev.site") {
		t.Errorf("the note should still name the project's own hostname:\n%s", errOut)
	}
}
