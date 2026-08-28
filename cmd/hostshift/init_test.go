package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(cmd.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// worktree builds the shape the tier-2 design describes: a repo whose DDEV
// project is `name`, and a linked git worktree beside it whose own DDEV project
// is `wtName`. Returns the worktree's path.
func worktree(t *testing.T, name, wtName, branch string) string {
	t.Helper()
	root := t.TempDir()
	main := filepath.Join(root, "main")
	writeFile(t, main, ".ddev/config.yaml", "name: "+name+"\ntype: wordpress\n")
	git(t, main, "init", "-q", "-b", "master")
	git(t, main, "add", "-A")
	git(t, main, "commit", "-qm", "init")

	wt := filepath.Join(root, "wt")
	git(t, main, "worktree", "add", "-q", wt, "-b", branch)
	if wtName != "" {
		writeFile(t, wt, ".ddev/config.yaml", "name: "+wtName+"\ntype: wordpress\n")
	}
	return wt
}

// TestInitDoesNotNameTheProjectAfterTheVariant. Naming a worktree's DDEV
// project wt-a--acme makes its own hostname and the variant hostname the same
// string, so web is left with no VIRTUAL_HOST and mailpit and `ddev launch`
// stop routing. init must say so rather than write it.
func TestInitDoesNotNameTheProjectAfterTheVariant(t *testing.T) {
	wt := worktree(t, "acme", "wt-a--acme", "wt-a")

	_, out, errOut := run(t, "", cmdInit, "-C", wt, "--dry-run")
	if !strings.Contains(out, "HOSTSHIFT_WEB_HOSTS=\n") {
		t.Fatalf("this test assumes web ends up with nothing:\n%s", out)
	}
	if !strings.Contains(errOut, "VIRTUAL_HOST") {
		t.Errorf("the collision was not reported:\n%s", errOut)
	}
}

// TestInitWritesTheMapFromTheParentWorktree is the shape the tier-2 design
// describes, and the one a worktree cannot resolve on its own: its
// .ddev/config.yaml names the hostname it is served *at*, so the canonical
// hostnames — the ones the shared database was written for — are only in the
// checkout it was created from. The proxy resolves the map itself from
// /project, so they have to be written down where it will look.
func TestInitWritesTheMapFromTheParentWorktree(t *testing.T) {
	wt := worktree(t, "acme", "acme-wt-a", "feature/x")

	code, _, errOut := run(t, "", cmdInit, "-C", wt, "--slug", "wt-a")
	if code != exitOK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	got := readAll(t, filepath.Join(wt, "hostshift.yaml"))
	if !strings.Contains(got, "canonical: https://acme.ddev.site") {
		t.Errorf("canonical not taken from the parent checkout:\n%s", got)
	}
	if strings.Contains(got, "acme-wt-a.ddev.site") {
		t.Errorf("this worktree's own hostname was declared canonical:\n%s", got)
	}

	// And the resolved map is now what the proxy will see.
	_, out, _ := run(t, "", cmdMap, "-C", wt, "--slug", "wt-a")
	if !strings.Contains(out, "https://acme.ddev.site  ->  https://wt-a--acme.ddev.site") {
		t.Errorf("the map does not resolve as written:\n%s", out)
	}
}

// TestInitNeverOverwritesAnExistingMap: hostshift.yaml is the
// production-canonical opt-in, and a generated ddev-canonical one would
// silently revert a site to browsing its own dev hostnames.
func TestInitNeverOverwritesAnExistingMap(t *testing.T) {
	wt := worktree(t, "acme", "acme-wt-a", "feature/x")
	const declared = "version: 1\nsites:\n  - {name: main, canonical: https://www.acme.test, base: https://acme.ddev.site}\n"
	writeFile(t, wt, "hostshift.yaml", declared)

	if code, _, errOut := run(t, "", cmdInit, "-C", wt, "--slug", "wt-a"); code != exitOK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if got := readAll(t, filepath.Join(wt, "hostshift.yaml")); got != declared {
		t.Errorf("an existing map was overwritten:\n%s", got)
	}
}

// TestInitFallsBackToTheBranch: a worktree that shares its parent's DDEV
// project has the same name in both places, so there is nothing to subtract.
func TestInitFallsBackToTheBranch(t *testing.T) {
	// No wtName: the worktree inherits the tracked .ddev/config.yaml, so both
	// ends are "acme".
	wt := worktree(t, "acme", "", "feature/ABC-123")

	code, out, errOut := run(t, "", cmdInit, "-C", wt, "--dry-run")
	if code != exitOK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	// "feature/ABC-123" is the ordinary branch name, not the exotic one, and
	// neither the slash nor the capitals can appear in a hostname label.
	if !strings.Contains(errOut, `slug "feature-abc-123"`) {
		t.Errorf("branch not reduced to a hostname label:\n%s", errOut)
	}
	if !strings.Contains(out, "HOSTSHIFT_VARIANTS=feature-abc-123--acme.ddev.site") {
		t.Errorf("wrong variant:\n%s", out)
	}
}

// TestInitWritesBothFiles is the point of the command: two generated files, one
// of them a merge.
func TestInitWritesBothFiles(t *testing.T) {
	wt := worktree(t, "acme", "acme-wt-a", "wt-a")
	// .ddev/.env is a project file — it is where a project puts whatever its
	// own compose overrides need — so a run that truncated it would delete
	// someone's configuration. The documented setup, `map --env > .ddev/.env`,
	// did exactly that.
	writeFile(t, wt, ".ddev/.env", "SOMETHING_ELSE=keep me\nHOSTSHIFT_SLUG=stale\n")

	if code, _, errOut := run(t, "", cmdInit, "-C", wt); code != exitOK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}

	env := readAll(t, filepath.Join(wt, ".ddev", ".env"))
	if !strings.Contains(env, "SOMETHING_ELSE=keep me") {
		t.Errorf("an unrelated variable was dropped:\n%s", env)
	}
	if !strings.Contains(env, "HOSTSHIFT_SLUG=wt-a") {
		t.Errorf("the slug was not updated:\n%s", env)
	}
	if strings.Contains(env, "stale") {
		t.Errorf("the old value survived alongside the new one:\n%s", env)
	}

	cfg := readAll(t, filepath.Join(wt, ".ddev", "config.hostshift.local.yaml"))
	// The full list, not only the new names: DDEV's own config.yaml is left
	// untouched, and a merge that replaced would otherwise drop the project's
	// existing hostnames.
	for _, want := range []string{"- acme-wt-a", "- wt-a--acme"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("%q missing from the generated config:\n%s", want, cfg)
		}
	}

	// Idempotent: running it again changes nothing.
	before := env + cfg
	if code, _, _ := run(t, "", cmdInit, "-C", wt); code != exitOK {
		t.Fatal("second run failed")
	}
	after := readAll(t, filepath.Join(wt, ".ddev", ".env")) +
		readAll(t, filepath.Join(wt, ".ddev", "config.hostshift.local.yaml"))
	if before != after {
		t.Errorf("not idempotent:\n--- before\n%s\n--- after\n%s", before, after)
	}
}

