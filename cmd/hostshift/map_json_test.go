package main

import (
	"encoding/json"
	"strings"
	"testing"
)

const prodYAML = `version: 1
sites:
  - name: main
    canonical: https://www.example.com
    base: https://acme.ddev.site
    aliases: [https://acme.staging.example.net]
  - name: shop
    canonical: https://shop.example.com
    base: https://shop.acme.ddev.site
`

type mapJSON struct {
	Layer, Source string
	Sites         []struct {
		Name, Canonical, Base, Variant string
		Aliases                        []string
	}
}

func decodeMap(t *testing.T, out string) mapJSON {
	t.Helper()
	var m mapJSON
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("%v:\n%s", err, out)
	}
	return m
}

// `map --json` lists the sites without a slug, so a caller can read a
// hostshift.yaml's canonicals and bases without deciding a variant; with one,
// each site carries its variant too.
func TestMapJSONListsSitesWithoutASlug(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "hostshift.yaml", prodYAML)

	code, out, errOut := run(t, "", cmdMap, "-C", dir, "--json")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	m := decodeMap(t, out)
	if m.Layer != "file" || !strings.HasSuffix(m.Source, "hostshift.yaml") || len(m.Sites) != 2 {
		t.Fatalf("%+v", m)
	}
	s := m.Sites[0]
	if s.Name != "main" || s.Canonical != "https://www.example.com" || s.Base != "https://acme.ddev.site" ||
		s.Variant != "" || len(s.Aliases) != 2 {
		t.Errorf("site 1: %+v", s)
	}
	if strings.Contains(out, `"variant"`) {
		t.Errorf("a variant without a slug:\n%s", out)
	}

	_, out, _ = run(t, "", cmdMap, "-C", dir, "--json", "--slug", "wt-a")
	if v := decodeMap(t, out).Sites[1].Variant; v != "https://wt-a--shop.acme.ddev.site" {
		t.Errorf("variant = %q", v)
	}

	// Without --json a missing slug is still an error.
	if code, _, errOut := run(t, "", cmdMap, "-C", dir); code != exitConfig || !strings.Contains(errOut, "no variant") {
		t.Errorf("exit %d: %s", code, errOut)
	}
}

// --file reads a hostshift.yaml other than the project's, and - reads it from
// stdin: the project's own copy is not consulted.
func TestMapFileOverridesTheProjects(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "hostshift.yaml", strings.ReplaceAll(prodYAML, "www.example.com", "www.example.org"))
	writeFile(t, dir, "other.yaml", prodYAML)

	_, out, _ := run(t, prodYAML, cmdMap, "-C", dir, "--json", "--file", "-")
	if m := decodeMap(t, out); m.Source != "stdin" || m.Sites[0].Canonical != "https://www.example.com" {
		t.Errorf("stdin: %+v", m)
	}
	_, out, _ = run(t, "", cmdMap, "-C", dir, "--json", "--file", dir+"/other.yaml")
	if m := decodeMap(t, out); m.Source != dir+"/other.yaml" || m.Sites[0].Canonical != "https://www.example.com" {
		t.Errorf("file: %+v", m)
	}
	// A named file that is not there is an error, not a fall-back.
	if code, _, _ := run(t, "", cmdMap, "-C", dir, "--json", "--file", dir+"/missing.yaml"); code != exitConfig {
		t.Errorf("missing file: exit %d", code)
	}
	// Only plain origins: a path is refused, as everywhere else.
	bad := strings.Replace(prodYAML, "https://shop.example.com", "https://shop.example.com/x", 1)
	if code, _, errOut := run(t, bad, cmdMap, "--json", "--file", "-"); code != exitConfig || !strings.Contains(errOut, "no path") {
		t.Errorf("path: exit %d: %s", code, errOut)
	}
	// rewrite reads its body from stdin, so it cannot take the file there too.
	if code, _, _ := run(t, "x", cmdRewrite, "--file", "-"); code != exitConfig {
		t.Errorf("rewrite --file -: exit %d", code)
	}
}

// The layer says which of the three the map came from.
func TestMapJSONLayer(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".ddev/config.yaml", "name: acme\n")
	_, out, _ := run(t, "", cmdMap, "-C", dir, "--json")
	if m := decodeMap(t, out); m.Layer != "ddev" || m.Sites[0].Base != "https://acme.ddev.site" {
		t.Errorf("ddev: %+v", m)
	}
	_, out, _ = run(t, "", cmdMap, "-C", dir, "--json", "--map", "https://a.example=https://b.example")
	if m := decodeMap(t, out); m.Layer != "flags" || m.Sites[0].Base != "" {
		t.Errorf("flags: %+v", m)
	}
}

func TestHostsJSON(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".ddev/config.yaml", "name: site\nadditional_hostnames:\n  - blog\n")
	if _, out, _ := run(t, "", cmdHosts, "-C", dir, "--json"); out != `["site.ddev.site","blog.ddev.site"]`+"\n" {
		t.Errorf("out = %q", out)
	}
}
