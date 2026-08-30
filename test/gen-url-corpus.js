// Generate testdata/url-shapes.tsv.gz: every URL spelling we can think of, with
// the host a browser actually resolves it to.
//
// Five audit rounds in a row found a test-28 leak, and every one of them was the
// same mistake in a different place — the byte model saying one thing and the
// WHATWG URL parser saying another. Fuzzing the engine's invariants never found
// any of them, because the invariants held; what was wrong was the model. So the
// model gets an oracle.
//
// Node's URL is ada, the parser Chrome ships. The output is checked in so the Go
// test needs no node; regenerate with:
//
//     node test/gen-url-corpus.js | gzip -9 > testdata/url-shapes.tsv.gz
//
// Columns: base scheme, candidate, resolved host ("" when the parser rejects
// it). The candidate is escaped with JSON.stringify so control characters
// survive a TSV.

// Two bases, because the parser's rule for `scheme:host` with fewer than two
// slashes turns on whether the reference's scheme matches the *document's* — so
// one base exercises only half of authorityStart, and the half it skips is the
// one that leaked in round ten.
const BASES = [
  "https://wt-a--example.ddev.site/dir/page",
  "http://wt-a--example.ddev.site/dir/page",
];
const CANON = "www.example.fi";

const schemes = ["https:", "http:", "HTTPS:", "HtTp:", ""];
const slashes = ["", "/", "//", "///", "////", "\\\\", "/\\", "\\/", "//\\", "\\\\/"];
const userinfos = ["", "u@", "u:p@", "@", "u%40b@"];
const ports = ["", ":443", ":80", ":8080", ":"];
const tails = ["/x", "/x?q=1", "/x#f", "", "/"];

// Host spellings. Everything here is a way of writing CANON that a browser may
// or may not fold onto it — plus near misses that must NOT be rewritten.
const hosts = [
  CANON,
  CANON.toUpperCase(),
  CANON + ".",
  "www.exa­mple.fi",           // soft hyphen
  "ｗｗｗ.example.fi",  // fullwidth w
  "www。example。fi",       // ideographic full stop
  "www．example．fi",       // fullwidth full stop
  "www｡example｡fi",       // halfwidth ideographic full stop
  "​www.example.fi",           // zero-width space
  "www.example​.fi",
  "www.ex%61mple.fi",               // percent-encoded letter
  "%77ww.example.fi",
  "www%2Eexample%2Efi",             // percent-encoded dots
  "www.example\t.fi",               // tab
  "www.example\n.fi",
  "www.example\r.fi",
  // Near misses: none of these is the canonical host.
  "www.example.fi.evil.com",
  "awww.example.fi",
  "www.example.fi.",
  "wwwXexample.fi",
  "example.fi",
  "cdn.other.example",
  "wt-a--example.ddev.site",        // the variant itself
];

const seen = new Set();
const out = [];
for (const scheme of schemes) {
  for (const slash of slashes) {
    for (const user of userinfos) {
      for (const host of hosts) {
        for (const port of ports) {
          for (const tail of tails) {
            const candidate = scheme + slash + user + host + port + tail;
            for (const base of BASES) {
              const key = base + "\u0000" + candidate;
              if (seen.has(key)) continue;
              seen.add(key);
              let resolved = "";
              try {
                resolved = new URL(candidate, base).host;
              } catch {
                resolved = "";
              }
              out.push(
                new URL(base).protocol.replace(":", "") +
                  "\t" + JSON.stringify(candidate) + "\t" + resolved,
              );
            }
          }
        }
      }
    }
  }
}
process.stdout.write(out.join("\n") + "\n");
