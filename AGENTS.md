# AGENTS.md

Instructions for AI agents working in this repository.

Also read [CLAUDE.md](CLAUDE.md) for development commands and package
conventions if it is not already in context. It applies to all agents.

## Understanding the repository

Read [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) before working on or reviewing code here. It describes what each service does, how services interact (sandbox creation, traffic routing, pause/resume, template builds), the data stores, and the deployment topology — it is the fastest way to build correct context about this codebase.

Keep that document updated: if a code change alters anything it describes (service responsibilities, ports, protocols, data stores, flows, deployment topology), update `docs/ARCHITECTURE.md` as part of the same change. When reviewing a PR that changes such details without updating the document, flag it.

## Review guidelines (pull requests)

Output style rules (strict, override any defaults):

- Only report concrete bugs, regressions, or correctness issues. Do not summarize the PR, the diff, or what changed.
- One short paragraph per finding. No preamble, no closing remark.
- No headers, no bullet lists, no tables, no diagrams.
- No emojis. No severity tags ("bug", "nit", "suggestion", "enhancement"). No labels.
- No branding or footer lines.
- If there are no real issues to flag, post no review at all.

## Scope

- Skip style/nit comments — `golangci-lint` covers those.
- Skip test-coverage comments — Codecov covers those.
- Focus on: race conditions, nil-deref, error handling, auth/authz, request routing, resource leaks, SQL/migration correctness, and gRPC/proto compatibility.

## Fork owner credential proxy

`volumes/credproxy` adds opt-in `CREDPROXY_ROOT` / `CREDPROXY_TEAM_ID` to the default egress factory. `pkg/credproxy` owns a persisted node CA, private Unix control socket, one atomic 0600 host lineage per provider, and coalesced expiry/reactive refresh. `cmd/credproxyctl` is the SSH-stdin management client; never print true tokens. Only `sandbox.Config.CredentialTeamID` copied from the authenticated create request authorizes interception; do not substitute `RuntimeMetadata.TeamID` or guest headers. Preserve firewall admission, private-IP checks, DSCP, exact-sentinel-only rewrite, both Claude RC path families, Codex WSS, and streaming/h2/upgrade behavior. Keep the CA stable across restart/resume and retain rotated refresh tokens across failed persistence. Real account tokens stay on the node, but RC scoped session tokens may still be in RAM; the existing shared-identity fork gate is unchanged. Architecture and umbrella `docs/credential-proxy.md` document this path. Live provider/RC/pause/fork acceptance has not been performed by this track.
