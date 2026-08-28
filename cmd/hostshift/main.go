// Command hostshift serves a CMS site from a hostname other than the one baked
// into its database, without rewriting the database.
//
// Conventions (PLAN §5.8): stdout is data, stderr is diagnostics, in every
// subcommand. Exit codes are 0 success, 1 runtime error, 2 invalid configuration.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/generoi/hostshift/internal/config"
	"github.com/generoi/hostshift/internal/corpus"
	"github.com/generoi/hostshift/internal/origin"
	"github.com/generoi/hostshift/internal/proxy"
	"github.com/generoi/hostshift/internal/rewrite"
)

const usage = `hostshift — serve a site from a hostname other than the one in its database

  hostshift rewrite --from https://a --to https://b < in.html > out.html
  hostshift proxy   --upstream http://web:80 --listen 0.0.0.0:8080 --slug wt-a
  hostshift map     print the resolved host map
  hostshift check   validate the config; exit 2 if invalid
  hostshift wp-cli  print wp-cli.local.yml for this project
  hostshift diff    corpus diff: crawl N pages canonical and through the proxy

The map is resolved from three layers, each overriding the last (PLAN §5.3):
DDEV defaults in .ddev/config.yaml, then hostshift.yaml, then these flags.

  -C dir        project directory (default ".")
  --slug        worktree slug; variants are derived by prefixing the leftmost
                label of each site's base host
  --from/--to   explicit index-aligned map; replaces the config files entirely
  --map C=V     the same thing written as one argument
  --upstream    upstream base URL, e.g. http://web:80
`

// version is set by the linker: -X main.version=… in the Makefile and
// Dockerfile. It must stay declared here — an -X flag naming a variable that
// does not exist is silently ignored, so the binary would have reported nothing
// and no build would have failed.
var version = "dev"

// exit codes, per PLAN §5.8
const (
	exitOK      = 0
	exitRuntime = 1
	exitConfig  = 2
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(exitConfig)
	}
	var err error
	code := exitOK
	switch os.Args[1] {
	case "rewrite":
		code, err = cmdRewrite(os.Args[2:])
	case "proxy":
		code, err = cmdProxy(os.Args[2:])
	case "map":
		code, err = cmdMap(os.Args[2:])
	case "check":
		code, err = cmdCheck(os.Args[2:])
	case "wp-cli":
		code, err = cmdWPCLI(os.Args[2:])
	case "diff":
		code, err = cmdDiff(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Fprint(os.Stderr, usage)
	case "version", "--version":
		fmt.Println(version)
	default:
		fmt.Fprintf(os.Stderr, "hostshift: unknown subcommand %q\n\n%s", os.Args[1], usage)
		code = exitConfig
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "hostshift:", err)
	}
	os.Exit(code)
}

// repeatable collects a flag that may be given more than once.
type repeatable []string

func (r *repeatable) String() string     { return strings.Join(*r, ",") }
func (r *repeatable) Set(v string) error { *r = append(*r, v); return nil }

// pairFlag accepts --map canonical=variant, the spelling PLAN §5.3 uses.
type pairFlag struct{ from, to *repeatable }

func (p pairFlag) String() string { return "" }
func (p pairFlag) Set(v string) error {
	c, variant, ok := strings.Cut(v, "=")
	if !ok {
		return errors.New(`want canonical=variant, e.g. --map https://a=https://b`)
	}
	*p.from = append(*p.from, c)
	*p.to = append(*p.to, variant)
	return nil
}

// common holds the flags every map-using subcommand shares.
type common struct {
	dir            string
	slug, upstream string
	from, to       repeatable
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.dir, "C", ".", "project directory")
	fs.StringVar(&c.slug, "slug", "", "worktree slug used to derive variant hosts")
	fs.StringVar(&c.upstream, "upstream", "", "upstream base URL, e.g. http://web:80")
	fs.Var(&c.from, "from", "canonical origin, repeatable, index-aligned with --to")
	fs.Var(&c.to, "to", "variant origin, repeatable, index-aligned with --from")
	fs.Var(pairFlag{&c.from, &c.to}, "map", "canonical=variant, repeatable")
}

