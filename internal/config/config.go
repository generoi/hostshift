// Package config resolves the host map from the three layers of PLAN §5.3:
// DDEV defaults, hostshift.yaml, and CLI flags, each overriding the last.
//
// Discovery by probing is impossible and would be a silent no-op — WP_HOME
// already follows the request host, so a probe with Host: X returns home =
// https://X and the proxy would conclude canonical == variant (PLAN §4.1). The
// map is declared, never discovered.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/generoi/hostshift/internal/origin"
)

// DefaultVariantPattern prefixes the leftmost label of the base host.
//
// PLAN §5.4: prefixing the leftmost label is sufficient *provided matching is on
// exact origin equality*. The previous revision's common-suffix scheme produced
// nat.acmecorp.ddev.site -> nat.wt-a.acmecorp.ddev.site -> nat.wt-a.wt-a.… ,
// the double-port bug in a new costume.
const DefaultVariantPattern = "{slug}--{leftmost-label}"

// File is hostshift.yaml.
type File struct {
	Version  int    `yaml:"version"`
	Upstream string `yaml:"upstream"`
	Variant  struct {
		Pattern string `yaml:"pattern"`
	} `yaml:"variant"`
	Sites []SiteFile `yaml:"sites"`
}

// SiteFile is one entry under `sites:`.
type SiteFile struct {
	Name      string   `yaml:"name"`
	Canonical string   `yaml:"canonical"`
	Base      string   `yaml:"base"`
	Aliases   []string `yaml:"aliases"`
	Variant   string   `yaml:"variant"` // explicit override for the rare case
}

// Flags are the CLI overrides. They win over every file (PLAN §5.3).
type Flags struct {
	Upstream string
	Slug     string
	// From/To are an explicit index-aligned map that replaces the files entirely.
	From, To []string
}

// Resolved is everything the proxy needs.
type Resolved struct {
	Map      *origin.Map
	Upstream string
	Source   string // where the map came from, for diagnostics

	// DDEVHosts is every hostname this DDEV project registers — `name` plus
	// `additional_hostnames`, suffixed with the project TLD. It is read even
	// when hostshift.yaml supplies the map, because `map --env` needs to know
	// which of the project's hostnames belong to `web` and which to hostshift.
	DDEVHosts []string

	// ProjectTLD is the DDEV project TLD, "ddev.site" unless overridden. Three
	// of the 64 local fleet projects override it, and `map --env` has to strip
	// the right suffix when it prints what to add to additional_hostnames —
	// DDEV appends the TLD itself, so trimming ".ddev.site" from a
	// ".ddev.local" host printed the whole hostname and registered
	// "wt-a--fsi.ddev.local.ddev.local", which mkcert then issues no SAN for.
	ProjectTLD string

	// Uncovered lists hostnames DDEV registers for this project that the map
	// does not mention, so they have no variant and cannot be reached here — a
	// hostshift.yaml declaring three of nine blogs leaves the other six with
	// nowhere to be previewed. The project's own primary hostname is exempt:
	// in a worktree it belongs to web by design.
	Uncovered []string
}

