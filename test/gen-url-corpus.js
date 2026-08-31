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
// Columns: base scheme, encoding, candidate, resolved host ("" when the parser
// rejects it). The candidate is escaped with JSON.stringify so control
// characters survive a TSV. The candidate is *encoded*; the resolved host is
// what the *decoded* form resolves to, because the consumer decodes first.

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
const tails = ["/x", "/x?q=1", ""];

// Host spellings. Everything here is a way of writing CANON that a browser may
// or may not fold onto it — plus near misses that must NOT be rewritten.
// Host spellings, built from mutators and *crossed with each other*.
//
// The flat list this replaces was one variation deep, and that is exactly what
// hid the root-dot leak: `www。example。fi` was in it and handled, `www.example.fi。`
// was not, and the combination of a fullwidth letter with a non-ASCII root dot
// was two steps past anything listed. A browser applies UTS46 to the whole host
// at once, so the spellings compose — and so must the corpus.
const mutators = {
  plain: (h) => h,
  upper: (h) => h.toUpperCase(),
  rootdot: (h) => h + ".",
  ideographicRoot: (h) => h + "\u3002",
  fullwidthRoot: (h) => h + "\uFF0E",
  halfwidthRoot: (h) => h + "\uFF61",
  ideographicDots: (h) => h.replace(/\./g, "\u3002"),
  fullwidthDots: (h) => h.replace(/\./g, "\uFF0E"),
  fullwidthFirst: (h) => "\uFF57" + h.slice(1),
  softHyphen: (h) => h.replace("exa", "exa\u00AD"),
  zeroWidth: (h) => "\u200B" + h,
  pctLetter: (h) => h.replace("a", "%61"),
  pctDots: (h) => h.replace(/\./g, "%2E"),
  tab: (h) => h.replace("example", "example\t"),
  nfd: (h) => h.replace("a", "a\u0308"),
};
// Crossed by class rather than exhaustively: a root-dot spelling over a
// letter/separator spelling is the combination that hid the leak, and the full
// 15x15 product multiplies the corpus by seven for shapes that add nothing.
const roots = ["rootdot", "ideographicRoot", "fullwidthRoot", "halfwidthRoot"];
const letters = [
  "upper", "ideographicDots", "fullwidthDots", "fullwidthFirst",
  "softHyphen", "zeroWidth", "pctLetter", "pctDots", "tab", "nfd",
];

const hostSet = new Set();
for (const a of Object.keys(mutators)) {
  hostSet.add(mutators[a](CANON));
}
for (const r of roots) {
  for (const l of letters) {
    hostSet.add(mutators[r](mutators[l](CANON)));
    hostSet.add(mutators[l](mutators[r](CANON)));
  }
}
// Near misses, which must never be rewritten.
for (const h of [
  "www.example.fi.evil.com",
  "awww.example.fi",
  "wwwXexample.fi",
  "example.fi",
  "cdn.other.example",
  "wt-a--example.ddev.site",
  "www.example.fi\u200B.evil.com",
]) {
  hostSet.add(h);
}
const hosts = [...hostSet];

// Encodings that sit *above* the URL parser: the HTML and CSS decoders run
// first, so the browser resolves the decoded form. Every mutator above is at or
// below the URL parser, and both of the last two rounds' bugs lived up here —
// the corpus contained zero character references and zero CSS escapes.
//
// The row records the *encoded* candidate and the host the decoded one resolves
// to, which is exactly what the consumer does, and the Go test feeds each
// encoding into a surface whose parser performs it.
const encoders = {
  raw: (u) => u,
  // Numeric references: an HTML attribute value decodes these.
  refs: (u) => u.replace(/\//g, "&#47;").replace(/:/g, "&#58;"),
  // Named references, the other spelling of the same thing.
  named: (u) => u.replace(/\//g, "&sol;").replace(/:/g, "&colon;"),
  // CSS hex escapes, which the CSS tokenizer decodes before anything sees a URL.
  css: (u) => u.replace(/:/g, "\\3a ").replace(/\//g, "\\2f "),
  // A character reference spelling a control the URL parser *removes*. The
  // corpus had zero of these and two consecutive leaks lived here: the decoders
  // strip tab, LF and CR and their reference spellings, but only stripForURL
  // did, and its three siblings reached it solely as a fall-through. So a
  // reference-encoded separator with a reference-encoded LF between its slashes
  // survived every decode that actually fired.
  refsctl: (u) => u.replace(/\//g, "&#47;").replace(/:/g, "&#58;")
    .replace("&#47;&#47;", "&#47;&#10;&#47;"),
  //
  // The other half of that shape — an unrelated backslash elsewhere in the
  // *buffer* — is a property of the document, not of the URL, so it belongs in
  // the Go surface wrappers rather than here: prefixing one to the candidate
  // would change what the candidate resolves to and the row's expectation with
  // it.
};

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
              let resolved = "";
              try {
                resolved = new URL(candidate, base).host;
              } catch {
                resolved = "";
              }
              const scheme = new URL(base).protocol.replace(":", "");
              for (const [enc, f] of Object.entries(encoders)) {
                // Only the shapes worth encoding: the whole cross product of
                // encodings and mutations would multiply the corpus fourfold for
                // rows that add nothing, so the encoded forms cover the plain
                // hosts and the fold family, which is where decoding matters.
                if (enc !== "raw" && (port !== "" || user !== "" || tail === "")) continue;
                // Only candidates the encoding can represent faithfully. A raw
                // backslash already in the candidate collides with the CSS
                // escape syntax — `\2f \w` decodes to `/w`, not to `/\w` — so
                // the decoded form would stop matching the host this row's
                // `resolved` was computed from, and the expectation would be
                // wrong rather than the code.
                if (enc !== "raw" && candidate.includes("\\")) continue;
                const encoded = f(candidate);
                const key = base + "\u0000" + enc + "\u0000" + encoded;
                if (seen.has(key)) continue;
                seen.add(key);
                out.push(
                  scheme + "\t" + enc + "\t" + JSON.stringify(encoded) + "\t" + resolved,
                );
              }
            }
          }
        }
      }
    }
  }
}
process.stdout.write(out.join("\n") + "\n");
