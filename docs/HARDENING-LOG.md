# Hardening log

A running record of the production-readiness work on Sluice: what was found,
how severe it was, what was done, and how the fix was verified.

Entries are newest-first within each pass. Every fix has a test that fails
against the previous behaviour — a finding without a regression test is a
finding that comes back.

---

## Pass 1 — deployability and the control-plane attack surface

**Date:** 2026-08-13
**Posture before:** the project built, tested and demoed cleanly, but had never
been deployed, was not under version control, and its own admin API was
unauthenticated.

### SLU-001 · Unauthenticated write access to the authorisation policy

**Severity: critical.** The defect that matters most in the whole project.

`PUT /api/policy` replaces the document that authorises every request flowing
through the data plane. It had no access control. Anyone who could reach port
8080 could turn a zero-trust router into an open one with a single request:

```console
$ curl -X PUT -d '{"source":"policy \"pwn\" { priority 1 effect allow when true }"}' \
    http://sluice:8080/api/policy
{"hash":"5aa4590a3f85","ok":true,"policies":1}
```

That was verified against a running instance before the fix. Every subsequent
request in the fleet was then authorised by a rule saying `allow when true`.
`POST /api/incidents` and `DELETE /api/incidents/{id}` were equally open.

**Fixed** in `internal/api/auth.go` with a bearer-token gate whose defaults make
the exposed configuration unreachable by accident:

- Mutating requests require `Authorization: Bearer <token>` or `X-Sluice-Token`.
  Tokens are compared as SHA-256 digests through `subtle.ConstantTimeCompare`,
  so comparison time cannot vary with how many leading characters a guess got
  right.
- A token in the **query string is deliberately not accepted** — it would land
  in every access log and proxy trace along the path.
- **Reads stay open by default.** They expose topology and cost figures, not
  control. `api.requireAuthForReads` gates them (and `/metrics`) for
  deployments that want it.
- `/healthz` and `/readyz` are *always* exempt. A probe that needs a credential
  fails during exactly the incident where the credential source is unavailable.
- Refusals are counted and logged with peer and user-agent — never with the
  presented token, which is one typo away from being a valid one in the log.
  The count surfaces on the dashboard and in `/api/status`.

**And made impossible to misconfigure**, in `internal/config/config.go`:

- The default bind address moved from `:8080` (every interface) to
  `127.0.0.1:8080`. A component that can rewrite authorisation policy should
  default to *not reachable*.
- `Normalize()` now **refuses to start** when the API is bound to a routable
  address with no token, and the error says how to fix it. Failing at deploy
  time beats failing at exploit time: a rollout failure is discovered by the
  person deploying, while they still hold the context to fix it. A 403 is
  discovered by whoever probes for it.
- The unsafe posture must be spelled out — `--dev-insecure` /
  `SLUICE_API_ALLOW_ANONYMOUS_MUTATIONS` — and when set, the process logs a
  standing warning and the dashboard shows a permanent `write API is open` chip.
- The Helm chart `fail`s at template time without `api.existingSecret` or
  `api.token`, so `helm install` cannot produce the open configuration either.

**Dashboard:** mutations go through an `apiFetch` wrapper that attaches the
token and, on 401, opens a dialog and retries once. The token lives in
`sessionStorage`, not `localStorage` — a credential that can rewrite fleet
policy should not outlive the tab opened to use it — and is never attached to
reads.

**Verified:** `TestAPIAuthentication` covers unauthenticated writes, wrong
tokens, prefix/extension of a valid token, both accepted header forms, the
query-string form being rejected, that a refused write leaves the policy hash
unchanged, and that probes never require a credential. `TestAPIPostureFailsClosed`
covers the startup refusal. Confirmed end to end against a live process: the
same `curl` above now returns 401.

### SLU-002 · Unknown `/api/*` paths returned 200 with an HTML page

**Severity: moderate.** `GET /api/does-not-exist` fell through to the SPA
handler and answered `200 text/html` with the dashboard. Every API client
parses that as success — a typo in an endpoint produced a silent no-op rather
than an error.

**Fixed:** the API is mounted on its own `ServeMux` with a `/api/` catch-all
that returns a JSON 404 listing the real endpoints. The dashboard handler is
now registered without a method and answers non-GET with 405 and an `Allow`
header, rather than the 404 the mux produced before.

*Incidental:* mounting the API separately also means a new endpoint cannot ship
without the authenticator — registering middleware per route is how that
eventually happens.

**Verified:** subtests in `TestAPIAuthentication`; confirmed live
(`404 application/json`, and `POST /` → `405 Allow: GET, HEAD`).

### SLU-003 · Environment variables were documented but ignored

**Severity: high (silent failure).** The Kubernetes manifest and the Helm chart
both set `SLUICE_ELECTRICITY_MAPS_TOKEN`. Nothing in the binary read a single
environment variable. An operator wiring up live grid-carbon data would have
seen it configured, deployed, and quietly kept getting modelled values — with
no error anywhere.

**Fixed:** `internal/config/env.go` adds a proper 12-factor layer with
precedence *defaults < file < environment < flags*, covering listen addresses,
secrets, paths and behaviour flags. Malformed values are errors, not silently
ignored defaults.

Every secret also accepts a `_FILE` suffix (`SLUICE_API_TOKEN_FILE`) that reads
from a path — the convention Docker secrets and Kubernetes projected volumes
use, and it keeps credentials out of the process environment where
`/proc/<pid>/environ` and every crash dump can read them.

`--print-env` prints the catalogue, generated from the same table the parser
uses so the two cannot drift.

**Verified:** `TestApplyEnv`, `TestApplyEnvReadsSecretFiles`.