func TestHostLabel(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"wt-a", "wt-a"},
		{"feature/ABC-123", "feature-abc-123"},
		{"WT_A", "wt-a"},
		{"...", ""},
		{"release/2026.08.28", "release-2026-08-28"},
		{strings.Repeat("x", 50), strings.Repeat("x", 30)},
	} {
		if got := hostLabel(c.in); got != c.want {
			t.Errorf("hostLabel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestInitZeroConfigWorktree is the whole point, for the simplest kind of site.
//
// A project that omits `name:` from .ddev/config.yaml gets DDEV's default, which
// is the directory name — so a git worktree is automatically its own DDEV
// project, and two projects cannot share a name is no longer a problem anyone
// has to solve by hand. Nothing is committed, nothing is passed, and `init`
// works out both ends: canonical from the checkout the worktree was created
// from, variant from its own branch.
func TestInitZeroConfigWorktree(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "btbtransformers")
	// No `name:` — this is the one-line change a project makes.
	writeFile(t, main, ".ddev/config.yaml", "type: wordpress\ndocroot: web\n")
	git(t, main, "init", "-q", "-b", "master")
	git(t, main, "add", "-A")
	git(t, main, "commit", "-qm", "init")

	wt := filepath.Join(root, "btbtransformers-wt-a")
	git(t, main, "worktree", "add", "-q", wt, "-b", "feature/ABC-123")

	code, _, errOut := run(t, "", cmdInit, "-C", wt)
	if code != exitOK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}

	// Canonical is the parent checkout's project, which is its directory name.
	got := readAll(t, filepath.Join(wt, "hostshift.yaml"))
	if !strings.Contains(got, "canonical: https://btbtransformers.ddev.site") {
		t.Errorf("canonical not derived from the parent checkout:\n%s", got)
	}

	env := readAll(t, filepath.Join(wt, ".ddev", ".env"))
	if !strings.Contains(env, "HOSTSHIFT_VARIANTS=feature-abc-123--btbtransformers.ddev.site") {
		t.Errorf("variant:\n%s", env)
	}
	// web keeps the worktree's own hostname, so mailpit and `ddev launch` still
	// route. That is what naming the project after the directory buys.
	if !strings.Contains(env, "HOSTSHIFT_WEB_HOSTS=btbtransformers-wt-a.ddev.site") {
		t.Errorf("web hosts:\n%s", env)
	}

	// And the compose service, so nothing had to be committed and no add-on
	// install was needed.
	compose := readAll(t, filepath.Join(wt, ".ddev", "docker-compose.hostshift.yaml"))
	if !strings.Contains(compose, "ghcr.io/generoi/hostshift") {
		t.Errorf("the compose service was not written:\n%s", compose)
	}

	// The proxy resolves the same map the init run reported.
	_, out, _ := run(t, "", cmdMap, "-C", wt, "--slug", "feature-abc-123")
	if !strings.Contains(out, "https://btbtransformers.ddev.site  ->  https://feature-abc-123--btbtransformers.ddev.site") {
		t.Errorf("map:\n%s", out)
	}
}

// TestInitDoesNotClobberAnInstalledAddon: a project that has run
// `ddev add-on get`, or pinned the service deliberately, keeps what it has.
func TestInitDoesNotClobberAnInstalledAddon(t *testing.T) {
	wt := worktree(t, "acme", "acme-wt-a", "wt-a")
	const pinned = "services:\n  hostshift:\n    image: ghcr.io/generoi/hostshift:v0.1.0\n"
	writeFile(t, wt, ".ddev/docker-compose.hostshift.yaml", pinned)

	if code, _, errOut := run(t, "", cmdInit, "-C", wt, "--slug", "wt-a"); code != exitOK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if got := readAll(t, filepath.Join(wt, ".ddev", "docker-compose.hostshift.yaml")); got != pinned {
		t.Errorf("an existing compose service was overwritten:\n%s", got)
	}
}

// TestInitRefreshesItsOwnFiles. Refusing to overwrite anything protects a
// hand-written hostshift.yaml — which is the production-canonical declaration
// and must never be clobbered — but the same guard pinned a *generated* one
// forever. Add a blog to the checkout a worktree came from and the worktree
// kept the old map, silently, for as long as it existed.
func TestInitRefreshesItsOwnFiles(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "acme")
	writeFile(t, main, ".ddev/config.yaml", "type: wordpress\n")
	git(t, main, "init", "-q", "-b", "master")
	git(t, main, "add", "-A")
	git(t, main, "commit", "-qm", "init")

	wt := filepath.Join(root, "acme-wt-a")
	git(t, main, "worktree", "add", "-q", wt, "-b", "wt-a")

	if code, _, errOut := run(t, "", cmdInit, "-C", wt); code != exitOK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if got := readAll(t, filepath.Join(wt, "hostshift.yaml")); strings.Contains(got, "nat.acme") {
		t.Fatalf("test setup wrong:\n%s", got)
	}

	// A blog is added to the parent, the way a real one would be.
	writeFile(t, main, ".ddev/config.yaml",
		"type: wordpress\nadditional_hostnames:\n  - nat.acme\n")

	// check says so before anything is rerun.
	_, _, errOut := run(t, "", cmdCheck, "-C", wt)
	if !strings.Contains(errOut, "nat.acme.ddev.site") || !strings.Contains(errOut, "rerun") {
		t.Errorf("drift was not reported:\n%s", errOut)
	}

	// And rerunning init picks it up.
	if code, _, e := run(t, "", cmdInit, "-C", wt); code != exitOK {
		t.Fatalf("exit %d\n%s", code, e)
	}
	got := readAll(t, filepath.Join(wt, "hostshift.yaml"))
	if !strings.Contains(got, "https://nat.acme.ddev.site") {
		t.Errorf("a generated map was not refreshed:\n%s", got)
	}
	if _, _, errOut := run(t, "", cmdCheck, "-C", wt); strings.Contains(errOut, "rerun") {
		t.Errorf("still reports drift after a refresh:\n%s", errOut)
	}
}