func (c *common) load() (*config.Resolved, error) {
	return config.Load(c.dir, config.Flags{
		Upstream: c.upstream,
		Slug:     c.slug,
		From:     c.from,
		To:       c.to,
	})
}

func report(st *rewrite.Stats, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(os.Stderr)
		enc.SetIndent("", "  ")
		return enc.Encode(st.Snapshot())
	}
	st.WriteReport(os.Stderr)
	return nil
}

// cmdRewrite is the whole engine as a Unix filter. It is what collapses the
// corpus diff to one line, and it is the same code path the proxy uses — which
// is what test 27 asserts.
func cmdRewrite(args []string) (int, error) {
	fs := flag.NewFlagSet("rewrite", flag.ContinueOnError)
	var c common
	c.register(fs)
	ctype := fs.String("type", "text/html", "content type of the input")
	reverse := fs.Bool("reverse", false, "map variant origins back to canonical, as the request direction does")
	dryRun := fs.Bool("dry-run", false, "count rewrites but emit the input unchanged")
	explain := fs.Bool("explain", false, "trace every candidate that did not result in a rewrite")
	asJSON := fs.Bool("json", false, "emit counters as JSON on stderr")
	quiet := fs.Bool("quiet", false, "suppress the counter report")
	noSweep := fs.Bool("no-sweep", false, "disable §4.4's straggler backstop, to measure the structured pass")
	if err := fs.Parse(args); err != nil {
		return exitConfig, nil
	}
	res, err := c.load()
	if err != nil {
		return exitConfig, err
	}
	m := res.Map.Forward()
	if *reverse {
		m = res.Map.Reverse()
	}

	st := rewrite.NewStats(*explain)
	mt := strings.ToLower(strings.TrimSpace(strings.SplitN(*ctype, ";", 2)[0]))

	var src io.Reader = os.Stdin
	switch {
	case mt == "text/html":
		src = rewrite.NewResponseBody(os.Stdin, m, nil, rewrite.Options{
			DryRun:  *dryRun,
			NoSweep: *noSweep,
			Stats:   st,
			Log:     slog.New(slog.NewTextHandler(os.Stderr, nil)),
		})
	case mt == "application/json" || strings.HasSuffix(mt, "+json"):
		// JSON is buffered, not streamed (PLAN §5.8).
		body, err := io.ReadAll(os.Stdin)
		if err != nil {
			return exitRuntime, err
		}
		out := rewrite.RewriteJSON(body, m, st, *explain)
		if *dryRun {
			out = body
		}
		src = bytes.NewReader(out)
	}
	// Anything else streams through untouched and never enters a rewriter —
	// which is what test 25's per-surface counter of zero proves.

	if _, err := io.Copy(os.Stdout, src); err != nil {
		return exitRuntime, err
	}
	if !*quiet {
		if err := report(st, *asJSON); err != nil {
			return exitRuntime, err
		}
	}
	return exitOK, nil
}

