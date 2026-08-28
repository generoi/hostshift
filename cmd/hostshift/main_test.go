package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func writeFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestUploadsRedirectWarning. The self-redirect guard deliberately does not
// absorb the fleet's uploads-redirect loop — that would be carrying a
// workaround for a one-character bug in someone else's nginx config, and it
// would silently pass through every redirect that changes only the query on the
// same path. The compensating control is that `check` names it, and until now
// that control did not exist: `grep -rn request_uri --include='*.go'` found
// nothing but comments, while 52 of 53 fleet repos still had the loop.
func TestUploadsRedirectWarning(t *testing.T) {
	t.Run("the broken form is named", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, ".ddev/nginx/redirect-uploads.conf",
			"location @external {\n    rewrite ^ https://www.acmecorp.fi$request_uri redirect;\n}\n")

		w := uploadsRedirectWarnings(dir)
		if len(w) != 1 {
			t.Fatalf("warnings = %v, want exactly one", w)
		}
		for _, want := range []string{"redirect-uploads.conf", "414", "$request_uri?"} {
			if !strings.Contains(w[0], want) {
				t.Errorf("the warning does not mention %q:\n%s", want, w[0])
			}
		}
	})

	t.Run("the fixed form is silent", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, ".ddev/nginx/redirect-uploads.conf",
			"location @external {\n    rewrite ^ https://www.acmecorp.fi$request_uri? redirect;\n}\n")
		if w := uploadsRedirectWarnings(dir); len(w) != 0 {
			t.Errorf("the fixed form still warns: %v", w)
		}
	})

	t.Run("nginx snippets in a subdirectory are found", func(t *testing.T) {
		// The fleet spells this directory several ways.
		dir := t.TempDir()
		writeFile(t, dir, ".ddev/nginx_full/wordpress/uploads.conf",
			"rewrite ^ https://www.acmecorp.fi$request_uri redirect;\n")
		if w := uploadsRedirectWarnings(dir); len(w) != 1 {
			t.Errorf("warnings = %v, want one", w)
		}
	})

	t.Run("a project with no nginx config is silent", func(t *testing.T) {
		if w := uploadsRedirectWarnings(t.TempDir()); len(w) != 0 {
			t.Errorf("warnings = %v, want none", w)
		}
	})
}

// run invokes a subcommand with stdin/stdout/stderr captured, so the tests
// assert on what a person actually sees. The commands write through the os
// globals, which is right for a CLI and means these cannot run in parallel.
func run(t *testing.T, stdin string, f func([]string) (int, error), args ...string) (code int, out, errOut string) {
	t.Helper()
	oldIn, oldOut, oldErr := os.Stdin, os.Stdout, os.Stderr
	defer func() { os.Stdin, os.Stdout, os.Stderr = oldIn, oldOut, oldErr }()

	if stdin != "" {
		rIn, wIn, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		go func() { io.WriteString(wIn, stdin); wIn.Close() }()
		os.Stdin = rIn
	}
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = wOut, wErr

	var bo, be bytes.Buffer
	done := make(chan struct{}, 2)
	go func() { io.Copy(&bo, rOut); done <- struct{}{} }()
	go func() { io.Copy(&be, rErr); done <- struct{}{} }()

	code, cmdErr := f(args)
	wOut.Close()
	wErr.Close()
	<-done
	<-done
	if cmdErr != nil {
		be.WriteString("hostshift: " + cmdErr.Error() + "\n")
	}
	return code, bo.String(), be.String()
}

// ddevProject writes the minimum a DDEV project needs for the map to resolve.
func ddevProject(t *testing.T, extra string) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, ".ddev/config.yaml",
		"name: acmecorp\nadditional_hostnames:\n  - nat.acmecorp\n"+extra)
	return dir
}