### SLU-004 · `--print-config` was POSIX-only, and leaked secrets

**Severity: low, then moderate.** `cfg.Save("/dev/stdout")` fails anywhere that
device node does not exist — Windows, and some minimal container images.

While fixing it, a worse problem: `--print-config` is what an operator pastes
into a ticket when a deployment misbehaves, and it would have printed the API
token and the Electricity Maps token in clear.

**Fixed:** renamed to `Config.Render(io.Writer)` and writes to `os.Stdout`
directly. Both credentials are replaced with `<redacted>` — the marker is left
in so the reader knows a value exists. HTML escaping is disabled so the output
is readable and diffable. The live config is not mutated by printing it.

*Naming note:* the first version was called `WriteTo`, which `go vet` correctly
flagged as half-implementing `io.WriterTo` (whose signature returns a byte
count). Renamed rather than reshaped.

**Verified:** `TestRenderRedactsSecrets`.

### SLU-005 · `Demo.RPS = 0` silently generated 60 req/s

**Severity: moderate.** The traffic generator substituted a default of 60 when
told to run at zero. A configuration asking for a quiet system — a test driving
its own traffic, or an operator watching an idle fleet — got a flood instead.

Found because it made an integration test non-deterministic: background traffic
included denials from other profiles, so an assertion about *the* deny reason
picked up an unrelated one depending on timing. The test was right and the code
was wrong.

**Fixed:** `Generator.Run` returns immediately when `RPS <= 0`, and `init` no
longer substitutes a default. Zero now means zero.

### SLU-006 · No version control

**Severity: blocking for "deployable".** The project had no git repository. CI
needs one, image tags come from commit SHAs, and the Dockerfile takes a git
context.

**Fixed:** `git init`, baseline commit before any hardening changes so this pass
is reviewable as a diff.

### SLU-007 · No `.gitattributes` — CRLF would break Linux images

**Severity: moderate, latent.** `git add` on the initial commit warned that 81
files would be checked out with CRLF on Windows. A shell script or entrypoint
with `\r` line endings fails inside a container as `bad interpreter: /bin/sh^M`
— a build that passes on the developer's machine and dies in the registry.

**Fixed:** `.gitattributes` forces LF on checkout for every file type a Linux
container executes or parses.

### SLU-008 · Image build ran the test suite

**Severity: low.** The Dockerfile ran `go test ./...` during the build. The
suite starts ten HTTP listeners and depends on timing — fine on a workstation,
flaky in a constrained build container. A test failure at image-build time is
also discovered later and read less carefully than one in CI.

**Fixed:** removed, with a comment saying where the suite actually runs (CI,
with the race detector, before the image is built).

### Also in this pass

- Docker `CMD` now passes `--listen 0.0.0.0:8080 --dev-insecure` explicitly, so
  the zero-config `docker run` demo still works *and* announces that its write
  API is open, rather than getting there by omission.
- `docker-compose.yml` sets `SLUICE_API_TOKEN` (overridable) and the 0.0.0.0
  binds through the environment, so `configs/compose.jsonc` is byte-identical
  to what runs on a laptop.
- The Kubernetes manifest and Helm chart set `SLUICE_LISTEN_*` through the
  environment for the same reason, and both wire the API token from a Secret.
- `--help` rewritten with usage examples and the precedence order.

### Tooling note for future passes

Patching source with Python on Windows corrupted a UTF-8 character:
`pathlib.write_text` defaults to the system codepage, so an em dash was written
as a single 0x97 byte and Go refused to compile the file. Use
`write_text(..., encoding="utf-8")` — or the editor tooling — for anything
containing non-ASCII.

---

## Verification for this pass

```
gofmt      clean
go vet     clean
go test    all packages pass, ×2
```

Live checks against a running instance, before and after:

| Probe | Before | After |
|---|---|---|
| `PUT /api/policy` unauthenticated | `200 {"ok":true}` | `401` |
| `PUT /api/policy` wrong token | *(n/a)* | `401` |
| `PUT /api/policy` correct token | *(n/a)* | `200` |
| `GET /api/does-not-exist` | `200 text/html` | `404 application/json` |
| `POST /` | `404` | `405 Allow: GET, HEAD` |
| start on `0.0.0.0` with no token | starts | refuses, with remediation |

---

## Known gaps, carried forward

Ordered by what I would do next.

1. **The container image has never been built.** `docker build` is wired into CI
   but has not run locally, and the compose stack with a real Envoy has not been
   exercised end to end.
2. **The Envoy config uses YAML anchors and merge keys** for the upstream
   clusters. Envoy's parser should handle them, but "should" is not "verified",
   and a config that fails to load takes the data plane with it.
3. **The Helm chart has never been rendered** — `helm` is not installed on this
   machine. CI lints and templates it; that has not run either.
4. **No release pipeline.** No image publishing, no SBOM, no `govulncheck`, no
   signed artefacts, no multi-arch build.
5. **The race detector has never run.** The local toolchain's gcc is 32-bit
   only. CI runs it on Linux; that job has not executed yet.
6. **No structured request logging or trace propagation** in the control plane.
   Decisions carry an ID but it is not tied to an inbound trace header.
7. **No pprof endpoint**, so a production latency regression has no profiling
   path.
8. **SSE has no connection cap.** A misbehaving client could open subscribers
   without bound.
9. **mTLS is implemented but undemonstrated.** There is no certificate
   generation helper, so the data plane's identity path cannot be tried
   locally.
10. **No runbook** — no documented response for a stuck breaker, a policy
    rollback, or a region drain.
11. **No Grafana dashboard**, despite shipping the metrics and alert rules for
    one.
