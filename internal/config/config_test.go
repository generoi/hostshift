package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// bravoincYAML is the real five-blog map, taken from the repo's robo.yml. The
// production domains are unrelated registrable domains, which is the norm and
// not the exception (PLAN §4.2) — any suffix-derived mapping is dead.
const bravoincYAML = `
version: 1
upstream: http://web:80
sites:
  - name: main
    canonical: https://bravoinc.bravoinc.example-dev.com
    base:      https://bravoinc.ddev.site
  - name: kodin
    canonical: https://kodinbravoinc.fi
    base:      https://kodinbravoinc.ddev.site
  - name: maatilan
    canonical: https://maatilanbravoinc.fi
    base:      https://maatilanbravoinc.ddev.site
  - name: omapiha
    canonical: https://omapiha.info
    base:      https://omapiha.ddev.site
  - name: otlehti
    canonical: https://otlehti.bravoinc.example-dev.com
    base:      https://otlehti.ddev.site
`

// TestFiveBlogSite is acceptance test 10a.
func TestFiveBlogSite(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "hostshift.yaml", bravoincYAML)

	res, err := Load(dir, Flags{Slug: "wt-a"})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(res.Map.Sites); n != 5 {
		t.Fatalf("%d sites, want 5", n)
	}
	if res.Upstream != "http://web:80" {
		t.Errorf("upstream %q, want it from the file", res.Upstream)
	}

	want := map[string]string{
		"main":     "https://wt-a--bravoinc.ddev.site",
		"kodin":    "https://wt-a--kodinbravoinc.ddev.site",
		"maatilan": "https://wt-a--maatilanbravoinc.ddev.site",
		"omapiha":  "https://wt-a--omapiha.ddev.site",
		"otlehti":  "https://wt-a--otlehti.ddev.site",
	}
	for _, s := range res.Map.Sites {
		if got := s.Variant.String(); got != want[s.Name] {
			t.Errorf("site %q variant %q, want %q", s.Name, got, want[s.Name])
		}
		// The base host is kept as an alias, so a residual @ddev URL is
		// corrected too (test 10c).
		if len(s.Aliases) != 1 {
			t.Errorf("site %q has %d aliases, want the base host kept as one", s.Name, len(s.Aliases))
		}
	}

	// Each blog routes to its own canonical host — the sibling's, not the
	// network's main host.
	site, ok := res.Map.SiteForHost("wt-a--omapiha.ddev.site")
	if !ok {
		t.Fatal("the omapiha variant host does not resolve")
	}
	if site.Canonical.Host != "omapiha.info" {
		t.Errorf("omapiha resolves to %q, want omapiha.info", site.Canonical.Host)
	}
}

// TestDDEVDefaults is acceptance test 10e: name + additional_hostnames and no
// hostshift.yaml produce a working map. For a single-environment site this is
// sufficient on its own, and DDEV is the only third-party format hostshift
// reads (PLAN §5.3).
func TestDDEVDefaults(t *testing.T) {
	dir := t.TempDir()
	// Verbatim from acmecorp/.ddev/config.yaml: the additional hostname is
	// "nat.acmecorp", not "nat", so the local hosts come out fully qualified.
	write(t, dir, ".ddev/config.yaml", "name: acmecorp\nadditional_hostnames:\n  - nat.acmecorp\n")

	res, err := Load(dir, Flags{Slug: "wt-a"})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(res.Map.Sites); n != 2 {
		t.Fatalf("%d sites, want 2", n)
	}
	want := [][2]string{
		{"https://acmecorp.ddev.site", "https://wt-a--acmecorp.ddev.site"},
		{"https://nat.acmecorp.ddev.site", "https://wt-a--nat.acmecorp.ddev.site"},
	}
	for i, s := range res.Map.Sites {
		if s.Canonical.String() != want[i][0] || s.Variant.String() != want[i][1] {
			t.Errorf("site %d is %s -> %s, want %s -> %s",
				i, s.Canonical, s.Variant, want[i][0], want[i][1])
		}
	}
	if !strings.Contains(res.Source, "DDEV defaults") {
		t.Errorf("source is %q, want it to name the DDEV layer", res.Source)
	}
}

