package ddev

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestDDEVEnvNarrowsWebToThisProject is the regression test for a bug that only
// appears in a worktree sharing canonical's database.
//
// web must keep *this project's* hostnames minus the variants. Deriving them
// from the canonical set instead — which is what the first version did — hands
// a worktree's web container the hostnames of the canonical project, which is a
// different project that is still running and still owns them.
func TestDDEVEnvNarrowsWebToThisProject(t *testing.T) {
	// A worktree of acmecorp: its own project name, canonical's map.
	dir := t.TempDir()
	write(t, dir, ".ddev/config.yaml",
		"name: acmecorp-wt-tier2\nadditional_hostnames:\n  - wt2--acmecorp\n  - wt2--nat.acmecorp\n")
	write(t, dir, "hostshift.yaml", `
version: 1
sites:
  - {name: main, canonical: https://www.acmecorp.fi,    base: https://acmecorp.ddev.site}
  - {name: nat,  canonical: https://www.acmecorpnat.fi, base: https://nat.acmecorp.ddev.site}
`)
	p, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	wantVariants := []string{"wt2--acmecorp.ddev.site", "wt2--nat.acmecorp.ddev.site"}
	variants, webHosts := Env(p.Hosts, wantVariants)
	if strings.Join(variants, ",") != strings.Join(wantVariants, ",") {
		t.Errorf("variants = %v, want %v", variants, wantVariants)
	}
	// Only the worktree's own project hostname. Emitting acmecorp.ddev.site
	// here would make this project claim the canonical project's hostname.
	if strings.Join(webHosts, ",") != "acmecorp-wt-tier2.ddev.site" {
		t.Errorf("webHosts = %v, want just the worktree's own project hostname", webHosts)
	}
	for _, h := range webHosts {
		if strings.HasPrefix(h, "acmecorp.") || strings.HasPrefix(h, "nat.acmecorp.") {
			t.Errorf("web claims %q, which belongs to the canonical project", h)
		}
	}
}

// TestDDEVEnvForACanonicalProject: the same call on the canonical project keeps
// its own two hostnames and hands the variants to hostshift.
func TestDDEVEnvForACanonicalProject(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".ddev/config.yaml",
		"name: acmecorp\nadditional_hostnames:\n  - nat.acmecorp\n  - wt-a--acmecorp\n  - wt-a--nat.acmecorp\n")
	p, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	variants, webHosts := Env(p.Hosts,
		[]string{"wt-a--acmecorp.ddev.site", "wt-a--nat.acmecorp.ddev.site"})
	if strings.Join(variants, ",") != "wt-a--acmecorp.ddev.site,wt-a--nat.acmecorp.ddev.site" {
		t.Errorf("variants = %v", variants)
	}
	if strings.Join(webHosts, ",") != "acmecorp.ddev.site,nat.acmecorp.ddev.site" {
		t.Errorf("webHosts = %v, want the canonical project's own two hostnames", webHosts)
	}
}

// TestDDEVConfigOverridesAreMerged. DDEV merges every .ddev/config.*.yaml over
// config.yaml; hostshift read only the base file, so its view of the project
// could differ from the one DDEV was actually serving.
//
// That gap is exactly where a worktree lives. Two DDEV projects cannot share a
// name and .ddev/config.yaml is tracked, so a worktree that wants containers of
// its own has to override `name` somewhere else — and config.*.local.yaml,
// which DDEV's own .ddev/.gitignore ignores, is the only place that does not
// dirty the checkout. hostshift would have gone on resolving against the
// parent's name while DDEV served the worktree's, and every request would 421.
func TestDDEVConfigOverridesAreMerged(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".ddev/config.yaml",
		"name: acmecorp\nadditional_hostnames:\n  - nat.acmecorp\n")
	write(t, dir, ".ddev/config.worktree.local.yaml",
		"name: acmecorp-wt-a\nadditional_hostnames:\n  - nat.acmecorp\n  - extra.acmecorp\n")

	p, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "acmecorp-wt-a" {
		t.Errorf("name = %q; DDEV would say acmecorp-wt-a", p.Name)
	}
	if p.TLD != "ddev.site" {
		t.Errorf("tld = %q", p.TLD)
	}
	// Lists append and dedupe, which is what DDEV does — the pilot's own
	// override repeats a hostname already in config.yaml and the project ends
	// up with one of it.
	want := "acmecorp-wt-a.ddev.site,nat.acmecorp.ddev.site,extra.acmecorp.ddev.site"
	if strings.Join(p.Hosts, ",") != want {
		t.Errorf("hosts = %v\nwant  %s", p.Hosts, want)
	}
}

// TestDDEVProjectTLDOverride: project_tld can come from an override too.
func TestDDEVProjectTLDOverride(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".ddev/config.yaml", "name: fsi\n")
	write(t, dir, ".ddev/config.tld.local.yaml", "project_tld: ddev.local\n")
	p, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.TLD != "ddev.local" || p.Hosts[0] != "fsi.ddev.local" {
		t.Errorf("tld=%q hosts=%v", p.TLD, p.Hosts)
	}
}
