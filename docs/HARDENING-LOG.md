# Hardening log

A running record of the production-readiness work on Sluice: what was found,
how severe it was, what was done, and how the fix was verified.

Entries are newest-first within each pass. Every fix has a test that fails
against the previous behaviour — a finding without a regression test is a
finding that comes back.

---

## Pass 4 — the Kubernetes deployment, applied for the first time

**Date:** 2026-08-22
**Posture before:** `deploy/k8s/` contained manifests nobody had ever applied.
They rendered, they were well commented, and they had never met an API server.
The previous pass listed this explicitly as a carried-forward gap: "the probes,
the PDB and the NetworkPolicy are all theory."

They were worse than theory. A first deploy on a clean cluster failed, and the
first one that *succeeded* — every pod Ready, every probe green, the dashboard
serving — denied 100% of legitimate traffic. Both findings below are invisible
without a cluster, which is why CI now creates one.

### SLU-301 · The Kubernetes deployment had no data plane

**Severity: high.** `deploy/k8s/` deployed the control plane and nothing else.
Sluice's control plane makes decisions; **Envoy is what asks it and then moves
the packets.** Deployed to Kubernetes, the project was a router that routed
nothing: the ext_authz listener was up, correctly, and had no caller.

The symptom was not an error. Pods were Ready, `/api/overview` reported three
healthy backends, `/metrics` served, and the decision count stayed at zero
forever — a system that looks perfectly healthy and is not doing its job. The
Compose stack hid this completely, because there Envoy is a sibling container.

**Fixed:** `deploy/k8s/15-envoy.yaml` — Envoy Deployment (2 replicas), Service,
and the bootstrap as a ConfigMap, with mTLS material from a Secret and a
`checksum/config` annotation so a bootstrap change actually rolls the pods
rather than leaving every replica on the old config indefinitely.

**Not a second copy of the bootstrap.** The Kubernetes data plane differs from
the Compose one in exactly four endpoint addresses, and a 327-line config
maintained twice is a config where a fix lands in one and not the other — and a
bootstrap that fails to load takes the whole data plane with it (see SLU-101,
which is that exact failure). So `scripts/render-k8s-envoy.sh` generates the
manifest from `deploy/envoy/envoy.yaml`, refuses to emit anything if a rewrite
matched nothing or a Compose hostname survived, and `make envoy-render-check`
(also a CI step) fails if the checked-in render has drifted.

### SLU-302 · Every legitimate request was denied, with the cluster fully green

**Severity: high, and the most interesting finding in the project.** After the
data plane went in, mutual TLS negotiated, requests were authorised, decisions
were recorded — and every single one was a **deny**.

The shipped Kubernetes policy recognises trust domain `cluster.local`, which is
the SPIFFE convention in Kubernetes and what SPIRE issues by default.
`scripts/gen-certs.sh` issues under `prod.internal`. So every workload presented
a valid certificate, chaining to a CA Envoy trusted, carrying an identity the
policy had never heard of:

```
"verdict":"deny","denyReason":"unrecognised trust domain",
"subject":"spiffe://prod.internal/ns/web/sa/feed"
```

Nothing in the cluster reported a problem. Readiness only asserts that the
control loop produced a traffic plan; it did, and the plan was fine. The
authorisation outcome is not a health signal.

**This is the system behaving correctly.** An identity from an unrecognised
trust domain *should* be refused — that is the entire zero-trust premise, and
the alternative (defaulting to allow on an unknown domain) is the bug. The
defect was a deployment that shipped certificates the shipped policy could not
accept, and offered no signal saying so.

**Fixed:** `make k8s-certs` issues with `TRUST_DOMAIN=cluster.local`, and the
Makefile records why in a comment at the point of use. More importantly, the
verification below now fails loudly on this class of problem rather than
requiring somebody to notice.

### SLU-303 · `make k8s-deploy` could not succeed on a clean cluster

**Severity: moderate.** The target applied the Deployment and *then* created the
API token Secret. On a cluster where that Secret already existed the order was
invisible; on a fresh one — the only case that matters for "deployable" — both
replicas sat in `CreateContainerConfigError` until the rollout timed out.