// TestDDEVUnrelatedLocalBases: snellmanecom's local hosts are
// snellmanecom.ddev.site, shop.snellman.ddev.site and tilaus.figen.ddev.site —
// three different bases (PLAN §4.2). Prefixing the leftmost label derives each
// from its own host and needs no knowledge of which label is the project name,
// which is exactly why §5.4's rule is the leftmost-label one.
func TestDDEVUnrelatedLocalBases(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".ddev/config.yaml", `name: snellmanecom
additional_hostnames:
  - snellmanpetfood
  - mushb2b.snellmanecom
  - shop.snellman
  - tilaus.figen
`)
	res, err := Load(dir, Flags{Slug: "wt-a"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"https://wt-a--snellmanecom.ddev.site",
		"https://wt-a--snellmanpetfood.ddev.site",
		"https://wt-a--mushb2b.snellmanecom.ddev.site",
		"https://wt-a--shop.snellman.ddev.site",
		"https://wt-a--tilaus.figen.ddev.site",
	}
	for i, s := range res.Map.Sites {
		if got := s.Variant.String(); got != want[i] {
			t.Errorf("site %d variant %q, want %q", i, got, want[i])
		}
	}
}

// TestDDEVProjectTLD: project_tld is honoured rather than assumed.
func TestDDEVProjectTLD(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".ddev/config.yaml", "name: x\nproject_tld: ddev.local\n")
	res, err := Load(dir, Flags{Slug: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Map.Sites[0].Canonical.String(); got != "https://x.ddev.local" {
		t.Errorf("canonical %q, want the configured project_tld", got)
	}
}

// TestHostshiftYAMLOverridesDDEV: layer 2 wins over layer 1 (PLAN §5.3).
func TestHostshiftYAMLOverridesDDEV(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".ddev/config.yaml", "name: acmecorp\nadditional_hostnames:\n  - nat\n")
	write(t, dir, "hostshift.yaml", `
version: 1
sites:
  - name: main
    canonical: https://www.acmecorp.fi
    base: https://acmecorp.ddev.site
`)
	res, err := Load(dir, Flags{Slug: "wt-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Map.Sites) != 1 || res.Map.Sites[0].Canonical.Host != "www.acmecorp.fi" {
		t.Errorf("hostshift.yaml did not override the DDEV defaults: %s", res.Map)
	}
}

// TestFlagsOverrideEverything: layer 3 (PLAN §5.3), so the tool is usable with
// no config files at all.
func TestFlagsOverrideEverything(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "hostshift.yaml", `
version: 1
sites:
  - name: main
    canonical: https://www.acmecorp.fi
    base: https://acmecorp.ddev.site
`)
	res, err := Load(dir, Flags{
		From: []string{"https://a.example"},
		To:   []string{"https://b.example"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Map.Sites) != 1 || res.Map.Sites[0].Canonical.Host != "a.example" {
		t.Errorf("--from/--to did not replace the file: %s", res.Map)
	}
}

// TestExplicitVariantOverridesDerivation covers the rare case §5.3 allows for,
// and the plain-HTTP-listener shape §5.3 insists the map must express.
func TestExplicitVariantOverridesDerivation(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "hostshift.yaml", `
version: 1
sites:
  - name: main
    canonical: https://www.acmecorp.fi
    base: https://acmecorp.ddev.site
    variant: http://127.0.0.1:8080
`)
	res, err := Load(dir, Flags{}) // no --slug needed when variant is explicit
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Map.Sites[0].Variant.String(); got != "http://127.0.0.1:8080" {
		t.Errorf("variant %q, want the explicit one", got)
	}
}

// TestRejects covers the startup validation of PLAN §5.3 and §5.4: tests 10d,
// 17 and 29c. Each must fail loudly rather than mis-map at runtime.
func TestRejects(t *testing.T) {
	cases := []struct {
		name, yaml, slug, want string
	}{
		{
			// Test 10d: canonical sets overlap between sites.
			name: "canonical declared by two sites",
			yaml: `
sites:
  - {name: a, canonical: https://c.example, base: https://a.ddev.site}
  - {name: b, canonical: https://c.example, base: https://b.ddev.site}`,
			slug: "wt", want: "declared by both",
		},
		{
			// Test 10d again, via an alias rather than the primary.
			name: "alias collides with another site's canonical",
			yaml: `
sites:
  - {name: a, canonical: https://a.example, base: https://a.ddev.site, aliases: [https://b.example]}
  - {name: b, canonical: https://b.example, base: https://b.ddev.site}`,
			slug: "wt", want: "declared by both",
		},
		{
			// Two sites deriving the same variant host.
			name: "variants collide",
			yaml: `
sites:
  - {name: a, canonical: https://a.example, variant: https://v.ddev.site}
  - {name: b, canonical: https://b.example, variant: https://v.ddev.site}`,
			want: "derive the same variant host",
		},
		{
			// Tests 17 and 29c. What is rejected is not overlap as such — §5.4
			// permits containment — but a variant that a canonical origin token
			// *matches*, which would double-rewrite.
			name: "variant is matched by a canonical origin",
			yaml: `
sites:
  - {name: a, canonical: https://c.example, variant: https://c.example:8081}
  - {name: b, canonical: https://c.example:8081, variant: https://v.example}`,
			want: "collides",
		},
		{
			name: "empty sites",
			yaml: "sites: []",
			want: "`sites` is empty",
		},
		{
			name: "no slug and no variant",
			yaml: `
sites:
  - {name: a, canonical: https://c.example, base: https://a.ddev.site}`,
			want: "no variant",
		},
		{
			name: "unsupported version",
			yaml: `
version: 2
sites:
  - {name: a, canonical: https://c.example, variant: https://v.example}`,
			want: "version 2 is not supported",
		},
		{
			name: "canonical without a scheme",
			yaml: `
sites:
  - {name: a, canonical: c.example, variant: https://v.example}`,
			want: "scheme is required",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, "hostshift.yaml", c.yaml)
			_, err := Load(dir, Flags{Slug: c.slug})
			if err == nil {
				t.Fatalf("Load accepted a config it must refuse")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error is %q, want it to mention %q", err, c.want)
			}
		})
	}
}

