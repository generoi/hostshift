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
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/generoi/hostshift/internal/config"
	"github.com/generoi/hostshift/internal/corpus"
	"github.com/generoi/hostshift/internal/ddev"
	"github.com/generoi/hostshift/internal/origin"
	"github.com/generoi/hostshift/internal/proxy"
	"github.com/generoi/hostshift/internal/rewrite"
)

const usage = `hostshift — rewrite origins in HTTP traffic, in both directions

A site's content refers to one hostname; you want to reach it at another.
hostshift maps between them: responses get the hostname the browser is on,
requests get the hostname the content was written for. Nothing is rewritten at
rest.

  hostshift rewrite   a filter — bytes on stdin, rewritten bytes on stdout
  hostshift proxy     the same rewriting in front of an upstream
  hostshift map       print the resolved map
  hostshift hosts     print the hostnames a project declares, one per line
  hostshift check     validate it; exit 2 if it is not usable
  hostshift diff      crawl a site two ways and compare, to verify a deployment

Give it a map and it runs anywhere:

  --map C=V     canonical=variant, repeatable
  --from/--to   the same thing as two index-aligned lists
  hostshift.yaml         a file, when there are aliases or many sites
  .ddev/config.yaml      read, not written — a DDEV project already declares
                         the hostnames it answers to, so the map comes for free

It scaffolds nothing: no config files written, no slug guessed from a branch,
no knowledge of any CMS. That work is opinionated and belongs to whatever knows
the stack — for DDEV, the add-on, as "ddev hostshift".

  -C dir        project directory (default ".")
  --slug        derive each variant by prefixing the leftmost label of its
                canonical host
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
	case "hosts":
		code, err = cmdHosts(os.Args[2:])
	case "map":
		code, err = cmdMap(os.Args[2:])
	case "check":
		code, err = cmdCheck(os.Args[2:])
	case "diff":
		code, err = cmdDiff(os.Args[2:])
	case "-h", "--help", "help":
		// Asked for, so it is the answer and not a diagnostic: stdout, exit 0.
		// `hostshift --help > notes.txt` produced an empty file.
		fmt.Print(usage)
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

// describe gives a subcommand's flag dump a first line saying what the
// subcommand is for, and sends an explicitly requested help to stdout with exit
// 0 rather than to stderr with exit 2. `hostshift check --help` printed nine
// flags and not one word about what it checks.
// rewritableText mirrors the proxy's set, so `rewrite --type` and the proxy
// cannot disagree about the same bytes.
func rewritableText(mt string) bool {
	switch mt {
	case "text/plain", "text/xml", "application/xml",
		"application/rss+xml", "application/atom+xml", "image/svg+xml":
		return true
	}
	return strings.HasSuffix(mt, "+xml")
}

func describe(fs *flag.FlagSet, args []string, what string) (helped bool) {
	fs.Usage = func() {
		w := fs.Output()
		fmt.Fprintf(w, "hostshift %s — %s\n\nusage: hostshift %s [flags]\n\n", fs.Name(), what, fs.Name())
		fs.PrintDefaults()
	}
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "-help" {
			fs.SetOutput(os.Stdout)
			fs.Usage()
			return true
		}
	}
	return false
}

// repeatable collects a flag that may be given more than once.
type repeatable []string

func (r *repeatable) String() string { return strings.Join(*r, ",") }

// An empty value is no value. A caller assembling a command line from a
// template — a compose file, a CI job — cannot leave a flag out conditionally,
// so `--from ""` has to mean "I have nothing to give you" rather than fail.
func (r *repeatable) Set(v string) error {
	if v == "" {
		return nil
	}
	*r = append(*r, v)
	return nil
}

// pairFlag accepts --map canonical=variant, the spelling PLAN §5.3 uses.
type pairFlag struct{ from, to *repeatable }

func (p pairFlag) String() string { return "" }

// Repeatable, and also comma-separated in one value — because the caller that
// most needs to pass a whole map is a docker-compose `command:` list, which
// cannot expand one variable into several arguments. A hostname cannot contain
// a comma, so the separator is unambiguous. Empty is no map, per repeatable.Set.
func (p pairFlag) Set(v string) error {
	if v == "" {
		return nil
	}
	for _, pair := range strings.Split(v, ",") {
		c, variant, ok := strings.Cut(pair, "=")
		if !ok {
			return errors.New(`want canonical=variant, e.g. --map https://a=https://b`)
		}
		*p.from = append(*p.from, c)
		*p.to = append(*p.to, variant)
	}
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
	if describe(fs, args, "the whole engine as a Unix filter: bytes on stdin, rewritten bytes on stdout") {
		return exitOK, nil
	}
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
	case mt == "text/html" || mt == "application/xhtml+xml":
		src = rewrite.NewResponseBody(os.Stdin, m, nil, rewrite.Options{
			DryRun:      *dryRun,
			NoSweep:     *noSweep,
			Stats:       st,
			Log:         slog.New(slog.NewTextHandler(os.Stderr, nil)),
			XMLEntities: mt == "application/xhtml+xml",
		})
	case mt == "application/json" || mt == "text/json" || strings.HasSuffix(mt, "+json"):
		// JSON is buffered, not streamed (PLAN §5.8).
		body, err := io.ReadAll(os.Stdin)
		if err != nil {
			return exitRuntime, err
		}
		log := slog.New(slog.NewTextHandler(os.Stderr, nil))
		out := rewrite.RewriteJSON(body, m, st, log, *explain)
		if !*noSweep {
			// Inside the repair: the sweep is a raw byte matcher, so a host it
			// rewrites inside a serialized string leaves the length stale. On
			// RewriteJSON's decline path — a duplicate member is legal JSON and
			// is rejected — the sweep is the only pass that touches the body, so
			// it corrupted the blob while logging a line that reads like a save.
			out = rewrite.RepairSerialized(out, func(b []byte) []byte {
				return rewrite.SweepBytes(b, m, st, log)
			})
		}
		if *dryRun {
			out = body
		}
		src = bytes.NewReader(out)

	case rewritableText(mt):
		// The same set the proxy rewrites. `rewrite` is documented as "the same
		// engine", and it was not: text/plain, XML, RSS and SVG streamed through
		// byte-identical with no counters and nothing from --explain, while the
		// proxy rewrote every one of them. A developer debugging why a feed or a
		// sitemap looks wrong pipes it through here, sees an empty counter block,
		// and concludes the engine cannot do it — at the moment it just learned.
		body, err := io.ReadAll(os.Stdin)
		if err != nil {
			return exitRuntime, err
		}
		log := slog.New(slog.NewTextHandler(os.Stderr, nil))
		// Wrapped in RepairSerialized, exactly as the proxy's text arm is. This
		// command documents itself as "the same engine"; it was not, and a
		// serialized blob piped through it came out with its length prefix stale
		// — the very corruption the proxy had just been taught to prevent.
		var ev []origin.Event
		out := rewrite.RepairSerialized(body, func(b []byte) []byte {
			nv, nev := m.RewriteText(b, rewrite.SurfaceText, *explain)
			ev = append(ev, nev...)
			// The XML family's parser decodes character references; plain text
			// has no parser, so leaving them is correct there. The counted forms
			// because --json and --dry-run are this command's whole output.
			if strings.HasSuffix(mt, "xml") {
				return rewrite.HostLeaksXMLCounted(m, nv, false, st, rewrite.SurfaceText, 0)
			}
			return rewrite.HostLeaksCounted(m, nv, false, st, rewrite.SurfaceText, 0)
		})
		st.Record(rewrite.SurfaceText, 0, ev)
		if !*noSweep {
			// Inside the repair: the sweep is a raw byte matcher, so a host it
			// rewrites inside a serialized string leaves the length stale. On
			// RewriteJSON's decline path — a duplicate member is legal JSON and
			// is rejected — the sweep is the only pass that touches the body, so
			// it corrupted the blob while logging a line that reads like a save.
			out = rewrite.RepairSerialized(out, func(b []byte) []byte {
				return rewrite.SweepBytes(b, m, st, log)
			})
		}
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
	if describe(fs, args, "the same rewriting in front of an upstream") {
		return exitOK, nil
	}
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
	// The version, on the line every `ddev logs -s hostshift` starts with. Both
	// image-skew outages so far looked identical from the outside — the command
	// line was right and the binary parsed it differently — and neither the
	// startup banner nor `check` could tell you which binary was running.
	fmt.Fprintf(os.Stderr, "hostshift %s: listening on %s, upstream %s\n", version, *listen, up)

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

