package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	hostshift "github.com/generoi/hostshift"
	"github.com/generoi/hostshift/internal/config"
)

// cmdInit writes the two files a DDEV project needs before hostshift can serve
// anything, and derives the slug when it can.
//
// Before this, the documented setup was: run `hostshift map --env > .ddev/.env`,
// then hand-edit .ddev/config.yaml to add each variant to additional_hostnames,
// then restart. Three steps, two of them mechanical transcriptions of what
// `map` had just printed — and the redirection was destructive, because .ddev/.env
// is a project file that may already hold something else.
//
// Both files are derived from the same map, so neither is a decision anyone
// needs to make. What is left is the one thing hostshift cannot know: which
// worktree this is. Even that has an obvious answer most of the time, so it is
// guessed from the branch and printed rather than demanded.
func cmdInit(args []string) (int, error) {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	var c common
	c.register(fs)
	dryRun := fs.Bool("dry-run", false, "print the files that would be written, write nothing")
	if err := fs.Parse(args); err != nil {
		return exitConfig, nil
	}

	if c.slug == "" {
		slug, from, err := deriveSlug(c.dir)
		if err != nil {
			return exitConfig, err
		}
		c.slug = slug
		fmt.Fprintf(os.Stderr, "hostshift: slug %q, from %s — pass --slug to choose another\n", slug, from)
	}

	// A worktree with a DDEV project of its own cannot derive the canonical
	// hostnames from its own .ddev/config.yaml — that file names the hostname
	// it will be *served* at. They live in the worktree it was created from,
	// and the proxy resolves the map itself from /project, so they have to be
	// written down where it will look. That is what the tier-2 prototype does
	// by hand; this does it from what git already knows.
	if written, err := writeMapFromParent(c.dir, c.slug); err != nil {
		return exitConfig, err
	} else if written != "" {
		fmt.Fprintf(os.Stderr, "hostshift: wrote %s, canonical hostnames from %s\n", written, mainWorktree(c.dir))
	}

	res, err := c.load()
	if err != nil {
		return exitConfig, err
	}
	_, own, tld, err := config.DDEVProject(c.dir)
	if err != nil {
		return exitConfig, err
	}
	if len(own) == 0 {
		return exitConfig, fmt.Errorf("no .ddev/config.yaml in %s: init configures a DDEV project", c.dir)
	}
	// The project's own hostnames, minus the variants — which are in that list
	// only because a previous run put them there. Without this the second run
	// reads its own output back as more of the project's identity: the variants
	// become hostnames web must keep, the generated list grows, and init stops
	// being idempotent. sitesFromDDEV skips them for the same reason.
	kept := own[:0]
	for _, h := range own {
		if !strings.HasPrefix(h, c.slug+"--") {
			kept = append(kept, h)
		}
	}
	// And minus anything the parent checkout registers.
	//
	// A worktree inherits the tracked .ddev/config.yaml, so a multisite's
	// additional_hostnames come with it: herrfors-wt-a registers
	// nat.herrfors.ddev.site as well as its own name. Handing that to web makes
	// two DDEV projects claim one hostname — and the one it belongs to is the
	// canonical project, which is still running and still serving it.
	parentHosts := map[string]bool{}
	if main := mainWorktree(c.dir); main != "" && ddevProjectName(main) != ddevProjectName(c.dir) {
		_, ph, _, _ := config.DDEVProject(main)
		for _, h := range ph {
			parentHosts[h] = true
		}
	}
	mine := kept[:0]
	for _, h := range kept {
		if !parentHosts[h] {
			mine = append(mine, h)
		}
	}
	if len(mine) < len(kept) {
		fmt.Fprintf(os.Stderr,
			"hostshift: warning: this worktree also registers %d hostname(s) belonging to the\n"+
				"  checkout it came from, because .ddev/config.yaml is tracked and\n"+
				"  additional_hostnames comes with it. Two DDEV projects claiming one\n"+
				"  hostname is a race the router settles by whichever started last. Move\n"+
				"  additional_hostnames out of the tracked config.yaml to fix it properly.\n",
			len(kept)-len(mine))
	}
	res.DDEVHosts, res.ProjectTLD = mine, tld
	own = mine
	variants, webHosts := res.DDEVEnv()

	// Every hostname the project should register: its own, plus the variants.
	// The full list rather than only the new ones, because DDEV's own
	// config.yaml is left untouched and a merge that only replaced would
	// otherwise drop the project's existing hostnames.
	//
	// Short form where the project TLD can be stripped, since DDEV appends the
	// TLD to additional_hostnames; anything else is an FQDN and goes in the
	// other list.
	var short, fqdns []string
	for _, h := range append(append([]string{}, res.DDEVHosts...), variants...) {
		if s, ok := strings.CutSuffix(h, "."+res.ProjectTLD); ok {
			short = append(short, s)
		} else {
			fqdns = append(fqdns, h)
		}
	}
	short, fqdns = dedupe(short), dedupe(fqdns)

	cfg := generatedBy + " — delete it to undo, rerun to update.\n" +
		"#\n" +
		"# Registering the variant hostnames is what puts them in the mkcert\n" +
		"# certificate's SAN and in DDEV's router. A three-label variant host is not\n" +
		"# covered by the *.ddev.site wildcard, so it needs registering regardless of\n" +
		"# hostshift (PLAN §5.6).\n" +
		"#\n" +
		"# config.*.local.yaml is gitignored by DDEV's own .ddev/.gitignore, so this is\n" +
		"# local runtime config rather than per-repo installed footprint (PLAN §3).\n\n" +
		"additional_hostnames:\n"
	for _, h := range short {
		cfg += "  - " + h + "\n"
	}
	if len(fqdns) > 0 {
		cfg += "\nadditional_fqdns:\n"
		for _, h := range fqdns {
			cfg += "  - " + h + "\n"
		}
	}

	env := map[string]string{
		"HOSTSHIFT_SLUG":      c.slug,
		"HOSTSHIFT_VARIANTS":  strings.Join(variants, ","),
		"HOSTSHIFT_WEB_HOSTS": strings.Join(webHosts, ","),
	}
	envPath := filepath.Join(c.dir, ".ddev", ".env")
	merged, err := mergeEnv(envPath, env)
	if err != nil {
		return exitConfig, err
	}

	// Before writing, not after: --dry-run exists to be read.
	if len(webHosts) == 0 {
		fmt.Fprintf(os.Stderr,
			"hostshift: warning: every hostname this project registers is a variant, so web\n"+
				"  gets no VIRTUAL_HOST and mailpit and `ddev launch` stop routing. That is\n"+
				"  what naming the DDEV project after the variant does — give it a name of\n"+
				"  its own (%s-%s) and let the variants be additional hostnames.\n",
			ddevProjectName(mainWorktree(c.dir))+"", c.slug)
	}

	// The compose service, if the project has not got one.
	//
	// Written rather than committed, which is what keeps the per-repo footprint
	// at zero (PLAN §3). The alternative is that every project commits an
	// 84-line file most of its developers never use — 31 of the fleet's 61 DDEV
	// repos have done that for phpmyadmin, which reads as "whoever installed it
	// committed the output" rather than a decision — or that every worktree runs
	// `ddev add-on get` before it can start. The binary already carries the file.
	//
	// Never overwritten: a project that has installed the add-on, or pinned the
	// service deliberately, keeps what it has.
	composePath := filepath.Join(c.dir, ".ddev", "docker-compose.hostshift.yaml")
	writeCompose := ours(composePath) || hasMarker(composePath, "#ddev-generated")

	cfgPath := filepath.Join(c.dir, ".ddev", "config.hostshift.local.yaml")
	if *dryRun {
		if writeCompose {
			fmt.Printf("--- %s\n(%d bytes, the add-on's compose service)\n", composePath, len(hostshift.ComposeService))
		}
		fmt.Printf("--- %s\n%s\n--- %s\n%s", cfgPath, cfg, envPath, merged)
		return exitOK, nil
	}
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		return exitRuntime, err
	}
	if err := os.WriteFile(envPath, []byte(merged), 0o644); err != nil {
		return exitRuntime, err
	}
	if writeCompose {
		if err := os.WriteFile(composePath, []byte(hostshift.ComposeService), 0o644); err != nil {
			return exitRuntime, err
		}
		fmt.Fprintf(os.Stderr, "hostshift: wrote .ddev/docker-compose.hostshift.yaml\n")
	}

	fmt.Fprintf(os.Stderr, "hostshift: wrote .ddev/config.hostshift.local.yaml and .ddev/.env\n")
	for _, s := range res.Map.Sites {
		fmt.Fprintf(os.Stderr, "  %-6s %s  ->  %s\n", s.Name, s.Canonical, s.Variant)
	}
	fmt.Fprintf(os.Stderr, "hostshift: now run `ddev restart`\n")
	return exitOK, nil
}