// TestContainmentIsPermitted is the other half of tests 17 and 29c, and the one
// that matters more: §5.4 permits a variant host to contain a canonical host,
// because anchoring makes it safe. A substring ban would forbid the whole
// leftmost-label prefix scheme and there would be no way to derive variants at
// all.
func TestContainmentIsPermitted(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "hostshift.yaml", `
version: 1
sites:
  - {name: main, canonical: https://acmecorp.ddev.site}
  - {name: nat,  canonical: https://nat.acmecorp.ddev.site}
`)
	res, err := Load(dir, Flags{Slug: "wt-a"})
	if err != nil {
		t.Fatalf("a legitimate leftmost-label map was rejected: %v", err)
	}
	if got := res.Map.Sites[0].Variant.Host; got != "wt-a--acmecorp.ddev.site" {
		t.Errorf("variant host %q", got)
	}
	// The invariant §5.4 actually states: the automaton finds nothing in the
	// variant, so a second pass is a fixed point.
	for _, s := range res.Map.Sites {
		probe := []byte(s.Variant.String() + "/x")
		out, _ := res.Map.Forward().Rewrite(probe, "test", false)
		if string(out) != string(probe) {
			t.Errorf("the automaton matched inside variant %s: %q", s.Variant, out)
		}
	}
}