// TestWPCLIRoundTrip is the piece §4.3 predicted wrongly and that only a person
// running it by hand had ever checked.
//
// §4.3 assumed wp-cli.local.yml *merges* with wp-cli.yml, so a two-line file
// carrying only url: would be enough. Measured with WP-CLI 2.12.0 in the M6
// pilot, it replaces it: a bare url: file lost path:, require: and every alias,
// and left WP-CLI unable to find the installation at all. So the whole config
// has to be carried through with url: added — which is a YAML round trip, and
// round trips lose things quietly.
func TestWPCLIRoundTrip(t *testing.T) {
	dir := ddevProject(t, "")
	writeFile(t, dir, "wp-cli.yml", `path: web
require:
  - config/wp-cli/pre-ssh.php
core config:
  dbname: wordpress
  extra-php: |
    define('WP_DEBUG', true);
"@production":
  ssh: user@example.com
  url: https://www.acmecorp.fi
"@ddev":
  ssh: ddev
  url: https://acmecorp.ddev.site
`)
	writeFile(t, dir, ".gitignore", "wp-cli.local.yml\n")

	code, out, errOut := run(t, "", cmdWPCLI, "-C", dir, "--slug", "wt-a")
	if code != exitOK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}

	var got map[string]any
	if err := yaml.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("the generated file is not valid YAML: %v\n%s", err, out)
	}

	// Nothing from the original may be lost — that is the whole reason this
	// writes the full config rather than two lines.
	if got["path"] != "web" {
		t.Errorf("path: was dropped (%v); WP-CLI would not find the installation", got["path"])
	}
	if _, ok := got["require"]; !ok {
		t.Error("require: was dropped")
	}
	if _, ok := got["core config"]; !ok {
		t.Error("the `core config` subcommand defaults were dropped")
	}
	for _, alias := range []string{"@production", "@ddev"} {
		if _, ok := got[alias]; !ok {
			t.Errorf("alias %s was dropped", alias)
		}
	}
	// Multi-line scalars survive the round trip.
	if cc, ok := got["core config"].(map[string]any); ok {
		if s, _ := cc["extra-php"].(string); !strings.Contains(s, "WP_DEBUG") {
			t.Errorf("extra-php did not survive: %q", s)
		}
	}

	// And the one thing added: the root url is blog 1's canonical host, which
	// is what get_site_by_path matches on.
	if got["url"] != "https://acmecorp.ddev.site" {
		t.Errorf("url: = %v, want blog 1's canonical origin", got["url"])
	}

	if !strings.HasPrefix(out, "# generated by hostshift") {
		t.Errorf("the file does not say it is generated:\n%s", out)
	}
	// Multisite: say how to reach a sibling blog, since url: can only name one.
	if !strings.Contains(errOut, "--url") {
		t.Errorf("no note about reaching a sibling blog:\n%s", errOut)
	}
}

// TestWPCLIWarnsAboutStaleAliases. An alias whose url: is one of a site's
// *alias* origins no longer resolves once the database holds the canonical
// hostname, because get_site_by_path matches wp_blogs.domain exactly. Warning
// beats rewriting: some of these aliases SSH into production.
func TestWPCLIWarnsAboutStaleAliases(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".ddev/config.yaml", "name: acmecorp\n")
	writeFile(t, dir, "hostshift.yaml",
		"version: 1\nsites:\n  - {name: main, canonical: https://www.acmecorp.fi, base: https://acmecorp.ddev.site}\n")
	writeFile(t, dir, "wp-cli.yml", `path: web
"@ddev":
  url: https://acmecorp.ddev.site
"@elsewhere":
  url: https://unrelated.example
`)
	writeFile(t, dir, ".gitignore", "wp-cli.local.yml\n")

	_, _, errOut := run(t, "", cmdWPCLI, "-C", dir, "--slug", "wt-a")
	if !strings.Contains(errOut, "@ddev") {
		t.Errorf("no warning for the alias the database no longer holds:\n%s", errOut)
	}
	if strings.Contains(errOut, "@elsewhere") {
		t.Errorf("warned about an alias that has nothing to do with the map:\n%s", errOut)
	}
}

// TestWPCLIWithNoExistingConfig: a stock WordPress has no wp-cli.yml. The
// generated file is then just the url, and must still be valid YAML.
func TestWPCLIWithNoExistingConfig(t *testing.T) {
	dir := ddevProject(t, "")
	code, out, _ := run(t, "", cmdWPCLI, "-C", dir, "--slug", "wt-a")
	if code != exitOK {
		t.Fatalf("exit %d", code)
	}
	var got map[string]any
	if err := yaml.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not valid YAML: %v\n%s", err, out)
	}
	if len(got) != 1 || got["url"] == nil {
		t.Errorf("want exactly a url:, got %v", got)
	}
}