// generatedBy is the first line of every file init writes, and the thing that
// makes rewriting one safe.
const generatedBy = "# generated by hostshift init"

// ours reports whether init may write path: either nothing is there, or what is
// there is something init wrote.
//
// Refusing to overwrite anything was the first rule, and it was wrong in a way
// that only shows up later. hostshift.yaml is the production-canonical
// declaration, so a hand-written one must never be clobbered — but the same
// guard pinned a *generated* one forever. Add a blog to the checkout a worktree
// came from and the worktree keeps the old map, silently, for as long as it
// exists. Anything written to disk goes out of date; the answer is to know
// which files are ours to refresh.
func ours(path string) bool {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return true
	}
	if err != nil {
		return false
	}
	return strings.HasPrefix(string(b), generatedBy)
}

// hasMarker reports whether path begins with a given generated-file marker.
// The compose service carries DDEV's own, since it is byte for byte the file
// `ddev add-on get` installs — so the same rule applies to it, and a project
// that has removed the marker to own the file keeps it.
func hasMarker(path, marker string) bool {
	b, err := os.ReadFile(path)
	return err == nil && strings.HasPrefix(string(b), marker)
}

// deriveSlug works out which worktree this is, without being told.
//
// The branch is the answer that needs no explanation: a worktree exists to hold
// a branch, and an agent asked to preview its work knows which one it is on.
//
// The worktree's own DDEV project name is *not* used, though it looks like it
// should be. Naming the project after the variant — wt-a--acme — makes the
// project's own hostname and the variant hostname the same string, so web ends
// up with no VIRTUAL_HOST at all and mailpit and `ddev launch` stop routing.
// The convention the tier-2 prototype proved keeps a name of its own, acme-wt-a,
// and registers the variants as additional hostnames; there is nothing to
// subtract from that.
//
// The guess is reported rather than made silently, because a wrong slug
// produces a hostname nobody expects and the failure — a 421, or a certificate
// with no matching SAN — does not name its cause.
func deriveSlug(dir string) (slug, from string, err error) {
	branch := gitOutput(dir, "symbolic-ref", "--short", "HEAD")
	if branch == "" {
		branch = gitOutput(dir, "rev-parse", "--abbrev-ref", "HEAD")
	}
	if branch == "" || branch == "HEAD" {
		return "", "", fmt.Errorf("cannot work out a slug here (no git branch): pass --slug")
	}
	if slug = hostLabel(branch); slug == "" {
		return "", "", fmt.Errorf("branch %q has nothing usable as a hostname label: pass --slug", branch)
	}
	return slug, "the git branch " + branch, nil
}