// TestNoConfigAtAll: the failure has to name what is missing.
func TestNoConfigAtAll(t *testing.T) {
	_, err := Load(t.TempDir(), Flags{})
	if err == nil {
		t.Fatal("Load succeeded with no config and no flags")
	}
	for _, want := range []string{"hostshift.yaml", ".ddev/config.yaml", "--from"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestSlugMustBeAHostnameLabel is the regression test for a config that `check`
// called "injective and anchored" while every request to it returned 421.
//
// A slug is a worktree slug, which in practice is a branch name — so uppercase
// and "/" are the common case. deriveVariant assembled an Origin by hand,
// skipping normalisation, so "feature/ABC-123" produced a host SiteForHost
// could never match because it lowercases the incoming Host.
func TestSlugMustBeAHostnameLabel(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".ddev/config.yaml", "name: acmecorp\nadditional_hostnames:\n  - nat.acmecorp\n")

	for _, bad := range []string{"feature/ABC-123", "wt a", "wt-a.", "wt_a."} {
		if _, err := Load(dir, Flags{Slug: bad}); err == nil {
			t.Errorf("--slug %q was accepted; it cannot be a hostname label", bad)
		}
	}

	// Uppercase alone is fine — it normalises, and must normalise, because the
	// map is keyed on the lowercased host.
	res, err := Load(dir, Flags{Slug: "WT-A"})
	if err != nil {
		t.Fatalf("--slug WT-A should normalise, not fail: %v", err)
	}
	if got := res.Map.Sites[0].Variant.Host; got != "wt-a--acmecorp.ddev.site" {
		t.Errorf("variant host %q, want it lowercased", got)
	}
	if _, ok := res.Map.SiteForHost("wt-a--acmecorp.ddev.site"); !ok {
		t.Error("the derived variant host does not route — this is the 421")
	}
}

// TestUncoveredDDEVHostsAreReported: a hostshift.yaml replaces the DDEV layer
// rather than merging with it, which is right — but a project registering nine
// hostnames whose yaml declares three will 421 the other six in silence.
func TestUncoveredDDEVHostsAreReported(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".ddev/config.yaml",
		"name: acmecorp\nadditional_hostnames:\n  - nat.acmecorp\n  - extra.acmecorp\n")
	write(t, dir, "hostshift.yaml",
		"version: 1\nsites:\n  - {name: main, canonical: https://www.acmecorp.fi, base: https://acmecorp.ddev.site}\n")

	res, err := Load(dir, Flags{Slug: "wt-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Uncovered) != 2 {
		t.Fatalf("Uncovered = %v, want the two undeclared DDEV hostnames", res.Uncovered)
	}
	// And the ones the map does cover must not be reported.
	for _, h := range res.Uncovered {
		if h == "acmecorp.ddev.site" {
			t.Errorf("%s is the declared base and is covered", h)
		}
	}
}

// TestOwnHostnameIsNotReportedUncovered. The uncovered-hostname warning fired
// on every correctly configured worktree, because a worktree's own DDEV project
// name is *supposed* to be absent from the map — it is what web answers to, and
// web is where mailpit and `ddev launch` live. A warning that is always wrong is
// how people learn to skip warnings.
func TestOwnHostnameIsNotReportedUncovered(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".ddev/config.yaml",
		"name: acmecorp-wt-a\nadditional_hostnames:\n  - stray.acmecorp\n")
	write(t, dir, "hostshift.yaml",
		"version: 1\nsites:\n  - {name: main, canonical: https://acmecorp.ddev.site, base: https://acmecorp.ddev.site}\n")

	res, err := Load(dir, Flags{Slug: "wt-a"})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range res.Uncovered {
		if h == "acmecorp-wt-a.ddev.site" {
			t.Errorf("the project's own hostname was reported as uncovered: %v", res.Uncovered)
		}
	}
	// But a genuinely undeclared one still is — that is the fsi shape the
	// warning was written for, where a hostshift.yaml declares three blogs of
	// nine and the rest have nowhere to be previewed.
	if len(res.Uncovered) != 1 || res.Uncovered[0] != "stray.acmecorp.ddev.site" {
		t.Errorf("Uncovered = %v, want just the undeclared hostname", res.Uncovered)
	}
}
