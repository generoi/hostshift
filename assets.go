// Package hostshift embeds the DDEV add-on's project files so `hostshift init`
// can write them itself.
//
// It sits at the repo root rather than in ddev/ so that directory stays exactly
// what PLAN §5.7 says the add-on is — "only a compose service, no lib.sh, no
// generated files, no hooks, no guard" — which internal/config's
// TestAddonHasNoHooksOrScripts asserts by reading the directory. go:embed
// cannot reach a parent directory, so the package that embeds them has to be
// above them.
//
// Carrying the file in the binary is what keeps the per-repo footprint at zero
// (§3). The alternative is that every project commits an 84-line compose file
// most of its developers never use, or that every worktree runs
// `ddev add-on get` before it can start.
package hostshift

import _ "embed"

// ComposeService is the proxy's compose service definition, byte for byte the
// file the add-on installs.
//
//go:embed ddev/docker-compose.hostshift.yaml
var ComposeService string

// Loopback is the container-scoped loopback containment override (PLAN §4.4).
// Only production-canonical needs it, so `init` does not write it.
//
//go:embed ddev/docker-compose.hostshift-loopback.yaml
var Loopback string