// Load resolves the map for a project directory.
func Load(dir string, f Flags) (*Resolved, error) {
	// Layer 3 first when it is total: an explicit --from/--to map needs no files.
	if len(f.From) > 0 || len(f.To) > 0 {
		m, err := mapFromFlags(f)
		if err != nil {
			return nil, err
		}
		return &Resolved{Map: m, Upstream: f.Upstream, Source: "--from/--to"}, nil
	}

	var (
		sites   []origin.Site
		up      = f.Upstream
		source  string
		pattern = DefaultVariantPattern
	)

	// The DDEV config is read whether or not it supplies the map, for two
	// reasons. `map --env` needs the project's full hostname list to work out
	// which hostnames stay with `web` — in a worktree those are not the
	// canonical hosts at all, since canonical is a different project still
	// running. And a hostshift.yaml that omits some of the project's registered
	// hostnames can then be reported rather than silently 421ing them.
	ddev, ddevPath, err := loadDDEV(dir)
	if err != nil {
		return nil, err
	}

	switch hf, path, err := loadFile(dir); {
	case err != nil:
		return nil, err
	case hf != nil:
		source = path
		if hf.Variant.Pattern != "" {
			pattern = hf.Variant.Pattern
		}
		if up == "" {
			up = hf.Upstream
		}
		if sites, err = sitesFromFile(hf, pattern, f.Slug); err != nil {
			return nil, err
		}
	default:
		// Layer 1. For a single-environment site with no extra aliases this is
		// sufficient on its own and no hostshift.yaml is needed at all.
		if ddev == nil {
			return nil, fmt.Errorf("no map: found neither hostshift.yaml nor .ddev/config.yaml in %s, and no --from/--to given", dir)
		}
		source = ddevPath + " (DDEV defaults)"
		if sites, err = sitesFromDDEV(ddev, pattern, f.Slug); err != nil {
			return nil, err
		}
	}

	m, err2 := origin.NewMap(sites)
	if err = err2; err != nil {
		return nil, err
	}
	res := &Resolved{Map: m, Upstream: up, Source: source,
		DDEVHosts: ddevHostnames(ddev), ProjectTLD: projectTLD(ddev)}

	// A hostshift.yaml *replaces* the DDEV layer rather than merging with it:
	// an explicit map is a statement about which hosts this project serves, and
	// merging would resurrect ones the author deliberately left out. But a
	// project whose .ddev/config.yaml registers nine hostnames and whose
	// hostshift.yaml declares three will 421 the other six with no explanation,
	// which is a plausible fsi or bravoinc shape. Say so.
	if ddev != nil && len(sites) > 0 {
		covered := map[string]bool{}
		for _, st := range sites {
			for _, o := range st.CanonicalSet() {
				covered[o.Host] = true
			}
			covered[st.Variant.Host] = true
		}
		// The project's own primary hostname is exempt. In a worktree it is
		// supposed to be absent from the map — acmecorp-wt-a.ddev.site is what
		// web answers to, and web is where mailpit and `ddev launch` live — so
		// warning about it fires on every correctly configured worktree, which
		// is how people learn to skip warnings. In a canonical project it is a
		// canonical host anyway and never reaches here.
		own := ddev.Name + "." + projectTLD(ddev)
		for _, h := range ddevHostnames(ddev) {
			if !covered[h] && h != own {
				res.Uncovered = append(res.Uncovered, h)
			}
		}
	}
	return res, nil
}

// DDEVEnv returns the two lists the DDEV add-on needs in .ddev/.env.
//
// The second is the non-obvious one. DDEV puts every additional hostname on
// web's VIRTUAL_HOST, so unless it is narrowed, web and hostshift both claim the
// variants and the router picks web — WordPress then sees a variant host,
// fails to match wp_blogs.domain, and redirects to wp-signup.php.
//
// web keeps *this project's* hostnames minus the variants, not the canonical
// set. The two coincide for a canonical project and diverge for a worktree
// sharing canonical's database: there, canonical is a separate project that is
// still running and still owns its own hostnames, and handing them to the
// worktree's web container makes two projects claim one hostname.
func (r *Resolved) DDEVEnv() (variants, webHosts []string) {
	isVariant := map[string]bool{}
	for _, s := range r.Map.Sites {
		variants = append(variants, s.Variant.Host)
		isVariant[s.Variant.Host] = true
	}
	for _, h := range r.DDEVHosts {
		if !isVariant[h] {
			webHosts = append(webHosts, h)
		}
	}
	return variants, webHosts
}

// projectTLD is the project's DDEV TLD, defaulted the way DDEV defaults it.
func projectTLD(d *ddevConfig) string {
	if d == nil || d.ProjectTLD == "" {
		return "ddev.site"
	}
	return d.ProjectTLD
}