// TestInitNeverTouchesAHandWrittenMap is the other half, and the reason the
// rule is keyed on the marker rather than on the file existing.
func TestInitNeverTouchesAHandWrittenMap(t *testing.T) {
	wt := worktree(t, "acme", "acme-wt-a", "wt-a")
	const declared = "version: 1\nsites:\n  - {name: main, canonical: https://www.acme.test, base: https://acme.ddev.site}\n"
	writeFile(t, wt, "hostshift.yaml", declared)

	if code, _, errOut := run(t, "", cmdInit, "-C", wt, "--slug", "wt-a"); code != exitOK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if got := readAll(t, filepath.Join(wt, "hostshift.yaml")); got != declared {
		t.Errorf("a hand-written map was overwritten:\n%s", got)
	}
	// And it is not reported as drifted: disagreeing with DDEV is the entire
	// point of a production-canonical declaration.
	if _, _, errOut := run(t, "", cmdCheck, "-C", wt, "--slug", "wt-a"); strings.Contains(errOut, "rerun") {
		t.Errorf("a hand-written map was reported as stale:\n%s", errOut)
	}
}

// TestInitRefreshesTheComposeService: same rule, DDEV's own marker. A project
// that removed the marker to own the file keeps it.
func TestInitRefreshesTheComposeService(t *testing.T) {
	wt := worktree(t, "acme", "acme-wt-a", "wt-a")
	p := filepath.Join(wt, ".ddev", "docker-compose.hostshift.yaml")

	writeFile(t, wt, ".ddev/docker-compose.hostshift.yaml", "#ddev-generated\nservices: {}\n")
	if code, _, e := run(t, "", cmdInit, "-C", wt, "--slug", "wt-a"); code != exitOK {
		t.Fatalf("exit %d\n%s", code, e)
	}
	if !strings.Contains(readAll(t, p), "ghcr.io/generoi/hostshift") {
		t.Error("a stale generated compose service was not refreshed")
	}

	const owned = "services:\n  hostshift:\n    image: ghcr.io/generoi/hostshift:v0.1.0\n"
	writeFile(t, wt, ".ddev/docker-compose.hostshift.yaml", owned)
	if code, _, e := run(t, "", cmdInit, "-C", wt, "--slug", "wt-a"); code != exitOK {
		t.Fatalf("exit %d\n%s", code, e)
	}
	if readAll(t, p) != owned {
		t.Error("a file the project owns was overwritten")
	}
}