**Fixed:** namespace and Secrets first, workloads second. `k8s-deploy` now
depends on `k8s-certs`, which is idempotent and creates the namespace, the API
token and the mTLS material before anything mounts them.

### SLU-304 · The manifests could not be applied without the Prometheus Operator

**Severity: low, high friction.** The `ServiceMonitor` lived in the same file as
the Deployment. On any cluster without the operator's CRDs, `kubectl apply`
applied the eight healthy objects, then exited **non-zero** on the ninth:

```
deployment.apps/sluice created
service/sluice created
...
error: resource mapping not found for name: "sluice" ... no matches for kind
"ServiceMonitor" in version "monitoring.coreos.com/v1"
```

A perfectly good rollout reported failure, and any script gating on that exit
code would stop.

**Fixed:** split into `deploy/k8s/30-servicemonitor.yaml`, applied on request,
with the reason in the file so it does not get merged back.

### SLU-305 · `gen-certs.sh` produced nothing on Windows, and said so quietly

**Severity: moderate; portability.** Git Bash / MSYS rewrites arguments that
look like POSIX paths into Windows ones, turning `-subj /CN=sluice-demo-ca` into
`-subj 'C:/Program Files/Git/CN=sluice-demo-ca'`, which openssl rejects.

Every openssl call redirected stderr to `/dev/null` — to hide the progress
dots — which also hid the error. `set -eu` did stop the script, so the failure
was at least not silent; it just gave no reason, and left one file behind:

```
==> certificate authority
$ ls certs/
ca.key
```

**Fixed:** `MSYS_NO_PATHCONV=1` / `MSYS2_ARG_CONV_EXCL='*'` exported at the top,
and stderr routed to a log that an `EXIT` trap prints when the script dies.

### SLU-306 · Readiness was the only thing anyone could check

**Severity: this is the pass's actual finding.** Every defect above passed
`kubectl get pods`. SLU-301 passed it with no data plane at all; SLU-302 passed
it while refusing every request.

**Fixed:** `scripts/verify-k8s.sh` (`make k8s-verify`), which asserts behaviour
rather than status. It creates a probe pod satisfying the namespace's
`restricted` Pod Security admission, then checks that:

- an **anonymous** caller is refused at the TLS handshake (curl reports `000`) —
  the request never reaches a policy decision;
- an **untrusted identity** gets `403`;
- **60 authorised mutual-TLS requests** through Envoy return `200`;
- those requests **landed on upstream pods**, counted from each region's own
  `/stats` across every replica — zero here is the exact symptom of the
  route-cache bug in SLU-107;
- traffic reached **more than one region**, so a router that pins everything to
  a single backend fails;
- **Envoy's own `upstream_rq_200` counters** agree, read per pod because the
  counters are per-process and a Service-load-balanced query reads the wrong
  replica.

Every one of SLU-301 through SLU-305 fails at least one of these assertions.

**Result on the live cluster:**

```
==> identity
  ok    an anonymous caller is refused at the TLS handshake
  ok    an untrusted identity is denied
==> routing
  ok    60/60 authorised requests reached an upstream
        aws-us-east-1          served 115
        gcp-us-central1        served 28
        azure-francecentral    served 7
  ok    150 requests landed on upstreams
  ok    traffic distributed across 3 regions
==> observability
  ok    3 backends healthy and probed through cluster DNS
  ok    Envoy counted 150 successful upstream responses
```

Envoy's count and the upstreams' own count agree exactly, from two independent
sources. The distribution is the softmax allocation the control loop computed,
not round-robin.

### SLU-307 · CI now stands up a cluster

The `kubernetes` job creates a kind cluster, builds and loads the image, runs
`make k8s-deploy`, and runs `make k8s-verify`. Manifests that render are not
manifests that deploy, and manifests that deploy are not a system that routes;
this pass produced one finding at each of those three levels, so the pipeline
checks all three.

CI is now eight jobs: `test`, `build`, `policy`, `image`, `chart`, `envoy`,
`end-to-end stack`, `kubernetes`.

### SLU-308 · The Helm chart had the same missing data plane

**Severity: high.** Fixing `deploy/k8s/` (SLU-301) left the chart shipping
exactly the defect that had just been removed from the raw manifests:
`helm install` produced a control plane with nothing asking it questions, and
the README offered it as an equal alternative.