// mainWorktree returns the root of the worktree this one was created from, or
// "" when dir is not a linked worktree. --git-common-dir is the main .git even
// from inside a linked worktree, which is exactly the question being asked.
func mainWorktree(dir string) string {
	common := gitOutput(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	git := gitOutput(dir, "rev-parse", "--path-format=absolute", "--git-dir")
	if common == "" || common == git {
		return "" // not a worktree, or this is the main one
	}
	return filepath.Dir(common)
}

// writeMapFromParent declares the parent worktree's DDEV hostnames as this
// worktree's canonical set, when nothing else has.
//
// It returns the path written, or "" when there was nothing to do: not a linked
// worktree, the same DDEV project as its parent (then the parent's config.yaml
// is this one's too, and the DDEV layer already resolves correctly), or a
// hostshift.yaml already present — which is the production-canonical opt-in and
// must never be overwritten.
func writeMapFromParent(dir, slug string) (string, error) {
	main := mainWorktree(dir)
	if main == "" || ddevProjectName(main) == "" || ddevProjectName(main) == ddevProjectName(dir) {
		return "", nil
	}
	path := filepath.Join(dir, "hostshift.yaml")
	if !ours(path) {
		return "", nil
	}
	_, hosts, _, err := config.DDEVProject(main)
	if err != nil {
		return "", err
	}
	if len(hosts) == 0 {
		return "", fmt.Errorf("%s has no .ddev/config.yaml, so there is nothing to inherit: pass --map or write hostshift.yaml", main)
	}

	body := generatedBy + " — delete it to undo, rerun to update.\n" +
		"#\n" +
		"# The canonical hostnames are the ones " + filepath.Base(main) + " serves, because that\n" +
		"# is the checkout whose database this worktree shares. They cannot be read from\n" +
		"# this project's own .ddev/config.yaml, which names the hostname this worktree\n" +
		"# is served *at*.\n" +
		"#\n" +
		"# To browse a pristine production database instead, replace canonical: with the\n" +
		"# production hostname and leave base: as it is (PLAN §5.3).\n\n" +
		"version: 1\nupstream: http://web:80\n\nsites:\n"
	for i, h := range hosts {
		name := "main"
		if i > 0 {
			name, _, _ = strings.Cut(h, ".")
		}
		body += fmt.Sprintf("  - {name: %s, canonical: https://%s, base: https://%s}\n", name, h, h)
	}
	return path, os.WriteFile(path, []byte(body), 0o644)
}

// ddevProjectName is what DDEV would call the project in dir.
func ddevProjectName(dir string) string {
	if dir == "" {
		return ""
	}
	name, _, _, err := config.DDEVProject(dir)
	if err != nil {
		return ""
	}
	return name
}

func gitOutput(dir string, args ...string) string {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// hostLabel reduces an arbitrary string to something that can be one label of a
// hostname. Branch names carry slashes, dots and capitals — "feature/ABC-123"
// is the ordinary case, not the exotic one.
func hostLabel(s string) string {
	var b strings.Builder
	prevDash := true // never start with one
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case !prevDash:
			b.WriteByte('-')
			prevDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	// A label may be 63 bytes, but it is prefixed to a host that also has to
	// fit, and a variant host this long is unreadable long before it is
	// invalid.
	if len(out) > 30 {
		out = strings.Trim(out[:30], "-")
	}
	return out
}

// mergeEnv rewrites the keys hostshift owns and leaves every other line of
// .ddev/.env exactly as it was.
//
// The documented setup was `hostshift map --env > .ddev/.env`, which truncates.
// .ddev/.env is a project file — it is where a project puts anything its own
// compose overrides need — so the documented command could silently delete
// someone's configuration on a second run.
func mergeEnv(path string, set map[string]string) (string, error) {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var out []string
	seen := map[string]bool{}
	f, err := os.Open(path)
	if err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := sc.Text()
			k, _, isAssign := strings.Cut(line, "=")
			k = strings.TrimSpace(k)
			if v, ours := set[k]; isAssign && ours {
				out = append(out, k+"="+v)
				seen[k] = true
				continue
			}
			out = append(out, line)
		}
		if err := sc.Err(); err != nil {
			return "", fmt.Errorf("%s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	for _, k := range keys {
		if !seen[k] {
			out = append(out, k+"="+set[k])
		}
	}
	return strings.Join(out, "\n") + "\n", nil
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