func cmdProxy(args []string) (int, error) {
	fs := flag.NewFlagSet("proxy", flag.ContinueOnError)
	var c common
	c.register(fs)
	listen := fs.String("listen", "127.0.0.1:8080", "listen address; use 0.0.0.0 inside a container")
	dryRun := fs.Bool("dry-run", false, "serve responses unmodified while logging every rewrite it would have made")
	explain := fs.Bool("explain", false, "trace every candidate that did not result in a rewrite")
	strict := fs.Bool("strict-origins", false, "return 404 instead of passing a self-redirect through (PLAN §4.4)")
	noSweep := fs.Bool("no-sweep", false, "disable §4.4's straggler backstop, to measure the structured pass")
	compress := fs.Bool("compress", false, "re-encode responses per the client's Accept-Encoding, for performance work")
	maxBody := fs.Int64("max-body", proxy.DefaultMaxBody, "request-body buffering cap in bytes")
	if err := fs.Parse(args); err != nil {
		return exitConfig, nil
	}
	res, err := c.load()
	if err != nil {
		return exitConfig, err
	}
	if res.Upstream == "" {
		return exitConfig, errors.New("no upstream: pass --upstream or set `upstream:` in hostshift.yaml")
	}
	up, err := url.Parse(res.Upstream)
	if err != nil {
		return exitConfig, fmt.Errorf("upstream %q: %w", res.Upstream, err)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	p := &proxy.Proxy{
		Upstream:      up,
		Map:           res.Map,
		Stats:         rewrite.NewStats(*explain),
		DryRun:        *dryRun,
		StrictOrigins: *strict,
		NoSweep:       *noSweep,
		Compress:      *compress,
		MaxBody:       *maxBody,
		Log:           log,
	}

	fmt.Fprintf(os.Stderr, "hostshift: map from %s\n%s", res.Source, res.Map.String())
	fmt.Fprintf(os.Stderr, "hostshift: listening on %s, upstream %s\n", *listen, up)

	// No daemonising: run in the foreground and let DDEV or a supervisor own the
	// lifecycle. SIGTERM drains (PLAN §5.8).
	srv := &http.Server{Addr: *listen, Handler: p.Handler()}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sig
		log.Info("draining")
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return exitRuntime, err
	}
	p.Stats.WriteReport(os.Stderr)
	return exitOK, nil
}

func cmdMap(args []string) (int, error) {
	fs := flag.NewFlagSet("map", flag.ContinueOnError)
	var c common
	c.register(fs)
	asJSON := fs.Bool("json", false, "emit the map as JSON")
	asEnv := fs.Bool("env", false, "emit the .ddev/.env block the DDEV add-on needs")
	if err := fs.Parse(args); err != nil {
		return exitConfig, nil
	}
	res, err := c.load()
	if err != nil {
		return exitConfig, err
	}
	if *asEnv {
		// Both lists are needed, and the second is the non-obvious one: DDEV
		// puts every additional hostname on web's VIRTUAL_HOST, so without
		// narrowing it back to the canonical hosts, web and hostshift both claim
		// the variants and the router sends them to web.
		var variants, webHosts []string
		for _, s := range res.Map.Sites {
			variants = append(variants, s.Variant.Host)
			for _, a := range s.CanonicalSet() {
				if strings.HasSuffix(a.Host, ".ddev.site") {
					webHosts = append(webHosts, a.Host)
				}
			}
		}
		fmt.Printf("HOSTSHIFT_SLUG=%s\n", c.slug)
		fmt.Printf("HOSTSHIFT_VARIANTS=%s\n", strings.Join(variants, ","))
		fmt.Printf("HOSTSHIFT_WEB_HOSTS=%s\n", strings.Join(webHosts, ","))
		fmt.Fprintf(os.Stderr,
			"\nhostshift: add these to .ddev/additional_hostnames as well, or mkcert\n"+
				"issues no certificate for them and the browser gets a TLS interstitial:\n")
		for _, v := range variants {
			fmt.Fprintf(os.Stderr, "  - %s\n", strings.TrimSuffix(v, ".ddev.site"))
		}
		return exitOK, nil
	}
	if *asJSON {
		type site struct {
			Name      string   `json:"name"`
			Canonical string   `json:"canonical"`
			Aliases   []string `json:"aliases,omitempty"`
			Variant   string   `json:"variant"`
		}
		out := make([]site, 0, len(res.Map.Sites))
		for _, s := range res.Map.Sites {
			e := site{Name: s.Name, Canonical: s.Canonical.String(), Variant: s.Variant.String()}
			for _, a := range s.Aliases {
				e.Aliases = append(e.Aliases, a.String())
			}
			out = append(out, e)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return exitRuntime, err
		}
		return exitOK, nil
	}
	fmt.Fprintf(os.Stderr, "map from %s\n", res.Source)
	fmt.Print(res.Map.String())
	return exitOK, nil
}