**Fixed:** `deploy/helm/sluice/templates/envoy.yaml`, gated on
`dataPlane.enabled` (default true). Its bootstrap prelude is generated from
`deploy/envoy/envoy.yaml` by the same renderer, and the **upstream clusters are
built from `.Values.backends`** — so adding a region to values gives it an Envoy
cluster, rather than a backend Sluice can choose and Envoy cannot reach. The
cluster name is the backend id, because that is what `x-sluice-backend` carries
and what `cluster_header` resolves.

The chart now `fail`s at template time on a data plane with no enabled backend,
and `required`s a TLS secret with a message naming `TRUST_DOMAIN=cluster.local`
— the mistake in SLU-302, stated where somebody is about to make it.

**Verified:** the chart's own bootstrap, extracted from the rendered ConfigMap,
loads in Envoy 1.32 — *four clusters, one listener, `configuration OK`*, with
the endpoints resolved from values:

```
sluice_authz         -> sluice.default.svc.cluster.local:8081
aws-us-east-1        -> checkout.aws-us-east-1.svc.cluster.local:8080
gcp-us-central1      -> checkout.gcp-us-central1.svc.cluster.local:8080
azure-francecentral  -> checkout.azure-francecentral.svc.cluster.local:8080
```

The `chart` job in CI now does that on every push, plus asserts the cluster
count and that the chart refuses both a missing API token and a data plane with
no certificates. Linting proves a chart renders; only Envoy proves it runs.

### SLU-309 · Every script was committed non-executable

**Severity: moderate, and it masqueraded as two unrelated failures.** Git for
Windows cannot detect the executable bit, so every script authored there is
committed `100644`. On the Linux runners, `./scripts/render-k8s-envoy.sh` and
`./scripts/gen-certs.sh` were *Permission denied*.

What CI reported was "the Kubernetes render is stale" and a `Deploy` step that
could not create the mTLS Secret — two plausible, entirely wrong diagnoses. The
render was byte-identical; reproducing it in an `ubuntu:24.04` container proved
that before any time went into chasing line endings or hash determinism.

**Fixed:** `git update-index --chmod=+x` on all three, and a check in the `test`
job that fails on any `*.sh` staged at anything but `100755`. The next script
added from a Windows checkout would land the same way, and point at the wrong
thing again.

### Also in this pass

- Three regional namespaces (`deploy/k8s/10-regions.yaml`) with `checkout`
  Deployments and Services, so the DNS names the config already referenced
  resolve to something.
- `deploy/k8s/sluice.yaml` renamed `20-control-plane.yaml`; the files now apply
  in dependency order by name.
- The image reference was `ghcr.io/Saumya-patel-31/...`. Registries reject
  uppercase repository names, so it never could have pulled.

### Verified working, first time

- The `restricted` Pod Security admission on the namespace, which rejected a
  throwaway debug pod that omitted a `securityContext` — while the Envoy
  Deployment complied as written.
- Cluster DNS to all three regional Services; probes and latency measurements
  arrived without adjustment.
- The PodDisruptionBudget, the resource requests, both probes, and the
  read-only root filesystem on every container.

### Known gaps, carried forward

- **The NetworkPolicy is still untested.** kind's default CNI does not enforce
  NetworkPolicy, so it applied cleanly and proved nothing. It needs a cluster
  running Calico or Cilium to mean anything.
- **`allowed_headers` on ext_authz remains deprecated.** Envoy 1.32 warns on
  every load. The obvious fix — hoisting the field to the filter level — does
  not parse, and round-tripping the bootstrap through a YAML library to do it
  properly would strip the comments that make that file maintainable. It is
  load-bearing (without it Sluice never sees `x-forwarded-client-cert` and every
  request is anonymous), so it stays until it can be replaced and re-tested
  against the identity path rather than merely validated.
- **Still no load test.** The claim that a decision belongs in a request path is
  untested above roughly 100 rps.
- **No pprof endpoint.**
- **The release workflow has never been tagged.**

---

## Pass 3 — observability that can be checked

**Date:** 2026-08-16
**Posture before:** the project exported 34 metric series and shipped 15 alerting
rules, with nothing to look at them through. Neither the metrics nor the rules
had ever been run against a real Prometheus.