// TestWPCLIRejectsBrokenYAML: exit 2 is "invalid configuration" (§5.8), and the
// alternative — silently writing a file that drops everything unparseable — is
// how you lose a require: line and spend an afternoon on it.
func TestWPCLIRejectsBrokenYAML(t *testing.T) {
	dir := ddevProject(t, "")
	writeFile(t, dir, "wp-cli.yml", "path: web\n  bad indentation: [\n")
	code, out, _ := run(t, "", cmdWPCLI, "-C", dir, "--slug", "wt-a")
	if code != exitConfig {
		t.Errorf("exit %d, want %d for unparseable config", code, exitConfig)
	}
	if strings.Contains(out, "url:") {
		t.Errorf("wrote a config anyway:\n%s", out)
	}
}

// TestCheckExitCodes is §5.8's contract: 0 success, 2 invalid configuration.
// `check` exists to be run in a rollout script, so the exit code is the API.
func TestCheckExitCodes(t *testing.T) {
	for _, c := range []struct {
		name, yaml string
		want       int
	}{
		{"valid", "version: 1\nsites:\n  - {name: main, canonical: https://www.acmecorp.fi, base: https://acmecorp.ddev.site}\n", exitOK},
		{"unsupported version", "version: 99\nsites: []\n", exitConfig},
		{"no sites", "version: 1\nsites: []\n", exitConfig},
		{"duplicate canonical", "version: 1\nsites:\n" +
			"  - {name: a, canonical: https://www.acmecorp.fi, base: https://a.acmecorp.ddev.site}\n" +
			"  - {name: b, canonical: https://www.acmecorp.fi, base: https://b.acmecorp.ddev.site}\n", exitConfig},
		{"not yaml", "sites: [\n", exitConfig},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := ddevProject(t, "")
			writeFile(t, dir, "hostshift.yaml", c.yaml)
			code, _, errOut := run(t, "", cmdCheck, "-C", dir, "--slug", "wt-a")
			if code != c.want {
				t.Errorf("exit %d, want %d\n%s", code, c.want, errOut)
			}
		})
	}

	t.Run("a slug that cannot be a hostname label", func(t *testing.T) {
		dir := ddevProject(t, "")
		if code, _, _ := run(t, "", cmdCheck, "-C", dir, "--slug", "feature/ABC-123"); code != exitConfig {
			t.Errorf("exit %d, want %d — this map 421s every request", code, exitConfig)
		}
	})

	t.Run("warnings do not change the exit code", func(t *testing.T) {
		dir := ddevProject(t, "")
		writeFile(t, dir, ".ddev/nginx/redirect-uploads.conf",
			"rewrite ^ https://www.acmecorp.fi$request_uri redirect;\n")
		code, _, errOut := run(t, "", cmdCheck, "-C", dir, "--slug", "wt-a")
		if code != exitOK {
			t.Errorf("exit %d: a warning is not a failure", code)
		}
		if !strings.Contains(errOut, "request_uri?") {
			t.Errorf("the uploads-redirect loop was not reported:\n%s", errOut)
		}
	})
}

// TestMapEnv is what the DDEV add-on's .ddev/.env is generated from, so its
// shape is load-bearing: three variables, and web keeping the hostnames the
// slug does not claim.
func TestMapEnv(t *testing.T) {
	dir := ddevProject(t, "")
	code, out, errOut := run(t, "", cmdMap, "-C", dir, "--slug", "wt-a", "--env")
	if code != exitOK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	env := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		k, v, _ := strings.Cut(line, "=")
		env[k] = v
	}
	for _, k := range []string{"HOSTSHIFT_SLUG", "HOSTSHIFT_VARIANTS", "HOSTSHIFT_WEB_HOSTS"} {
		if _, ok := env[k]; !ok {
			t.Errorf("%s missing; the compose file has no default for it and web's VIRTUAL_HOST goes blank", k)
		}
	}
	if env["HOSTSHIFT_VARIANTS"] != "wt-a--acmecorp.ddev.site,wt-a--nat.acmecorp.ddev.site" {
		t.Errorf("HOSTSHIFT_VARIANTS = %q", env["HOSTSHIFT_VARIANTS"])
	}
	// web keeps the project's own hostnames minus the variants. Handing it the
	// variants too makes both claim them and the router picks web.
	if env["HOSTSHIFT_WEB_HOSTS"] != "acmecorp.ddev.site,nat.acmecorp.ddev.site" {
		t.Errorf("HOSTSHIFT_WEB_HOSTS = %q", env["HOSTSHIFT_WEB_HOSTS"])
	}
	// The additional_hostnames note prints entries without the TLD, because
	// DDEV appends it — printing the whole hostname registers it twice-suffixed
	// and mkcert then issues no SAN.
	if !strings.Contains(errOut, "- wt-a--acmecorp\n") {
		t.Errorf("additional_hostnames note is wrong:\n%s", errOut)
	}
}

