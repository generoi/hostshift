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
		"name: herrfors\nadditional_hostnames:\n  - nat.herrfors\n")
	write(t, dir, ".ddev/config.worktree.local.yaml",
		"name: herrfors-wt-a\nadditional_hostnames:\n  - nat.herrfors\n  - extra.herrfors\n")

	p, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "herrfors-wt-a" {
		t.Errorf("name = %q; DDEV would say herrfors-wt-a", p.Name)
	}
	if p.TLD != "ddev.site" {
		t.Errorf("tld = %q", p.TLD)
	}
	// Lists append and dedupe, which is what DDEV does — the pilot's own
	// override repeats a hostname already in config.yaml and the project ends
	// up with one of it.
	want := "herrfors-wt-a.ddev.site,nat.herrfors.ddev.site,extra.herrfors.ddev.site"
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