// TestInitSharesTheParentDatabase is the half that makes the rest worth having.
// A worktree only needs a hostname of its own *because* it is serving someone
// else's database; a proxy in front of an empty database solves nothing.
func TestInitSharesTheParentDatabase(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "acme")
	writeFile(t, main, ".ddev/config.yaml", "type: wordpress\ndocroot: web\n")
	writeFile(t, main, "web/app/uploads/2025/05/x.png", "not really a png")
	git(t, main, "init", "-q", "-b", "master")
	git(t, main, "add", "-A")
	git(t, main, "commit", "-qm", "init")

	wt := filepath.Join(root, "acme-wt-a")
	git(t, main, "worktree", "add", "-q", wt, "-b", "wt-a")

	code, _, errOut := run(t, "", cmdInit, "-C", wt)
	if code != exitOK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}

	cfg := readAll(t, filepath.Join(wt, ".ddev", "config.hostshift.local.yaml"))
	// No database of its own...
	if !strings.Contains(cfg, "omit_containers: [db]") {
		t.Errorf("the worktree still runs its own database:\n%s", cfg)
	}
	// ...and the application pointed at the parent's. DDEV already puts every
	// container on ddev_default, so the name resolves with no network override.
	if !strings.Contains(cfg, "DATABASE_URL=mysql://db:db@ddev-acme-db:3306/db") {
		t.Errorf("DATABASE_URL does not name the parent's database:\n%s", cfg)
	}

	// Uploads are content, and the rows saying which post owns which file are
	// in the shared database — so they have to be the same files, read-only.
	up := readAll(t, filepath.Join(wt, ".ddev", "docker-compose.hostshift-uploads.yaml"))
	want := main + "/web/app/uploads:/var/www/html/web/app/uploads:ro"
	if !strings.Contains(up, want) {
		t.Errorf("uploads not mounted from the parent:\n%s", up)
	}

	if !strings.Contains(errOut, "shares acme's database") {
		t.Errorf("the consequence was not stated:\n%s", errOut)
	}

	// Idempotent, like the rest.
	before := cfg + up
	if code, _, _ := run(t, "", cmdInit, "-C", wt); code != exitOK {
		t.Fatal("second run failed")
	}
	after := readAll(t, filepath.Join(wt, ".ddev", "config.hostshift.local.yaml")) +
		readAll(t, filepath.Join(wt, ".ddev", "docker-compose.hostshift-uploads.yaml"))
	if before != after {
		t.Error("not idempotent")
	}
}