// cmdHosts prints the hostnames a project declares — the raw material a map is
// derived from, before any of it is decided.
//
// It reads; it does not scaffold. Something has to answer "what does the
// checkout at this path answer to", and a tool that already reads DDEV config
// as a map source may as well say what it read. Doing it in shell means parsing
// YAML with its override merging, which is how the careful parts get lost.
func cmdHosts(args []string) (int, error) {
	fs := flag.NewFlagSet("hosts", flag.ContinueOnError)
	dir := fs.String("C", ".", "project directory")
	if describe(fs, args, "print the hostnames a project declares, one per line") {
		return exitOK, nil
	}
	if err := fs.Parse(args); err != nil {
		return exitConfig, nil
	}
	proj, err := ddev.Load(*dir)
	if err != nil {
		return exitConfig, err
	}
	if proj == nil {
		return exitConfig, fmt.Errorf("no project config found in %s", *dir)
	}
	// One hostname per line is a contract, and a hostname with a space in it
	// breaks it silently: the caller splits on whitespace and gets two. It is
	// reachable — DDEV names an unnamed project after its directory, and a
	// directory may be called "My Site" — and `map` rejects the same project with
	// exit 2, so printing it here made the two disagree about the same config.
	for _, h := range proj.Hosts {
		if _, err := origin.Parse("https://" + h); err != nil {
			return exitConfig, fmt.Errorf("%s declares %q, which is not a hostname: %w", ddev.Path(*dir), h, err)
		}
	}
	for _, h := range proj.Hosts {
		fmt.Println(h)
	}
	return exitOK, nil
}