### SLU-201 · Grafana dashboard, verified end to end

Thirty panels across six rows, built against metric names scraped from a
running instance rather than assumed. The rows follow the argument the product
makes: the fleet at a glance, where traffic is going, **the trade being made**,
SLO and health, the raw signals, and control-plane cost.

The trade row is the one worth having. Cost avoided and cost added are separate
series rather than a netted figure, so "we are spending money to buy carbon" is
visible rather than hidden inside a smaller positive number — and the latency
paid for those savings sits beside them.

Prometheus and Grafana are now part of the compose stack, provisioned so the
datasource and dashboard exist on first boot rather than being an exercise for
the reader.

**Verified against the running stack:** both scrape targets healthy, 15 rule
groups loaded (which also proves the shipped alert PromQL parses — it never had
been), and **all 30 panel queries executed through Grafana's own datasource
proxy, every one returning data**. None empty, none rejected.

### SLU-202 · A dashboard that silently shows nothing

**Severity: moderate, and specific to observability assets.** A panel querying a
metric that has been renamed does not error. It renders an empty graph, which is
indistinguishable from "nothing is happening" — and it is silent precisely when
somebody is relying on it during an incident.

**Fixed:** `scripts/check-dashboard.py`, in two modes.

- *Static*: extracts every metric name from every PromQL expression and
  compares against the registrations in `internal/app/metrics.go`. Renaming a
  metric now breaks the build rather than the dashboard. It also reports
  histograms queried without a `_bucket`/`_sum`/`_count` suffix, which return
  nothing, and lists exported metrics that no panel shows.
- *Live* (`--live`): executes every panel expression through Grafana's
  datasource proxy — the same path the panels use — and reports any query that
  is rejected or returns no series.

Both run in CI against the real stack.

The static check immediately earned itself: it found that
`sluice_backend_error_rate` had no panel. That is one of the four objectives
the router scores on, so its absence was a real gap, not an oversight about a
minor gauge. Panels for it, bytes routed, and control-loop duration were added.

### SLU-203 · Grafana registered the dashboard twice

**Severity: low.** `GF_DASHBOARDS_DEFAULT_HOME_DASHBOARD_PATH` pointed at the
same file the dashboard provider loads, so the dashboard appeared twice in
search — once provisioned with a stable UID, once with a random one that
changes on every restart, so any link to that copy rots.

**Fixed:** dropped the env var. The provisioned copy has a fixed UID, so
`/d/sluice-overview` is a stable address.

### SLU-204 · Noisy provisioning startup

**Severity: cosmetic, but it costs attention.** Grafana probes for
`provisioning/alerting`, `provisioning/plugins` and `provisioning/notifiers`
and logs an error for each missing one. Four errors on every boot trains a
reader to skim past errors, which is how a real one gets missed.

**Fixed:** empty directories with a `.gitkeep` explaining why they exist.

---

## Pass 2 — actually deploying it

**Date:** 2026-08-13
**Posture before:** the container image had never been built, and the Envoy
integration — the project's headline claim — had never once been run.

Everything in this pass was found by *running* the thing rather than reading
it. Both of the serious findings were in code that looked correct.

### SLU-101 · The shipped Envoy config could not load

**Severity: critical.** The data plane never started.

The upstream clusters were written with a YAML anchor and merge key
(`- <<: *region`) to avoid repeating outlier-detection settings three times.
Envoy parses YAML into protobuf strictly and does not implement YAML 1.1 merge
keys, so it rejected the **entire bootstrap**:

```
error initializing config '/etc/envoy/envoy.yaml':
  ... @ static_resources.clusters[2]: no such field: '<<'
```

A config that fails to load takes the whole data plane with it, so this would
have been a total outage in any real deployment.

**Fixed:** the three clusters are written out in full. The repetition is
deliberate and the comment says why, so nobody tidies it back into an anchor.

**Verified:** `envoy --mode validate -c deploy/envoy/envoy.yaml` reports OK.
That command is now the cheapest possible guard and belongs in CI.

### SLU-102 · The compose demo's identity path could never have worked

