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
	Upstream  string
	Slug      string
	Canonical string
	// From/To are an explicit index-aligned map that replaces the files entirely.
	From, To []string
}

// Resolved is everything the proxy needs.
type Resolved struct {
	Map      *origin.Map
	Upstream string
	Source   string // where the map came from, for diagnostics
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
		d, path, err := loadDDEV(dir)
		if err != nil {
			return nil, err
		}
		if d == nil {
			return nil, fmt.Errorf("no map: found neither hostshift.yaml nor .ddev/config.yaml in %s, and no --from/--to given", dir)
		}
		source = path + " (DDEV defaults)"
		if sites, err = sitesFromDDEV(d, pattern, f.Slug); err != nil {
			return nil, err
		}
	}

	m, err := origin.NewMap(sites)
	if err != nil {
		return nil, err
	}
	return &Resolved{Map: m, Upstream: up, Source: source}, nil
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
	return origin.Origin{Scheme: o.Scheme, Host: host, Port: o.Port}, nil
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
