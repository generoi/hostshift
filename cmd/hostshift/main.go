// Command hostshift serves a CMS site from a hostname other than the one baked
// into its database, without rewriting the database.
//
// Conventions (PLAN §5.8): stdout is data, stderr is diagnostics, in every
// subcommand. Exit codes are 0 success, 1 runtime error, 2 invalid configuration.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/generoi/hostshift/internal/origin"
	"github.com/generoi/hostshift/internal/proxy"
	"github.com/generoi/hostshift/internal/rewrite"
)

const usage = `hostshift — serve a site from a hostname other than the one in its database

  hostshift rewrite --from https://a --to https://b < in.html > out.html
  hostshift proxy   --upstream http://web:80 --listen 0.0.0.0:8080 --from … --to …
  hostshift map     --from … --to …          print the resolved host map
  hostshift check   --from … --to …          validate the map; exit 2 if invalid

--from and --to are repeatable and index-aligned, one pair per blog. Config file
layering (PLAN §5.3) lands in M2; until then the map is given on the command line.
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

// mapFlags are the flags shared by every subcommand that needs a host map.
type mapFlags struct {
	from, to repeatable
}

func (m *mapFlags) register(fs *flag.FlagSet) {
	fs.Var(&m.from, "from", "canonical origin, repeatable, index-aligned with --to")
	fs.Var(&m.to, "to", "variant origin, repeatable, index-aligned with --from")
}

// build turns the flags into a validated matcher. Every error it returns is a
// configuration error.
func (m *mapFlags) build() (*origin.Matcher, error) {
	if len(m.from) == 0 {
		return nil, errors.New("no map: pass at least one --from/--to pair")
	}
	if len(m.from) != len(m.to) {
		return nil, fmt.Errorf("map is not index-aligned: %d --from against %d --to", len(m.from), len(m.to))
	}
	pairs := make([]origin.Pair, 0, len(m.from))
	for i := range m.from {
		c, err := origin.Parse(m.from[i])
		if err != nil {
			return nil, err
		}
		v, err := origin.Parse(m.to[i])
		if err != nil {
			return nil, err
		}
		pairs = append(pairs, origin.Pair{Canonical: c, Variant: v, Name: fmt.Sprintf("site%d", i+1)})
	}
	mt, err := origin.NewMatcher(pairs)
	if err != nil {
		return nil, err
	}
	if err := mt.Validate(); err != nil {
		return nil, err
	}
	return mt, nil
}

// report writes the counters to stderr, or JSON to stderr when asked. stdout
// stays data-only.
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
	var mf mapFlags
	mf.register(fs)
	ctype := fs.String("type", "text/html", "content type of the input")
	dryRun := fs.Bool("dry-run", false, "count rewrites but emit the input unchanged")
	explain := fs.Bool("explain", false, "trace every candidate that did not result in a rewrite")
	asJSON := fs.Bool("json", false, "emit counters as JSON on stderr")
	quiet := fs.Bool("quiet", false, "suppress the counter report")
	if err := fs.Parse(args); err != nil {
		return exitConfig, nil
	}
	m, err := mf.build()
	if err != nil {
		return exitConfig, err
	}

	st := rewrite.NewStats(*explain)
	var src io.Reader = os.Stdin

	mt := *ctype
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = mt[:i]
	}
	if strings.EqualFold(strings.TrimSpace(mt), "text/html") {
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
	var mf mapFlags
	mf.register(fs)
	upstream := fs.String("upstream", "", "upstream base URL, e.g. http://web:80")
	listen := fs.String("listen", "127.0.0.1:8080", "listen address")
	canonical := fs.String("canonical", "", "Host to present upstream; defaults to the first --from host")
	dryRun := fs.Bool("dry-run", false, "serve responses unmodified while logging every rewrite it would have made")
	explain := fs.Bool("explain", false, "trace every candidate that did not result in a rewrite")
	if err := fs.Parse(args); err != nil {
		return exitConfig, nil
	}
	if *upstream == "" {
		return exitConfig, errors.New("--upstream is required")
	}
	up, err := url.Parse(*upstream)
	if err != nil {
		return exitConfig, fmt.Errorf("--upstream: %w", err)
	}
	m, err := mf.build()
	if err != nil {
		return exitConfig, err
	}
	host := *canonical
	if host == "" {
		host = m.Pairs()[0].Canonical.HostPort()
	}

	p := &proxy.Proxy{
		Upstream:  up,
		Matcher:   m,
		Stats:     rewrite.NewStats(*explain),
		DryRun:    *dryRun,
		Canonical: host,
		Log:       slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	// No daemonising: run in the foreground and let DDEV or a supervisor own the
	// lifecycle (PLAN §5.8).
	fmt.Fprintf(os.Stderr, "hostshift: listening on %s, upstream %s, Host: %s\n", *listen, up, host)
	if err := http.ListenAndServe(*listen, p.Handler()); err != nil {
		return exitRuntime, err
	}
	return exitOK, nil
}

func cmdMap(args []string) (int, error) {
	fs := flag.NewFlagSet("map", flag.ContinueOnError)
	var mf mapFlags
	mf.register(fs)
	asJSON := fs.Bool("json", false, "emit the map as JSON")
	if err := fs.Parse(args); err != nil {
		return exitConfig, nil
	}
	m, err := mf.build()
	if err != nil {
		return exitConfig, err
	}
	if *asJSON {
		type entry struct{ Name, Canonical, Variant string }
		var out []entry
		for _, p := range m.Pairs() {
			out = append(out, entry{p.Name, p.Canonical.String(), p.Variant.String()})
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return exitRuntime, err
		}
		return exitOK, nil
	}
	fmt.Print(m.String())
	return exitOK, nil
}

func cmdCheck(args []string) (int, error) {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	var mf mapFlags
	mf.register(fs)
	if err := fs.Parse(args); err != nil {
		return exitConfig, nil
	}
	m, err := mf.build()
	if err != nil {
		return exitConfig, err
	}
	n := len(m.Pairs())
	if m.Identity() {
		fmt.Fprintf(os.Stderr, "hostshift: %d site(s), identity map — no rewrite can occur\n", n)
	} else {
		fmt.Fprintf(os.Stderr, "hostshift: %d site(s), map is injective and anchored\n", n)
	}
	return exitOK, nil
}