// ddevHostnames is every hostname the project registers with DDEV.
func ddevHostnames(d *ddevConfig) []string {
	if d == nil {
		return nil
	}
	tld := projectTLD(d)
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

func mapFromFlags(f Flags) (*origin.Map, error) {
	if len(f.From) != len(f.To) {
		return nil, fmt.Errorf("map is not index-aligned: %d --from against %d --to", len(f.From), len(f.To))
	}
	sites := make([]origin.Site, 0, len(f.From))
	for i := range f.From {
		c, err := origin.Parse(f.From[i])
		if err != nil {
			return nil, err
		}
		v, err := origin.Parse(f.To[i])
		if err != nil {
			return nil, err
		}
		sites = append(sites, origin.Site{Name: fmt.Sprintf("site%d", i+1), Canonical: c, Variant: v})
	}
	return origin.NewMap(sites)
}

func loadFile(dir string) (*File, string, error) {
	path := filepath.Join(dir, "hostshift.yaml")
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	var f File
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, "", fmt.Errorf("%s: %w", path, err)
	}
	if f.Version != 0 && f.Version != 1 {
		return nil, "", fmt.Errorf("%s: version %d is not supported (this build understands version 1)", path, f.Version)
	}
	if len(f.Sites) == 0 {
		return nil, "", fmt.Errorf("%s: `sites` is empty", path)
	}
	return &f, path, nil
}

func sitesFromFile(f *File, pattern, slug string) ([]origin.Site, error) {
	sites := make([]origin.Site, 0, len(f.Sites))
	for i, s := range f.Sites {
		name := s.Name
		if name == "" {
			name = fmt.Sprintf("site%d", i+1)
		}
		if s.Canonical == "" {
			return nil, fmt.Errorf("site %q: `canonical` is required", name)
		}
		canonical, err := origin.Parse(s.Canonical)
		if err != nil {
			return nil, fmt.Errorf("site %q: %w", name, err)
		}

		var aliases []origin.Origin
		alias := func(raw string) error {
			o, err := origin.Parse(raw)
			if err != nil {
				return fmt.Errorf("site %q: %w", name, err)
			}
			if !o.Equal(canonical) {
				aliases = append(aliases, o)
			}
			return nil
		}
		if s.Base != "" {
			if err := alias(s.Base); err != nil {
				return nil, err
			}
		}
		for _, a := range s.Aliases {
			if err := alias(a); err != nil {
				return nil, err
			}
		}

		variant, err := deriveVariant(s.Variant, s.Base, s.Canonical, pattern, slug, name)
		if err != nil {
			return nil, err
		}
		sites = append(sites, origin.Site{Name: name, Canonical: canonical, Aliases: aliases, Variant: variant})
	}
	return sites, nil
}

// deriveVariant applies the pattern to the leftmost label of base. An explicit
// variant: on the site wins; base falls back to canonical when absent.
func deriveVariant(explicit, base, canonical, pattern, slug, name string) (origin.Origin, error) {
	if explicit != "" {
		return origin.Parse(explicit)
	}
	src := base
	if src == "" {
		src = canonical
	}
	o, err := origin.Parse(src)
	if err != nil {
		return origin.Origin{}, fmt.Errorf("site %q: %w", name, err)
	}
	if slug == "" {
		return origin.Origin{}, fmt.Errorf("site %q: no variant — pass --slug, or declare `variant:` on the site", name)
	}
	host, err := applyPattern(pattern, slug, o.Host)
	if err != nil {
		return origin.Origin{}, fmt.Errorf("site %q: %w", name, err)
	}

	// Back through Parse rather than assembling an Origin by hand. The slug is
	// a worktree slug, which in practice is a branch name — so uppercase and
	// "/" are the common case, not the exotic one. Assembling the struct
	// directly skipped normalisation and validation entirely, so
	// `--slug feature/ABC-123` produced the host
	// "feature/ABC-123--acmecorp.ddev.site": `check` called the map "injective
	// and anchored" and exited 0, while SiteForHost lowercases the incoming
	// Host and so could never match it. Every request 421'd.
	v, err := origin.Parse(o.Scheme + "://" + host + portSuffix(o))
	if err != nil {
		return origin.Origin{}, fmt.Errorf(
			"site %q: --slug %q derives the invalid host %q: %w\n"+
				"slugs become hostname labels, so use only letters, digits and hyphens",
			name, slug, host, err)
	}
	if v.Host != strings.ToLower(host) || !validHostLabels(v.Host) {
		return origin.Origin{}, fmt.Errorf(
			"site %q: --slug %q derives %q, which is not a usable hostname\n"+
				"slugs become hostname labels, so use only letters, digits and hyphens",
			name, slug, host)
	}
	return v, nil
}

