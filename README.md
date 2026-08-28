# hostshift

Serve a CMS site from a hostname other than the one baked into its database,
without rewriting the database.

A reverse proxy that maps origins in both directions: the browser talks to a
variant hostname, the application sees the hostname its database was written
for. Nothing in the database is ever rewritten.

[`PLAN.md`](PLAN.md) is the authoritative design and is not re-decided here.
[`spike/`](spike/) is the working evidence behind the Go decision. Progress notes
for completed milestones live in [`docs/`](docs/).

**Status: M1.** The rewrite engine, the anchored origin matcher and the proxy
shell are in place. The host map, request direction and request bodies are M2;
`PLAN.md` §8 has the rest.

## Using it

The rewriter works as a Unix filter, with no server involved. That is what makes
it testable, pipeable and CI-friendly — and it is the same engine the proxy runs,
which acceptance test 27 asserts.

```
hostshift rewrite --from https://a --to https://b < in.html > out.html
hostshift proxy   --upstream http://web:80 --listen 0.0.0.0:8080 --from … --to …
hostshift map     --from … --to …     print the resolved host map
hostshift check   --from … --to …     validate the map; exit 2 if invalid
```

`--from` and `--to` are repeatable and index-aligned, one pair per blog. Config
file layering (§5.3) lands in M2; until then the map is given on the command
line.

- `--dry-run` counts every rewrite it would make and changes nothing, so it is
  safe to point at a live canonical checkout.
- `--explain` traces every candidate that did *not* result in a rewrite, with the
  reason — `not-a-url`, `host-not-in-map`, `unanchored`, `identity-map`. Given
  how many silent-failure modes this design has, that trace is the difference
  between a five-minute diagnosis and an afternoon.
- stdout is data, stderr is diagnostics, in every subcommand. Exit codes are
  0 success, 1 runtime error, 2 invalid configuration.

The corpus diff (§7) collapses to one line:

```bash
curl -s https://canonical/page | hostshift rewrite --quiet --from … --to … \
  | diff - <(curl -s https://variant/page)
```

## Building

```
go build ./cmd/hostshift
go test ./...
```

Note for this machine: there is no `go` on `PATH`. Go 1.26.5 is in the nix store
at `/nix/store/4im44k446822ixjai0mdaizqskc90qxr-go-1.26.5/bin/go`; put it on
`PATH` (nix profile or devShell) before the commands above work from a plain
shell.

## The invariant to keep

Rewriting with an **identity map must produce byte-identical output**, over
`spike/corpus` and `spike/adv`. It is asserted by
`internal/rewrite.TestIdentityMapByteIdentity` and it holds by construction — the
matcher never splices when a pair maps to itself. It catches every splice and
offset defect, and it runs in milliseconds over 5.9 MB. Do not let it go red.