**Severity: high (design error).** With Envoy finally starting, an
*authenticated* request was still refused as `unauthenticated request rejected`,
and all three upstreams reported `served: 0`. Nothing had ever reached a
backend.

The cause is a security default I should have known: **Envoy's
`forward_client_cert_details` defaults to `SANITIZE`, which strips any
client-supplied `x-forwarded-client-cert` header.** That behaviour is
completely correct — otherwise any caller could assert whatever SPIFFE ID it
liked by setting a header — and it means the demo's premise was wrong. It sent
XFCC as an ordinary header over plain HTTP, and Envoy quite properly threw it
away.

**Fixed** by making the demo do what a real mesh does, rather than working
around the protection:

- The Envoy listener now terminates **real mutual TLS**, with
  `require_client_certificate: true` and a `validation_context` pointing at a
  local CA.
- `forward_client_cert_details: SANITIZE_SET` with
  `set_current_client_cert_details.uri: true` makes Envoy *generate* the XFCC
  header from the certificate it verified. The header Sluice reads can now only
  have come from a cert that chained to the CA.
- `SANITIZE_SET` rather than `APPEND_FORWARD`: appending would let a caller
  prepend a forged element, and while Sluice reads the last one, an operator
  reading the header would see both.
- `scripts/gen-certs.sh` produces the CA and SPIFFE workload certificates —
  including one deliberately issued for a **different trust domain but signed
  by the same CA**, which proves the trust-domain check does real work rather
  than just re-checking the chain.
- A `certs` init service generates them into a shared volume on first run, so
  the stack is still one command.
- The load generator now uses real client certificates for four distinct
  workload identities.

This turned the weakest part of the demo into the part that best demonstrates
the product: identity comes from cryptography, not from a header anyone can set.

### SLU-107 · The documented ext_authz routing mechanism did not work

**Severity: critical.** The project's headline integration. The README, the
architecture doc and the ADR all described this mechanism, and all three were
wrong.

With Envoy loading and mTLS working, authorised requests still returned 503 and
every upstream reported `served: 0`. The diagnostics said everything was fine:

```
http.sluice_edge.ext_authz.ok:  1601      # authorisation succeeding
cluster.aws-us-east-1.upstream_rq_total: 0   # nothing forwarded
{"backend":"aws-us-east-1","flags":"NC","status":503}
```

Envoy *was* receiving the routing header — the access log shows it, with
exactly the distribution Sluice computed (429 / 351 / 169 across three
regions). It simply never routed on it.

**Cause:** Envoy resolves the route once, **before the filter chain runs**. At
that point the header `ext_authz` is about to set does not exist. The claim in
the original docs — that Envoy clears its route cache when ext_authz appends
upstream headers — is not true.

Two configurations were tried and rejected empirically:

| Attempt | Result |
|---|---|
| One route per backend matching `x-sluice-backend` | falls through to the catch-all |
| `cluster_header: x-sluice-backend` alone | `NC` — no cluster |

**Fixed** with three things that only work together:

1. A single catch-all route with `cluster_header: x-sluice-backend`, which is
   Envoy's purpose-built mechanism for an external service picking the upstream.
2. `allowed_upstream_headers` on the authz response, without which the header
   never reaches the request.
3. **A Lua filter after ext_authz that modifies a request header**, which
   invalidates the cached route so the router resolves `cluster_header` against
   what ext_authz just set.

This shape is also the one that scales: adding a region means adding a cluster
and nothing else.

**Verified** through the live stack — 1,311 requests served across three
upstreams, and the routing actually reflecting policy:

| Route | Objectives | Distribution |
|---|---|---|
| interactive | 60ms SLO, latency 55% | 40 us-central1 · 19 us-east-1 · 1 france |
| batch | no SLO, cost 45% + carbon 40% | **55 francecentral** · 4 us-east-1 · 1 us-central1 |
| payments | reliability 50%, mTLS | 21 us-central1 · 9 us-east-1 · 0 france |

Batch moved 92% of its traffic to the region with 27% cheaper egress and a grid
about seven times cleaner, and paid 88ms for it. The interactive SLO kept that
same region out. That is the product working.