// TestInitWarnsWhenUploadsCannotBeFound. Only 2 of the fleet's 66 projects set
// DDEV's upload_dirs, so the conventional layouts are tried — but only a
// directory that is really there is mounted, because mounting an empty one over
// a populated one is worse than not mounting. When none is found the media is
// missing, and that is worth saying rather than discovering.
func TestInitWarnsWhenUploadsCannotBeFound(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "acme")
	writeFile(t, main, ".ddev/config.yaml", "type: php\n") // no uploads anywhere
	git(t, main, "init", "-q", "-b", "master")
	git(t, main, "add", "-A")
	git(t, main, "commit", "-qm", "init")
	wt := filepath.Join(root, "acme-wt-a")
	git(t, main, "worktree", "add", "-q", wt, "-b", "wt-a")

	_, _, errOut := run(t, "", cmdInit, "-C", wt)
	if !strings.Contains(errOut, "no upload directory found") {
		t.Errorf("missing media was not reported:\n%s", errOut)
	}
	if _, err := os.Stat(filepath.Join(wt, ".ddev", "docker-compose.hostshift-uploads.yaml")); err == nil {
		t.Error("an empty uploads mount was written anyway")
	}
}

// TestInitDoesNotShareInTheMainCheckout: the database wiring is for worktrees.
// A project that is its own parent has nothing to point at.
func TestInitDoesNotShareInTheMainCheckout(t *testing.T) {
	wt := worktree(t, "acme", "", "wt-a") // same DDEV project as its parent
	code, _, errOut := run(t, "", cmdInit, "-C", wt)
	if code != exitOK {
		t.Fatalf("exit %d\n%s", code, errOut)
	}
	if cfg := readAll(t, filepath.Join(wt, ".ddev", "config.hostshift.local.yaml")); strings.Contains(cfg, "omit_containers") {
		t.Errorf("dropped the database of a project that owns it:\n%s", cfg)
	}
	if strings.Contains(errOut, "shares") {
		t.Errorf("claimed to share a database with itself:\n%s", errOut)
	}
}
