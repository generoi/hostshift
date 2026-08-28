package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
