package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeYAML(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestFromWithSlugDerivesVariants. A worktree's canonical hostnames belong to a
// different directory — the checkout it was branched from, whose database it
// shares — so they cannot come from layer 1, which only reads the project being
// configured. The map was therefore built against the hostname the worktree is
// *served at*, which appears nowhere in the database: the proxy had nothing to
// rewrite, every link stayed wrong, and `check` called it injective and anchored.
//
// --from says "here are the canonical origins", --slug says "derive the rest".
func TestFromWithSlugDerivesVariants(t *testing.T) {
	res, err := Load(t.TempDir(), Flags{
		From: []string{"https://acmecorp.ddev.site", "https://nat.acmecorp.ddev.site"},
		Slug: "wt-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := res.Map.String()
	for _, want := range []string{
		"https://acmecorp.ddev.site",
		"https://wt-a--acmecorp.ddev.site",
		"https://nat.acmecorp.ddev.site",
		"https://wt-a--nat.acmecorp.ddev.site",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("map is missing %s:\n%s", want, got)
		}
	}
}

// --from alone cannot mean anything without a way to derive the other side, and
// saying so beats "map is not index-aligned: 2 --from against 0 --to".
func TestFromWithoutToOrSlugSaysWhatIsMissing(t *testing.T) {
	_, err := Load(t.TempDir(), Flags{From: []string{"https://a.example"}})
	if err == nil {
		t.Fatal("accepted --from with neither --to nor --slug")
	}
	if !strings.Contains(err.Error(), "--slug") {
		t.Errorf("error does not mention --slug: %v", err)
	}
}

// TestUnknownKeyInFileIsAnError. A silently-ignored key is the worst failure
// this file has. `upsteam:` left the map valid, `check` reported it injective
// and anchored, and `proxy` then refused to start with "no upstream" pointing at
// a key the reader can see is right there.
func TestUnknownKeyInFileIsAnError(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, dir, "hostshift.yaml",
		"upsteam: http://web:80\nsites:\n  - canonical: https://a.example\n    variant: https://b.example\n")
	_, err := Load(dir, Flags{})
	if err == nil {
		t.Fatal("a misspelled top-level key was accepted")
	}
	if !strings.Contains(err.Error(), "upsteam") {
		t.Errorf("error does not name the key: %v", err)
	}
}

// TestUpstreamIsValidated. `--upstream web:80` is a likely typo given the
// documented http://web:80, and it started the proxy happily, then failed at
// dial time on every request. A URL with a space was echoed back percent-encoded
// ("upstream not%20a%20url"), which only the author could decode.
func TestUpstreamIsValidated(t *testing.T) {
	for _, up := range []string{"web:80", "not a url", "/just/a/path"} {
		_, err := Load(t.TempDir(), Flags{
			From:     []string{"https://a.example"},
			To:       []string{"https://b.example"},
			Upstream: up,
		})
		if err == nil {
			t.Errorf("--upstream %q was accepted", up)
			continue
		}
		if !strings.Contains(err.Error(), "upstream") {
			t.Errorf("--upstream %q: error does not say upstream: %v", up, err)
		}
	}
	if _, err := Load(t.TempDir(), Flags{
		From:     []string{"https://a.example"},
		To:       []string{"https://b.example"},
		Upstream: "http://web:80",
	}); err != nil {
		t.Errorf("a valid upstream was rejected: %v", err)
	}
}
