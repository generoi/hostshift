# hostshift

Serve a CMS site from a hostname other than the one baked into its database,
without rewriting the database.

A reverse proxy that maps origins in both directions: the browser talks to a
variant hostname, the application sees the hostname its database was written
for. Nothing in the database is ever rewritten.

**Status: pre-implementation.** [`PLAN.md`](PLAN.md) is the authoritative design
and is not re-decided here. [`spike/`](spike/) is the working evidence behind the
Go decision and the starting point for the framing and splicing.

Milestones are tracked in `PLAN.md` §8. Progress notes for completed
milestones live in [`docs/`](docs/).