// TestMapEnvHonoursProjectTLD: three of the 64 local fleet projects override it.
func TestMapEnvHonoursProjectTLD(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".ddev/config.yaml", "name: fsi\nproject_tld: ddev.local\n")
	_, out, errOut := run(t, "", cmdMap, "-C", dir, "--slug", "wt-a", "--env")
	if !strings.Contains(out, "wt-a--fsi.ddev.local") {
		t.Errorf("variant does not use the project TLD:\n%s", out)
	}
	if !strings.Contains(errOut, "- wt-a--fsi\n") {
		t.Errorf("the additional_hostnames note re-suffixes the TLD:\n%s", errOut)
	}
}

// TestRewriteFilter is test 27's premise from the CLI side: the filter and the
// proxy are the same code path, so the filter is how a corpus diff is a
// one-liner.
func TestRewriteFilter(t *testing.T) {
	dir := ddevProject(t, "")
	in := `<a href="https://acmecorp.ddev.site/x">k</a>`

	code, out, _ := run(t, in, cmdRewrite,
		"-C", dir, "--slug", "wt-a", "--quiet")
	if code != exitOK {
		t.Fatalf("exit %d", code)
	}
	if out != `<a href="https://wt-a--acmecorp.ddev.site/x">k</a>` {
		t.Errorf("got %s", out)
	}

	t.Run("reverse maps the request direction", func(t *testing.T) {
		code, out, _ := run(t, `<a href="https://wt-a--acmecorp.ddev.site/x">k</a>`,
			cmdRewrite, "-C", dir, "--slug", "wt-a", "--reverse", "--quiet")
		if code != exitOK {
			t.Fatalf("exit %d", code)
		}
		if out != `<a href="https://acmecorp.ddev.site/x">k</a>` {
			t.Errorf("got %s", out)
		}
	})

	t.Run("dry run emits the input unchanged and still counts", func(t *testing.T) {
		_, out, errOut := run(t, in, cmdRewrite, "-C", dir, "--slug", "wt-a", "--dry-run")
		if out != in {
			t.Errorf("--dry-run changed the body:\n got %s\nwant %s", out, in)
		}
		if !strings.Contains(errOut, "html-attr") {
			t.Errorf("--dry-run counted nothing; it must report what it would have done:\n%s", errOut)
		}
		if !strings.Contains(errOut, "straggler sweep: not run") {
			t.Errorf("the report prints no straggler count and does not say why:\n%s", errOut)
		}
	})

	t.Run("a type outside the rewritable set is passed through", func(t *testing.T) {
		css := "body{background:url(https://acmecorp.ddev.site/a.png)}"
		_, out, errOut := run(t, css, cmdRewrite,
			"-C", dir, "--slug", "wt-a", "--type", "text/css", "--json")
		if out != css {
			t.Errorf("a CSS file was rewritten; §5.2 puts it outside the set:\n%s", out)
		}
		// Test 25: proven by a per-surface counter of zero.
		if strings.Contains(errOut, `"rewrites": {}`) == false && strings.Contains(errOut, "html-attr") {
			t.Errorf("content outside the set entered a rewriter:\n%s", errOut)
		}
	})

	t.Run("json is rewritten and stays valid", func(t *testing.T) {
		_, out, _ := run(t, `{"link":"https:\/\/acmecorp.ddev.site\/x"}`, cmdRewrite,
			"-C", dir, "--slug", "wt-a", "--type", "application/json", "--quiet")
		if out != `{"link":"https:\/\/wt-a--acmecorp.ddev.site\/x"}` {
			t.Errorf("got %s", out)
		}
	})
}

// TestVersionIsStamped guards a failure the linker will not report: -X naming a
// variable that does not exist is silently ignored, so the binary would report
// "dev" forever and no build would fail.
func TestVersionIsStamped(t *testing.T) {
	if version == "" {
		t.Fatal("main.version is empty; -X has nothing to write to")
	}
}
