package main

import (
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
			"location @external {\n    rewrite ^ https://www.herrfors.fi$request_uri redirect;\n}\n")

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
			"location @external {\n    rewrite ^ https://www.herrfors.fi$request_uri? redirect;\n}\n")
		if w := uploadsRedirectWarnings(dir); len(w) != 0 {
			t.Errorf("the fixed form still warns: %v", w)
		}
	})

	t.Run("nginx snippets in a subdirectory are found", func(t *testing.T) {
		// The fleet spells this directory several ways.
		dir := t.TempDir()
		writeFile(t, dir, ".ddev/nginx_full/wordpress/uploads.conf",
			"rewrite ^ https://www.herrfors.fi$request_uri redirect;\n")
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
