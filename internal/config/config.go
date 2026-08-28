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
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/generoi/hostshift/internal/origin"
)

// DefaultVariantPattern prefixes the leftmost label of the base host.
//
// PLAN §5.4: prefixing the leftmost label is sufficient *provided matching is on
// exact origin equality*. The previous revision's common-suffix scheme produced
// nat.herrfors.ddev.site -> nat.wt-a.herrfors.ddev.site -> nat.wt-a.wt-a.… ,
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

	// Uncovered lists hostnames DDEV registers for this project that the map
	// does not mention. Requests for them reach hostshift and get a 421, so a
	// developer wondering why one blog of nine is dead wants this named.
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
	res := &Resolved{Map: m, Upstream: up, Source: source, DDEVHosts: ddevHostnames(ddev)}

	// A hostshift.yaml *replaces* the DDEV layer rather than merging with it:
	// an explicit map is a statement about which hosts this project serves, and
	// merging would resurrect ones the author deliberately left out. But a
	// project whose .ddev/config.yaml registers nine hostnames and whose
	// hostshift.yaml declares three will 421 the other six with no explanation,
	// which is a plausible fsi or pellervo shape. Say so.
	if ddev != nil && len(sites) > 0 {
		covered := map[string]bool{}
		for _, st := range sites {
			for _, o := range st.CanonicalSet() {
				covered[o.Host] = true
			}
			covered[st.Variant.Host] = true
		}
		for _, h := range ddevHostnames(ddev) {
			if !covered[h] {
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

// ddevHostnames is every hostname the project registers with DDEV.
func ddevHostnames(d *ddevConfig) []string {
	if d == nil {
		return nil
	}
	tld := d.ProjectTLD
	if tld == "" {
		tld = "ddev.site"
	}
	hosts := []string{d.Name + "." + tld}
	for _, h := range d.AdditionalHostnames {
		hosts = append(hosts, h+"."+tld)
	}
	return append(hosts, d.AdditionalFQDNs...)
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
	// "feature/ABC-123--herrfors.ddev.site": `check` called the map "injective
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
// ends alphanumeric. A slug ending in "." derives "wt-a.--herrfors.ddev.site",
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
	if d.Name == "" {
		return nil, "", fmt.Errorf("%s: no `name`", path)
	}
	return &d, path, nil
}

// sitesFromDDEV builds the ordered list of local hosts for free: `name` plus
// `additional_hostnames`, each suffixed with the project TLD. Canonical is the
// ddev host itself, which is the right map for browsing a db:pull'd database
// from a worktree.
func sitesFromDDEV(d *ddevConfig, pattern, slug string) ([]origin.Site, error) {
	tld := d.ProjectTLD
	if tld == "" {
		tld = "ddev.site"
	}
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