func cmdCheck(args []string) (int, error) {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	var c common
	c.register(fs)
	if err := fs.Parse(args); err != nil {
		return exitConfig, nil
	}
	res, err := c.load()
	if err != nil {
		return exitConfig, err
	}
	n := len(res.Map.Sites)
	if res.Map.Identity() {
		fmt.Fprintf(os.Stderr, "hostshift: %d site(s) from %s — identity map, no rewrite can occur\n", n, res.Source)
	} else {
		fmt.Fprintf(os.Stderr, "hostshift: %d site(s) from %s — map is injective and anchored\n", n, res.Source)
	}

	// Warn rather than silently generate a file that would be committed
	// (PLAN §4.3). Seven repos in the fleet lack this gitignore entry.
	if warn := gitignoreWarning(c.dir); warn != "" {
		fmt.Fprintln(os.Stderr, "hostshift: warning:", warn)
	}
	return exitOK, nil
}

// cmdWPCLI prints wp-cli.local.yml.
//
// Under production-canonical there is no HTTP_HOST during `ddev wp`, so
// application.php falls through to the ddev host while the pristine dump's
// wp_blogs.domain is the production one, matched exactly by get_site_by_path().
// WP-CLI's url: sets $_SERVER['HTTP_HOST'] before WordPress bootstraps, which
// satisfies the first branch of that match with no environment variables and no
// code change. What is missing fleet-wide is a *root-level* url: — 0 of 60 repos
// have one (PLAN §4.3, measured again in M0).
func cmdWPCLI(args []string) (int, error) {
	fs := flag.NewFlagSet("wp-cli", flag.ContinueOnError)
	var c common
	c.register(fs)
	if err := fs.Parse(args); err != nil {
		return exitConfig, nil
	}
	res, err := c.load()
	if err != nil {
		return exitConfig, err
	}
	first := res.Map.Sites[0]

	// wp-cli.local.yml *replaces* wp-cli.yml rather than merging with it —
	// measured with WP-CLI 2.12.0 in the M6 pilot, where a file containing only
	// `url:` lost `path:`, `require:` and every alias, and left WP-CLI unable to
	// find the installation at all. So the existing config is carried through
	// verbatim with the root url added, rather than a bare two-line file being
	// written over it.
	//
	// Reading wp-cli.yml is WP-CLI's own format, inside the WordPress adapter
	// §10 describes, not the Genero-specific coupling §4.2 rules out.
	out := map[string]any{}
	if b, err := os.ReadFile(filepath.Join(c.dir, "wp-cli.yml")); err == nil {
		if err := yaml.Unmarshal(b, &out); err != nil {
			return exitConfig, fmt.Errorf("wp-cli.yml: %w", err)
		}
	}
	out["url"] = first.Canonical.String()

	body, err := yaml.Marshal(out)
	if err != nil {
		return exitRuntime, err
	}
	fmt.Printf("# generated by hostshift — do not commit\n"+
		"# wp-cli.local.yml replaces wp-cli.yml rather than merging with it, so this\n"+
		"# is the whole config with a root url: added.\n%s", body)

	if warn := gitignoreWarning(c.dir); warn != "" {
		fmt.Fprintln(os.Stderr, "hostshift: warning:", warn)
	}
	if len(res.Map.Sites) > 1 {
		fmt.Fprintf(os.Stderr,
			"hostshift: root url: is blog 1 (%s); reach a sibling blog with --url, e.g. `wp --url=%s`\n",
			first.Canonical, res.Map.Sites[1].Canonical)
	}
	// An alias whose url: is one of a site's *alias* origins — its ddev or
	// staging host — no longer resolves, because the database now holds the
	// canonical hostname and get_site_by_path matches exactly. Warn rather than
	// rewrite it: silently changing what `wp @ddev` means is worse than saying
	// so, especially when some of these aliases are SSH into production.
	stale := map[string]string{}
	for _, s := range res.Map.Sites {
		for _, a := range s.Aliases {
			stale[a.Host] = s.Canonical.String()
		}
	}
	for k, v := range out {
		if !strings.HasPrefix(k, "@") {
			continue
		}
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		raw, _ := m["url"].(string)
		if raw == "" {
			continue
		}
		host := strings.TrimPrefix(strings.TrimPrefix(raw, "https://"), "http://")
		host, _, _ = strings.Cut(host, "/")
		if canon, bad := stale[strings.ToLower(host)]; bad {
			fmt.Fprintf(os.Stderr,
				"hostshift: warning: alias %s points at %s, which the database no longer holds; use %s\n",
				k, host, canon)
		}
	}
	return exitOK, nil
}