func cmdMap(args []string) (int, error) {
	fs := flag.NewFlagSet("map", flag.ContinueOnError)
	var c common
	c.register(fs)
	asJSON := fs.Bool("json", false, "emit the map as JSON")
	pairs := fs.Bool("pairs", false, "emit canonical=variant, one per line, for --map")
	hosts := fs.Bool("variant-hosts", false, "emit the variant hostnames, one per line")
	canon := fs.Bool("canonical-hosts", false, "emit every canonical-side hostname, aliases included, one per line")
	if describe(fs, args, "print the resolved map, and where it came from") {
		return exitOK, nil
	}
	if err := fs.Parse(args); err != nil {
		return exitConfig, nil
	}
	res, err := c.load()
	if err != nil {
		return exitConfig, err
	}
	// Flat output, because the only consumer that needs the whole map is shell,
	// and shell should not be parsing JSON. It was: the add-on shelled out to
	// python3 to read --json, an undeclared dependency whose absence produced
	// "could not resolve a map to hand the proxy" and named neither python3 nor
	// the cause. Printing its own map flat is as generic as printing it as JSON.
	// Every hostname the *content* may name, which is the canonical of each site
	// plus its aliases. The variant side is what the browser uses; this side is
	// what the application would reach out to, which is what loopback
	// containment has to cover.
	if *canon {
		for _, s := range res.Map.Sites {
			for _, o := range s.CanonicalSet() {
				fmt.Println(o.Host)
			}
		}
		return exitOK, nil
	}
	if *pairs || *hosts {
		for _, s := range res.Map.Sites {
			if *pairs {
				fmt.Printf("%s=%s\n", s.Canonical.String(), s.Variant.String())
			} else {
				fmt.Println(s.Variant.Host)
			}
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
	if describe(fs, args, "validate the map; exit 2 if it is not usable") {
		return exitOK, nil
	}
	if err := fs.Parse(args); err != nil {
		return exitConfig, nil
	}
	res, err := c.load()
	if err != nil {
		return exitConfig, err
	}
	n := len(res.Map.Sites)
	code := exitOK
	if res.Map.Identity() {
		// "no rewrite can occur" is the definition of not usable, and check
		// documents itself as exiting 2 when the map is not usable. Exiting 0
		// made it a status line rather than a check.
		fmt.Fprintf(os.Stderr, "hostshift: %d site(s) from %s — identity map, no rewrite can occur\n", n, res.Source)
		code = exitConfig
	} else {
		fmt.Fprintf(os.Stderr, "hostshift: %d site(s) from %s — map is injective and anchored\n", n, res.Source)
	}

	if len(res.Uncovered) > 0 {
		fmt.Fprintf(os.Stderr,
			"hostshift: warning: DDEV registers %d hostname(s) this map does not cover, so they\n"+
				"  have no variant and cannot be previewed here:\n", len(res.Uncovered))
		for _, h := range res.Uncovered {
			fmt.Fprintf(os.Stderr, "  %s\n", h)
		}
	}

	return code, nil
}

// cmdDiff is the corpus diff — PLAN §7's only test that validates against
// reality. Fixtures would not have caught the double-port bug; this would.
// isLoopbackHost reports whether a crawl of this host stays on the machine.
func isLoopbackHost(h string) bool {
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	// `.ddev.site` resolves to 127.0.0.1 by wildcard, which is what a worktree
	// crawl uses and is exactly the case that must not warn.
	return strings.HasSuffix(h, ".ddev.site") || strings.HasSuffix(h, ".test")
}

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
	if describe(fs, args, "crawl a site two ways and compare, to verify a deployment") {
		return exitOK, nil
	}
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
		// Keyed through the same fold the dialer looks up with, so what the
		// developer typed and what net/http asks for are the same string.
		resolveMap[corpus.ResolveKey(parts[0]+":"+parts[1])] = parts[2] + ":" + parts[3]
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

	// Say so before crawling the client's live site.
	//
	// Under production-canonical the canonical base *is* the production
	// hostname, and this fetches `-n` pages from it — on the host, outside the
	// loopback containment the add-on ships a whole compose file to provide.
	// `--resolve`'s help text names the hazard; nothing said it at the moment it
	// happens, which is the only moment it can be acted on.
	// Whether *this* host is covered, not whether the flag was passed at all.
	// `--resolve` copies curl's syntax and so inherits curl's classic mistake —
	// the wrong port, or a host that is not the one being crawled — and the
	// crawl then falls through to real DNS while the warning stays silent. A
	// guardrail switched off by a typo is worse than none, because it reads as
	// confirmation.
	port := cb.Port()
	if port == "" {
		port = map[string]string{"https": "443", "http": "80"}[cb.Scheme]
	}
	// The same key the dialer will look up, built by the same function. Asking
	// this question a second way is what made the guardrail disagree with the
	// dialer: it folded case where the dialer did not, and did not fold IDNA
	// where the dialer did, so `--resolve www.hämeenlinna.fi:443:…` connected to
	// the live site with no warning while the punycode spelling that worked
	// warned anyway.
	_, covered := resolveMap[corpus.ResolveKey(net.JoinHostPort(cb.Hostname(), port))]
	if !covered && !isLoopbackHost(cb.Hostname()) {
		fmt.Fprintf(os.Stderr,
			"hostshift: crawling %d page(s) from %s, which is not pointed anywhere local.\n"+
				"  Under production-canonical that is the client's live site. Pass --resolve\n"+
				"  to send these fetches somewhere else, as the loopback containment does for\n"+
				"  the application's own requests.\n", *n, cb.Host)
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
