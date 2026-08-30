// Package config resolves the host map from the three layers of PLAN §5.3:
// DDEV defaults, hostshift.yaml, and CLI flags, each overriding the last.
//
// Discovery by probing is impossible and would be a silent no-op — WP_HOME
// already follows the request host, so a probe with Host: X returns home =
// https://X and the proxy would conclude canonical == variant (PLAN §4.1). The
// map is declared, never discovered.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/generoi/hostshift/internal/ddev"
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

	// Uncovered lists hostnames DDEV registers for this project that the map
	// does not mention, so they have no variant and cannot be reached here — a
	// hostshift.yaml declaring three of nine blogs leaves the other six with
	// nowhere to be previewed. The project's own primary hostname is exempt:
	// in a worktree it belongs to web by design.
	Uncovered []string
}

// Load resolves the map for a project directory.
func Load(dir string, f Flags) (*Resolved, error) {
	res, err := resolve(dir, f)
	if err != nil {
		return nil, err
	}
	// Validated here rather than where it is dialled, so `check` catches a typo
	// before a restart does. `--upstream web:80` parsed happily and was reported
	// back as `upstream web:80`, then every request failed at dial time; a URL
	// with a space came back percent-encoded, which only the author could read.
	if err := validateUpstream(res.Upstream); err != nil {
		return nil, err
	}
	return res, nil
}

// validateUpstream holds an upstream to the same standard as an origin: a
// scheme and a host, because that is all a reverse proxy can dial.
func validateUpstream(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("upstream %q: %w", raw, err)
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return fmt.Errorf("upstream %q: scheme is required (http:// or https://)", raw)
	case u.Host == "":
		return fmt.Errorf("upstream %q: no host", raw)
	case strings.ContainsAny(raw, " \t"):
		return fmt.Errorf("upstream %q: contains a space", raw)
	}
	return nil
}

func resolve(dir string, f Flags) (*Resolved, error) {
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

	// Layer 1 of §5.3, and the only place the core knows DDEV exists: a project
	// already declares the hostnames it answers to, so a single-environment site
	// gets its map for free. It is read even when hostshift.yaml supplies the
	// map, so that a yaml omitting some of those hostnames can be reported
	// rather than silently leaving them unreachable.
	//
	// Reading is the whole of it. Writing .ddev/ files and deciding the env the
	// compose service reads is opinionated setup and lives in the add-on, as
	// `ddev hostshift`. This is a binary that runs anywhere, with a DDEV
	// integration beside it.
	proj, err := ddev.Load(dir)
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
		if proj == nil {
			return nil, fmt.Errorf("no map: found neither hostshift.yaml nor .ddev/config.yaml in %s, and no --from/--to given", dir)
		}
		source = ddev.Path(dir) + " (DDEV defaults)"
		if sites, err = sitesFromHosts(proj.Hosts, pattern, f.Slug); err != nil {
			return nil, err
		}
	}

	m, err2 := origin.NewMap(sites)
	if err = err2; err != nil {
		return nil, err
	}
	res := &Resolved{Map: m, Upstream: up, Source: source}

	// A hostshift.yaml *replaces* the DDEV layer rather than merging with it:
	// an explicit map is a statement about which hosts this project serves, and
	// merging would resurrect ones the author deliberately left out. But a
	// project whose .ddev/config.yaml registers nine hostnames and whose
	// hostshift.yaml declares three will 421 the other six with no explanation,
	// which is a plausible fsi or bravoinc shape. Say so.
	if proj != nil && len(sites) > 0 {
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
		own := proj.Name + "." + proj.TLD
		for _, h := range proj.Hosts {
			if !covered[h] && h != own {
				res.Uncovered = append(res.Uncovered, h)
			}
		}
	}
	return res, nil
}