**Documentation corrected** in `README.md`, `docs/ARCHITECTURE.md`,
`docs/decisions.md` and the config's own comments — all four described the
mechanism that does not work. The ADR entry now records what was tried and what
the symptoms looked like, because every part of the broken version looks
correct in isolation.

**Guarded** by two new CI jobs: `envoy` validates the bootstrap loads at all,
and `stack` brings up the real compose stack and asserts that authorised
traffic reaches an upstream, an untrusted identity gets 403, and the total
served count is non-zero — zero being the precise symptom of this bug.

### SLU-108 · The mTLS server certificate omitted the hostname clients use

**Severity: moderate.** `gen-certs.sh` issued the server certificate with
`DNS:localhost,DNS:sluice` but the compose service is named `envoy`. The mutual
handshake completed — client certificate sent, verified, accepted — and then
the connection failed hostname verification.

That is a nasty failure to read: the TLS layer succeeds, the request fails, and
it looks like an authorisation problem when it is a naming one.

**Fixed:** the SAN list now covers every name a client actually dials
(`localhost`, `envoy`, `sluiced`, `sluice`, `127.0.0.1`, `::1`) and is
extensible through `SERVER_SANS` for a real deployment.

### SLU-103 · Envoy trusted every RFC1918 address as "internal"

**Severity: moderate.** Envoy warned that `internal_address_config` was unset,
and its legacy default trusts all private ranges. On an edge listener that
means anything arriving from a private network is credited with whatever
`X-Forwarded-For` it supplied — and Sluice's CIDR policies key off exactly that
address.

**Fixed:** `internal_address_config.cidr_ranges: []`. Nothing is internal.

### SLU-104 · SSE had no subscriber limit

**Severity: moderate.** Reads need no credential, so anyone who could reach the
port could open unbounded event streams, each holding a goroutine and a
512-entry channel for the life of the connection.

**Fixed:** `api.maxEventStreams` (default 64). Past the ceiling the endpoint
returns 503 with `Retry-After` rather than accepting and degrading.

**Verified:** `TestSSESubscriberLimit` opens up to the limit, asserts the next
is refused with `Retry-After`, then closes one and asserts a slot is freed —
because a leaked counter would wedge the endpoint permanently after a burst.

### SLU-105 · No request correlation, access log, or panic recovery

**Severity: moderate.** A component making authorisation decisions had no
access log at all, and a panic in any handler dropped the connection with an
unstructured trace on stderr.

**Fixed** in `internal/api/middleware.go`:

- **Correlation IDs** reuse an inbound `X-Request-Id` or the trace-id from a
  W3C `traceparent` before minting one. A control plane that always mints its
  own produces logs that cannot be joined to the caller's trace, which is
  precisely the join needed when a decision looks wrong. Caller input is
  length-bounded.
- **Access logging** at a level chosen by outcome: successful reads at debug
  (a dashboard polls once a second — that is noise at info), 4xx at warn, 5xx
  at error, and **every mutation at info**, because a policy document that can
  be replaced needs an audit trail of who replaced it.
- **Panic recovery** producing a real structured log line and a JSON 500
  carrying the correlation ID, while re-panicking on `http.ErrAbortHandler` so
  a deliberate abort is not converted into a 500.

The wrapper preserves `http.Flusher`, which has a test of its own — hiding it
would silently buffer the event stream and the dashboard would just stop
updating.

### SLU-109 · Five reachable standard-library vulnerabilities

**Severity: high.** The first `govulncheck` run the project has ever had
reported five vulnerabilities in the Go 1.26.5 standard library that Sluice's
own code paths reach — not transitively-imported-but-unused, *called*:

```
GO-2026-5972  encoding/asn1 recursion depth
  internal/proxy/proxy.go:334  proxy.buildTLS → tls.LoadX509KeyPair → asn1.Unmarshal
GO-2026-5026  net/http via golang.org/x/net/idna punycode handling
  internal/signals/probe.go:154  Prober.ProbeOnce → http.Client.Do
  internal/proxy/proxy.go:218    Proxy.ServeHTTP → ReverseProxy → Transport.RoundTrip
```

The asn1 path runs when the data plane loads its certificates; the net/http
path runs on every probe and every proxied request. All five are fixed in
1.26.6.