// cmdDiff is the corpus diff — PLAN §7's only test that validates against
// reality. Fixtures would not have caught the double-port bug; this would.
func cmdDiff(args []string) (int, error) {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	var c common
	c.register(fs)
	canonicalBase := fs.String("canonical-base", "", "base URL to crawl; defaults to site 1's canonical origin")
	variantBase := fs.String("variant-base", "", "base URL the proxy serves; defaults to site 1's variant origin")
	n := fs.Int("n", 20, "how many pages to compare")
	pathList := fs.String("paths", "", "file of paths to compare, one per line; skips the crawl")
	timeout := fs.Duration("timeout", 30*time.Second, "per-request timeout")
	var resolve, headers repeatable
	fs.Var(&resolve, "resolve", "host:port:addr:port, like curl's --resolve; repeatable.\n"+
		"Under production-canonical the canonical base IS the production hostname,\n"+
		"so without this the crawl would hit the client's live site.")
	fs.Var(&headers, "canonical-header", "'Name: Value' added to the canonical fetch only; repeatable.\n"+
		"Use it to supply what the TLS-terminating router would have added when\n"+
		"--resolve points past it, e.g. 'X-Forwarded-Proto: https'.")
	if err := fs.Parse(args); err != nil {
		return exitConfig, nil
	}
	headerMap := map[string]string{}
	for _, h := range headers {
		k, v, ok := strings.Cut(h, ":")
		if !ok {
			return exitConfig, fmt.Errorf("--canonical-header %q: want 'Name: Value'", h)
		}
		headerMap[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	resolveMap := map[string]string{}
	for _, r := range resolve {
		parts := strings.Split(r, ":")
		if len(parts) != 4 {
			return exitConfig, fmt.Errorf("--resolve %q: want host:port:addr:port", r)
		}
		resolveMap[parts[0]+":"+parts[1]] = parts[2] + ":" + parts[3]
	}
	res, err := c.load()
	if err != nil {
		return exitConfig, err
	}
	site := res.Map.Sites[0]

	base := func(flagVal string, def origin.Origin) (*url.URL, error) {
		if flagVal == "" {
			flagVal = def.String()
		}
		return url.Parse(flagVal)
	}
	cb, err := base(*canonicalBase, site.Canonical)
	if err != nil {
		return exitConfig, fmt.Errorf("--canonical-base: %w", err)
	}
	vb, err := base(*variantBase, site.Variant)
	if err != nil {
		return exitConfig, fmt.Errorf("--variant-base: %w", err)
	}

	var paths []string
	if *pathList != "" {
		b, err := os.ReadFile(*pathList)
		if err != nil {
			return exitConfig, err
		}
		for _, line := range strings.Split(string(b), "\n") {
			if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
				paths = append(paths, line)
			}
		}
	}

	fmt.Fprintf(os.Stderr, "corpus diff: %s vs %s\n", cb, vb)
	results, err := corpus.Run(context.Background(), corpus.Options{
		Canonical: cb, Variant: vb, Map: res.Map,
		N: *n, Paths: paths, Timeout: *timeout, Resolve: resolveMap, CanonicalHeaders: headerMap,
	})
	if err != nil {
		return exitRuntime, err
	}
	if !corpus.WriteReport(os.Stdout, results) {
		return exitRuntime, nil
	}
	return exitOK, nil
}

// gitignoreWarning reports when wp-cli.local.yml would not be ignored by git.
func gitignoreWarning(dir string) string {
	if _, err := os.Stat(filepath.Join(dir, "wp-cli.yml")); err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		return "no .gitignore: add wp-cli.local.yml to it before generating one, or it will be committed"
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(strings.TrimSpace(line), "wp-cli.local.yml") {
			return ""
		}
	}
	return "wp-cli.local.yml is not in .gitignore; add it before generating one, or it will be committed"
}