func mapFromFlags(f Flags) (*origin.Map, error) {
	// --from on its own, with --slug, derives each variant the same way every
	// other layer does. It is how a caller says "here are the canonical origins,
	// you work out the rest" without writing a file, and it is what a worktree
	// needs: the hostnames its database holds belong to a *different* directory —
	// the checkout it was branched from — so they cannot come from layer 1, which
	// only ever reads the project being configured. Without this the map is built
	// against the hostname the worktree is served at, which appears nowhere in the
	// database, and nothing rewrites.
	if len(f.To) == 0 && len(f.From) > 0 {
		if f.Slug == "" {
			return nil, errors.New("--from without --to needs --slug, to derive the variant of each canonical origin")
		}
		sites := make([]origin.Site, 0, len(f.From))
		for i, raw := range f.From {
			name := fmt.Sprintf("site%d", i+1)
			c, err := origin.Parse(raw)
			if err != nil {
				return nil, err
			}
			v, err := deriveVariant("", "", raw, DefaultVariantPattern, f.Slug, name)
			if err != nil {
				return nil, err
			}
			sites = append(sites, origin.Site{Name: name, Canonical: c, Variant: v})
		}
		return origin.NewMap(sites)
	}
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
	// KnownFields, because the failure mode of a silently-ignored key is the
	// worst one this file has: `upsteam:` left the map valid, `check` reported it
	// injective and anchored, and `proxy` then refused to start with "no
	// upstream" pointing at a key the reader can see is right there. A misspelled
	// `alias:` was quieter still — the staging hostname simply was not in the map
	// and every request to it 421'd.
	var f File
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
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
		// Name the actual problem. "use only letters, digits and hyphens" is
		// unhelpful advice for a slug that is already only letters, digits and
		// hyphens and is simply too long — and length is the reachable case,
		// since the slug prefixes the leftmost label rather than replacing it.
		hint := "slugs become hostname labels, so use only letters, digits and hyphens"
		if l := longestLabel(strings.ToLower(host)); l > 63 {
			hint = fmt.Sprintf(
				"that host has a %d-character label and DNS allows 63 — "+
					"the slug prefixes the leftmost label, so shorten the slug", l)
		}
		return origin.Origin{}, fmt.Errorf(
			"site %q: --slug %q derives %q, which is not a usable hostname\n%s",
			name, slug, host, hint)
	}
	return v, nil
}

// validHostLabels checks each dot-separated label is non-empty, no longer than
// RFC 1035's 63 octets, and starts and ends alphanumeric.
//
// A slug ending in "." derives "wt-a.--acmecorp.ddev.site", whose second label
// starts with a hyphen — DNS-invalid, and it resolves or not depending on the
// resolver rather than failing at startup where it belongs. The length is the
// same class: the variant prefixes the leftmost label, so a 30-character slug on
// a 32-character label derives a 64-octet one. Nothing rejected it — the map
// reported "injective and anchored" and exit 0 — and it fails later, as a
// certificate mkcert will not issue a SAN for, which presents as a browser
// warning rather than as a naming mistake.
func validHostLabels(h string) bool {
	alnum := func(c byte) bool {
		return c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
	}
	for _, l := range strings.Split(h, ".") {
		if l == "" || len(l) > 63 || !alnum(l[0]) || !alnum(l[len(l)-1]) {
			return false
		}
	}
	return true
}

func longestLabel(h string) int {
	n := 0
	for _, l := range strings.Split(h, ".") {
		if len(l) > n {
			n = len(l)
		}
	}
	return n
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

// sitesFromHosts derives a map from a list of hostnames a site already answers
// to: each one is canonical, and its variant is that host with the slug
// prefixed. Canonical is the local host itself, which is the right map for
// browsing a db:pull'd database from a worktree.
//
// It takes hostnames rather than a DDEV project on purpose. Where the list came
// from is not this function's business, and it is the seam that keeps DDEV an
// integration: anything that can produce a list of hostnames can seed a map.
func sitesFromHosts(hosts []string, pattern, slug string) ([]origin.Site, error) {
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