This is the finding that justifies the whole supply-chain job. A project with
zero third-party modules still has a dependency — the standard library — and
"we have no dependencies" is not the same as "we have nothing to patch".

**Fixed:** `toolchain go1.26.6` in `go.mod`. That is a floor rather than a
preference: the go command downloads it automatically, so a contributor on an
older release cannot build a vulnerable binary by accident. CI and the
Dockerfile pin the same version.

**Verified:** `govulncheck ./...` now reports *No vulnerabilities found*.

### SLU-106 · `docker build` reported success when the daemon was down

**Severity: low, but it wasted a cycle.** The first image build "succeeded"
with exit 0 while Docker Desktop was not running. `docker build ... | tail`
returns *tail's* exit code, so the failure was invisible.

**Fixed** in every place a piped command's status matters: `set -o pipefail`
and `${PIPESTATUS[0]}`. Worth remembering generally — a CI step that pipes to
`tee` or `tail` has the same hole.

### Also in this pass

- **`Makefile`** covering the workflows CI runs, so "passes locally" and
  "passes in the pipeline" mean the same thing — including `make vuln`,
  `make sbom`, `make certs`, `make image-smoke` and `make compose-up`.
- **`.dockerignore`**, which keeps `.git`, docs and any local credentials out
  of the build context. Anything in the context is readable in the image's
  history.
- **`docs/RUNBOOK.md`** — a response for every alert that ships, keyed to the
  rules in `deploy/prometheus/alerts.yaml`. It leads with the fact that
  ext_authz fails closed, so *control-plane availability is data-plane
  availability*.

### Verified working, first time

The container image needed no fixes: 49.3 MB, runs as uid 65532, `/readyz`
reports ready, 259 metric series exported, dashboard serves, `sluicectl` works
inside the image, and the open-write-API warning appears in the logs exactly as
designed.

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

## Verification for pass 2

The dashboard was re-checked against the live compose stack after the auth
changes, since those touched the front end too: SSE connected, six KPI tiles
populated, twenty-four decisions in the feed, no console errors, and the token
button correctly reporting `token needed`.

The credential flow was exercised end to end in the browser: a backtest with no
token returned 401, the dialog opened by itself, the entered token was stored in
`sessionStorage`, the request was retried automatically, and the backtest ran
against 800 real decisions from the running stack.

| Probe | Result |
|---|---|
| `envoy --mode validate` | OK (was: rejected the whole bootstrap) |
| no client certificate | refused at the TLS handshake |
| valid chain, untrusted domain | `403 unrecognised trust domain` |
| valid workload certificate | `200`, served by a real upstream |
| upstream requests served | 1,311 across three regions (was: 0) |
| image smoke test | ready, 259 metric series, non-root, dashboard serving |

---

## Known gaps, carried forward

Closed in pass 2: the image builds and runs, the Envoy config loads and routes,
mTLS is demonstrated end to end, request logging and correlation exist, the SSE
cap is in, and the runbook is written.

Remaining, ordered by what I would do next.

1. **Nothing in CI has ever executed.** Every workflow here is unproven —
   including the two new jobs that guard the bugs found in pass 2. They need a
   remote and one push to prove themselves.
2. **The Helm chart has never been rendered.** `helm` is not installed on this
   machine. CI lints and templates it, and now also asserts the chart refuses
   to render without an API token — unverified until CI runs.
3. **The race detector has never run.** The local toolchain's gcc is 32-bit
   only. It is wired into both CI and the release workflow.
4. **No pprof endpoint**, so a production latency regression has no profiling
   path. It needs to be opt-in and behind the auth gate — an open pprof handler
   is a memory-dump endpoint.
6. **The release workflow is written but untagged.** Multi-arch publishing,
   SBOM, provenance attestation and checksums all exist on paper only.
7. **`allowed_headers` on ext_authz is deprecated** and Envoy warns on every
   load. It is load-bearing — without it the identity header never arrives —
   so the upgrade path needs establishing before Envoy removes it.
8. **No load test.** Decision cost is measured at demo rates (~100 req/s). The
   claim that this belongs in a request path is untested at four figures.
9. **Kubernetes manifests are unapplied.** They have never met a real API
   server, so the probes, the PDB and the NetworkPolicy are all theory.