// validHostLabels checks each dot-separated label is non-empty and starts and
// ends alphanumeric. A slug ending in "." derives "wt-a.--acmecorp.ddev.site",
// whose second label starts with a hyphen — DNS-invalid, and it resolves or not
// depending on the resolver rather than failing at startup where it belongs.
func validHostLabels(h string) bool {
	alnum := func(c byte) bool {
		return c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
	}
	for _, l := range strings.Split(h, ".") {
		if l == "" || !alnum(l[0]) || !alnum(l[len(l)-1]) {
			return false
		}
	}
	return true
}

func portSuffix(o origin.Origin) string {
	if o.Port == "" {
		return ""
	}
	return ":" + o.Port
}

// applyPattern rewrites the leftmost label of host according to pattern.
func applyPattern(pattern, slug, host string) (string, error) {
	if !strings.Contains(pattern, "{leftmost-label}") {
		return "", fmt.Errorf("variant pattern %q must contain {leftmost-label}", pattern)
	}
	label, rest, _ := strings.Cut(host, ".")
	newLabel := strings.NewReplacer("{slug}", slug, "{leftmost-label}", label).Replace(pattern)
	if newLabel == label {
		return "", fmt.Errorf("variant pattern %q leaves the host unchanged", pattern)
	}
	if rest == "" {
		return newLabel, nil
	}
	return newLabel + "." + rest, nil
}

// ddevConfig is the subset of .ddev/config.yaml hostshift reads. It is the only
// third-party format hostshift understands (PLAN §5.3).
type ddevConfig struct {
	Name                string   `yaml:"name"`
	ProjectTLD          string   `yaml:"project_tld"`
	AdditionalHostnames []string `yaml:"additional_hostnames"`
	AdditionalFQDNs     []string `yaml:"additional_fqdns"`
}

func loadDDEV(dir string) (*ddevConfig, string, error) {
	path := filepath.Join(dir, ".ddev", "config.yaml")
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	var d ddevConfig
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
		var o ddevConfig
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

// DDEVProject reports what DDEV would call this project and what it answers to.
//
// It is the shallow question — the project's own identity — as distinct from
// Load's, which is what maps to what. `init` needs both, and for a worktree they
// come from different directories: the map from the checkout whose database is
// shared, the identity from the project being configured.
func DDEVProject(dir string) (name string, hosts []string, tld string, err error) {
	d, _, err := loadDDEV(dir)
	if err != nil || d == nil {
		return "", nil, "", err
	}
	return d.Name, ddevHostnames(d), projectTLD(d), nil
}

// sitesFromDDEV builds the ordered list of local hosts for free: `name` plus
// `additional_hostnames`, each suffixed with the project TLD. Canonical is the
// ddev host itself, which is the right map for browsing a db:pull'd database
// from a worktree.
func sitesFromDDEV(d *ddevConfig, pattern, slug string) ([]origin.Site, error) {
	tld := projectTLD(d)
	hosts := []string{d.Name + "." + tld}
	for _, h := range d.AdditionalHostnames {
		hosts = append(hosts, h+"."+tld)
	}
	// additional_fqdns are already fully qualified.
	hosts = append(hosts, d.AdditionalFQDNs...)

	sites := make([]origin.Site, 0, len(hosts))
	for i, h := range hosts {
		// Skip hostnames that are already variants for this slug.
		//
		// The add-on requires the variants to be in additional_hostnames, or
		// mkcert issues no certificate for them and the browser gets a TLS
		// interstitial. But DDEV defaults turn every registered hostname into a
		// canonical site, so without this the variants become canonical *and*
		// derived, and startup fails with "variant collides with a canonical
		// origin". That is a chicken-and-egg in exactly the configuration this
		// layer exists to serve: a worktree with no hostshift.yaml at all.
		if slug != "" && strings.HasPrefix(h, slug+"--") {
			continue
		}
		c, err := origin.Parse("https://" + h)
		if err != nil {
			return nil, err
		}
		name := "main"
		if i > 0 {
			name, _, _ = strings.Cut(h, ".")
		}
		v, err := deriveVariant("", "", c.String(), pattern, slug, name)
		if err != nil {
			return nil, err
		}
		sites = append(sites, origin.Site{Name: name, Canonical: c, Variant: v})
	}
	return sites, nil
}
