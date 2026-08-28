// Package ddev is hostshift's DDEV integration, and it is exactly that: an
// integration, not the core.
//
// hostshift maps origins. It does not know what DDEV is, and a map given with
// --map or hostshift.yaml needs none of this. What DDEV supplies is one
// convenient source for that map — a project already declares the hostnames it
// answers to, so for a single-environment site the map comes for free (§5.3's
// layer 1) — plus the runtime files a project needs to route to the proxy.
//
// Keeping it in its own package is the point. §5.3 layers the map so that no
// layer is required, and hostshift had drifted into reading .ddev/config.yaml
// from the middle of map resolution and writing three .ddev/ files from `init`,
// which makes a proxy look like a DDEV add-on. It is a binary that runs
// anywhere, with a DDEV integration beside it.
package ddev

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

// Project is what a DDEV project declares about itself: what it is called, what
// it answers to, and under which TLD.
type Project struct {
	Name  string
	Hosts []string
	TLD   string
}

func load(dir string) (*config, string, error) {
	path := filepath.Join(dir, ".ddev", "config.yaml")
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	var d config
	if err := yaml.Unmarshal(b, &d); err != nil {
		return nil, "", fmt.Errorf("%s: %w", path, err)
	}

	// DDEV merges every .ddev/config.*.yaml over config.yaml, and reading only
	// the base file made hostshift's view of the project differ from DDEV's.
	//
	// That is not academic: config.*.local.yaml is gitignored by DDEV's own
	// .ddev/.gitignore, which makes it the natural place for a worktree to give
	// itself a project name of its own — two DDEV projects cannot share one
	// name, and .ddev/config.yaml is tracked, so overriding it there is the only
	// way that does not dirty the worktree. hostshift would have gone on
	// resolving the map against the parent's name while DDEV served the
	// worktree's, and every request would have 421'd.
	//
	// Scalars overwrite, lists append and dedupe, which is what DDEV does and
	// what the pilot's own override demonstrates: it repeats a hostname already
	// in config.yaml and the project ends up with one of it, not two.
	globs, _ := filepath.Glob(filepath.Join(dir, ".ddev", "config.*.yaml"))
	sort.Strings(globs)
	for _, g := range globs {
		ob, err := os.ReadFile(g)
		if err != nil {
			continue
		}
		var o config
		if err := yaml.Unmarshal(ob, &o); err != nil {
			return nil, "", fmt.Errorf("%s: %w", g, err)
		}
		if o.Name != "" {
			d.Name = o.Name
		}
		if o.ProjectTLD != "" {
			d.ProjectTLD = o.ProjectTLD
		}
		d.AdditionalHostnames = appendUnique(d.AdditionalHostnames, o.AdditionalHostnames)
		d.AdditionalFQDNs = appendUnique(d.AdditionalFQDNs, o.AdditionalFQDNs)
	}

	if d.Name == "" {
		// DDEV defaults the project name to the directory name, and hostshift
		// has to agree with it or the map is built for a project that does not
		// exist. Verified against `ddev debug configyaml`.
		//
		// It is also the thing that makes a worktree work with no configuration
		// at all: two DDEV projects cannot share a name, and .ddev/config.yaml
		// is tracked — so a repo that *omits* `name` gives every worktree its
		// own project for free, named after its own directory. Requiring the
		// field turned that into an error instead.
		abs, err := filepath.Abs(dir)
		if err != nil {
			return nil, "", err
		}
		d.Name = filepath.Base(abs)
	}
	return &d, path, nil
}

// ddevHostnames is every hostname the project registers with DDEV.
func hostnames(d *config) []string {
	if d == nil {
		return nil
	}
	tld := tldOf(d)
	all := []string{d.Name + "." + tld}
	for _, h := range d.AdditionalHostnames {
		all = append(all, h+"."+tld)
	}
	all = append(all, d.AdditionalFQDNs...)
	// Deduped as a whole, not pairwise: a generated config.*.local.yaml lists
	// the project's own hostname alongside the variants, so after the merge
	// `name` and an entry in additional_hostnames are the same host. Answering
	// to it twice is meaningless, and it reached HOSTSHIFT_WEB_HOSTS as a
	// repeated value.
	return appendUnique(nil, all)
}

// projectTLD is the project's DDEV TLD, defaulted the way DDEV defaults it.
func tldOf(d *config) string {
	if d == nil || d.ProjectTLD == "" {
		return "ddev.site"
	}
	return d.ProjectTLD
}

// DDEVProject reports what DDEV would call this project and what it answers to.
//
// It is the shallow question — the project's own identity — as distinct from
// Load's, which is what maps to what. `init` needs both, and for a worktree they
// come from different directories: the map from the checkout whose database is
// shared, the identity from the project being configured.
func Load(dir string) (*Project, error) {
	d, _, err := load(dir)
	if err != nil || d == nil {
		return nil, err
	}
	return &Project{Name: d.Name, Hosts: hostnames(d), TLD: tldOf(d)}, nil
}

// Path is where a project's DDEV config lives, for diagnostics.
func Path(dir string) string { return filepath.Join(dir, ".ddev", "config.yaml") }

func appendUnique(dst, src []string) []string {
	seen := make(map[string]bool, len(dst))
	for _, s := range dst {
		seen[s] = true
	}
	for _, s := range src {
		if !seen[s] {
			seen[s] = true
			dst = append(dst, s)
		}
	}
	return dst
}

// ddevConfig is the subset of .ddev/config.yaml hostshift reads. It is the only
// third-party format hostshift understands (PLAN §5.3).
type config struct {
	Name                string   `yaml:"name"`
	ProjectTLD          string   `yaml:"project_tld"`
	AdditionalHostnames []string `yaml:"additional_hostnames"`
	AdditionalFQDNs     []string `yaml:"additional_fqdns"`
}

// Env is the two lists the compose service reads from .ddev/.env.
//
// The second is the non-obvious one. DDEV puts every additional hostname on
// web's VIRTUAL_HOST, so unless it is narrowed, web and hostshift both claim
// the variants and the router picks web — WordPress then sees a variant host,
// fails to match wp_blogs.domain, and redirects to wp-signup.php.
//
// web keeps *this project's* hostnames minus the variants, not the canonical
// set. The two coincide for a canonical project and diverge for a worktree
// sharing canonical's database: there, canonical is a separate project that is
// still running and still owns its own hostnames, and handing them to the
// worktree's web container makes two projects claim one hostname.
func Env(projectHosts, variants []string) (variantList, webHosts []string) {
	isVariant := map[string]bool{}
	for _, v := range variants {
		isVariant[v] = true
	}
	for _, h := range projectHosts {
		if !isVariant[h] {
			webHosts = append(webHosts, h)
		}
	}
	return variants, webHosts
}
