// Command hostshift serves a CMS site from a hostname other than the one baked
// into its database, without rewriting the database.
//
// Conventions (PLAN §5.8): stdout is data, stderr is diagnostics, in every
// subcommand. Exit codes are 0 success, 1 runtime error, 2 invalid configuration.
package main

import (
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

	"github.com/generoi/hostshift/internal/config"
	"github.com/generoi/hostshift/internal/proxy"
	"github.com/generoi/hostshift/internal/rewrite"
)

const usage = `hostshift — serve a site from a hostname other than the one in its database

  hostshift rewrite --from https://a --to https://b < in.html > out.html
  hostshift proxy   --upstream http://web:80 --listen 0.0.0.0:8080 --slug wt-a
  hostshift map     print the resolved host map
  hostshift check   validate the config; exit 2 if invalid
  hostshift wp-cli  print wp-cli.local.yml for this project

The map is resolved from three layers, each overriding the last (PLAN §5.3):
DDEV defaults in .ddev/config.yaml, then hostshift.yaml, then these flags.

  -C dir        project directory (default ".")
  --slug        worktree slug; variants are derived by prefixing the leftmost
                label of each site's base host
  --from/--to   explicit index-aligned map; replaces the config files entirely
  --map C=V     the same thing written as one argument
  --upstream    upstream base URL, e.g. http://web:80
`

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
	case "-h", "--help", "help":
		fmt.Fprint(os.Stderr, usage)
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
	var src io.Reader = os.Stdin
	if strings.EqualFold(strings.TrimSpace(strings.SplitN(*ctype, ";", 2)[0]), "text/html") {
		src = rewrite.NewHTML(os.Stdin, m, nil, rewrite.Options{DryRun: *dryRun, Stats: st})
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
	if err := fs.Parse(args); err != nil {
		return exitConfig, nil
	}
	res, err := c.load()
	if err != nil {
		return exitConfig, err
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

	if len(res.Uncovered) > 0 {
		fmt.Fprintf(os.Stderr,
			"hostshift: warning: DDEV registers %d hostname(s) this map does not cover; "+
				"requests for them get a 421:\n", len(res.Uncovered))
		for _, h := range res.Uncovered {
			fmt.Fprintf(os.Stderr, "  %s\n", h)
		}
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
	fmt.Printf("# generated by hostshift — do not commit\nurl: %s\n", first.Canonical)

	if warn := gitignoreWarning(c.dir); warn != "" {
		fmt.Fprintln(os.Stderr, "hostshift: warning:", warn)
	}
	if len(res.Map.Sites) > 1 {
		fmt.Fprintf(os.Stderr,
			"hostshift: root url: is blog 1 (%s); sibling blogs keep working through the existing wp-cli.yml aliases\n",
			first.Canonical)
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
