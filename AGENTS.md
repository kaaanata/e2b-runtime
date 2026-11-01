# AGENTS.md

- 2026-11-01 `volumes/native-credentials-20261101` replaces in-proxy OAuth refresh with an external Linux `credowner` process. Require `CREDPROXY_OWNER_SOCKET`; native Claude/Codex account homes are the exclusive refresh-token writers. The proxy receives access-only leases, legacy import returns 410, control identity stays fixed, inference sessions are durably pinned across restarts, and 401/429 feedback never replays a stream. Preserve exact sentinel/team/authority checks, scoped RC tokens, account metadata consistency and existing snapshot identity fences. New source/tests are not live provider acceptance. Update architecture and umbrella credential-proxy/status docs together.

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

The opt-in egress integration requires `CREDPROXY_ROOT`, `CREDPROXY_TEAM_ID`, and `CREDPROXY_OWNER_SOCKET`. `pkg/credproxy` retains its persisted node CA and masked-document management socket; `cmd/credowner` owns isolated native CLI homes and durable account/session bindings. Never add OAuth HTTP refresh or refresh-token storage back into the proxy. `credproxyctl guest` returns only masked control-account documents; legacy imports are refused. Only `sandbox.Config.CredentialTeamID` from authenticated creation authorizes interception, never runtime log metadata or guest headers. Preserve firewall/private-IP/DSCP/authority checks, exact sentinels, scoped RC tokens and h2/SSE/WSS. Keep the control identity immutable and existing inference sessions pinned; native errors cannot silently switch identities or replay streams. RC scoped tokens remain in RAM, so the shared-identity fork gate and pre-resume fences still apply. Architecture and umbrella credential-proxy/status docs carry source/test/deployment boundaries; no synthetic test proves live provider or Remote Control acceptance.
