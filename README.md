<div align="center">

# Sluice

**A zero-trust traffic router that decides where your bytes go by what they cost, what they emit, and who is asking.**

Load balancers optimise for latency because latency is the only thing they can see.
Sluice sees the egress bill and the carbon intensity of the grid each region runs on,
weighs them against the latency budget you actually have, and refuses the request
outright if policy says it should never have left your network.

[Quickstart](#quickstart) · [How routing works](#how-routing-works) · [Policy language](#the-policy-language) · [Deployment](#deployment) · [What this is not](#what-this-is-not)

</div>

---

## The problem

Cross-cloud egress is one of the largest line items in a modern infrastructure bill, and
almost nothing in the request path knows it exists. A load balancer will happily send a
256 MB batch export from the most expensive region you own, at 6pm, on the dirtiest grid
in your fleet, because that region answered a health check two milliseconds faster.

Three signals that should influence routing are invisible to the data plane:

- **Egress price** varies by roughly 40% across providers in the same metro, and by
  more than 2× across regions. It also moves — volume tiers roll over, commitments lapse.
- **Grid carbon intensity** varies by more than 20× across regions (France at ~55
  gCO2e/kWh against South Africa at ~710) and swings 30% or more across a single day as
  solar comes and goes.
- **Identity and data class** determine where traffic is *permitted* to go at all.
  Personal data belonging to an EU subject is not a routing preference; it is a
  constraint that must hold on every request.

Sluice puts all three in the decision, alongside latency, and makes the tradeoff explicit
and auditable.

## What it does

```
                    ┌─────────────────────────────────────────────────────────┐
   client ─mTLS──▶  │  DATA PLANE — Envoy (ext_authz) or the native Go proxy  │
                    └──────────────────────────┬──────────────────────────────┘
                                               │  who is this, and where should it go?
                    ┌──────────────────────────▼──────────────────────────────┐
                    │  CONTROL PLANE                                          │
                    │                                                         │
                    │   ┌──────────────┐        ┌───────────────────────┐     │
                    │   │ Policy       │        │ Routing engine        │     │
                    │   │ deny/allow   │───────▶│ normalise → weight →  │     │
                    │   │ constrain    │ prunes │ softmax → SLO guard → │     │
                    │   │ prefer       │ pool   │ capacity → damp       │     │
                    │   └──────────────┘        └───────────┬───────────┘     │
                    │           ▲                           │                 │
                    │   ┌───────┴────────┬──────────┬───────┴────────┐        │
                    │   │ egress price   │ latency  │ grid carbon    │        │
                    │   │ live + tables  │ p95 probe│ live + modelled│        │
                    │   └────────────────┴──────────┴────────────────┘        │
                    │                                                         │
                    │   decision ledger  ·  Prometheus  ·  dashboard          │
                    └─────────────────────────────────────────────────────────┘
                                AWS      ·      GCP      ·      Azure
```

Every decision is recorded with the complete derivation: the policy trace, each
candidate's raw and normalised signals, the weighted contribution of each objective, and
the counterfactual — what a latency-only balancer would have picked, and what choosing
differently cost or saved. Ask *why* about any request and the answer is one command away.

## Quickstart

No cloud accounts, no credentials, no network. One command starts ten synthetic regions
across three clouds, drives real HTTP traffic through them, injects incidents, and serves
the dashboard:

```bash
go run ./cmd/sluiced --demo
```

Then open <http://localhost:8080>.

The simulator is not a mock of the router — it is a mock of the *world*. The upstreams
are real HTTP servers on real sockets with lognormal latency distributions and heavy
tails, prices drift, grid intensity follows a diurnal curve, and the prober, proxy and
byte accounting all run the same code they would in production.

<details>
<summary><b>Try these once it is running</b></summary>

```bash
# Watch decisions stream past
go run ./cmd/sluicectl watch

# Where is traffic going, and what is it costing?
go run ./cmd/sluicectl backends
go run ./cmd/sluicectl routes

# Ask why about any decision
go run ./cmd/sluicectl decisions --limit 5
go run ./cmd/sluicectl explain dec_<id>

# Break a region and watch the router shed it
go run ./cmd/sluicectl incident aws-us-east-1 brownout --seconds 90
go run ./cmd/sluicectl incident azure-northeurope outage --seconds 60

# Tighten a residency rule and see what it would break, before applying it
go run ./cmd/sluicectl policy test configs/policies.sluice
```

</details>

The dashboard's five views: **Overview** (KPIs, live traffic by cloud, objective radar,
current allocation), **Topology** (animated flow graph with carbon/cost/latency
overlays), **Decisions** (filterable ledger with full score derivation), **Policy**
(editor with backtesting), **Signals** (per-region trends and quote provenance).

## How routing works

Routing runs in two loops, and keeping them separate is the central design decision.

**The control loop**, once a second, computes a traffic distribution per route:

1. **Normalise.** Each objective is min-max scaled across the current candidates.
   Weights like "cost matters 40%" only mean something if cost and latency share a
   scale, and the only scale they share is their spread across the options available
   right now.
2. **Score.** `score = 1 − Σ (normalised × weight)` over cost, latency, carbon and
   reliability. Every dimension is expressed as lower-is-better so the arithmetic is
   uniform.
3. **Allocate** by softmax over scores with a temperature. Argmax would be wrong three
   ways: it oscillates between near-ties, it starves the losers of the traffic needed to
   keep their latency signals honest, and it turns a small measurement error into a total
   traffic shift.
4. **Hold the SLO.** If the projected blended p95 would breach the route's target, drop
   the slowest candidate and re-solve. Cost and carbon optimisation is only legitimate
   inside the latency budget, and the router gives up savings before it gives up the SLO.
5. **Cap by capacity,** then guarantee every eligible backend a small exploration floor
   so its signals stay real rather than synthetic.
6. **Damp and gate.** The published distribution moves a fraction of the way toward the
   target each cycle, and is only pushed to the data plane when it has moved past a
   deadband — except for ejections and sheds, which are safety actions and publish
   immediately.

**The request path** does no scoring. It evaluates policy, restricts the published
distribution to what policy permits, renormalises, and samples. Per-request work is
proportional to the candidate count, not to objectives × candidates, and two simultaneous
requests can never disagree about the weights.

> **Mean decision cost in the demo: 10–30µs** across ten candidate backends, with the
> authorisation cache absorbing most of the policy work. The dashboard reports it live,
> alongside the peak, so the claim is checkable rather than asserted.

### The carbon model, stated plainly

```
gCO2e per GB  =  energy_intensity (kWh/GB)  ×  PUE(provider)  ×  grid_intensity (gCO2e/kWh)
```

Published estimates for the network energy intensity of moving a gigabyte span roughly
**0.004 to 0.06 kWh** depending on methodology, vintage, and how much of the access
network is attributed to the transfer. Sluice defaults to **0.015** and puts that number
in its API and on its dashboard, because a carbon figure without its assumption attached
is a number nobody can check. Set `carbon.energyKwhPerGb` to whatever your organisation
reports against.

Grid intensity comes from a bundled zone dataset shaped by a diurnal model — solar
suppresses intensity around midday in proportion to how much of it a grid has built,
demand peaks in the early evening — or from live Electricity Maps readings when a token
is configured. Two providers in the same metro share a grid zone, which is why the
topology view shows AWS `us-east-1` and Azure `eastus` on the same PJM node: failing
between those clouds does nothing for emissions, and a carbon-aware router has to know it.

## The policy language

Zero trust means no request is authorised by where it came from. Policies are written in
a small expression language with four effects:

```
policy "pii-stays-in-eu" {
  description "GDPR: personal data belonging to EU subjects must not leave the EU."
  priority    100
  effect      constrain
  when        request.data_class == "pii" and subject.claims["residency"] == "eu"
  require     backend.jurisdiction == "EU"
  message     "GDPR residency: EU personal data may only egress to EU regions"
  tags        ["gdpr", "residency"]
}

policy "batch-traffic-favours-cost-and-carbon" {
  priority 210
  effect   prefer
  when     request.path startswith "/batch" or subject.service == "etl"
  prefer   { cost: 0.45, carbon: 0.40, latency: 0.05, reliability: 0.10 }
}
```

| Effect | What it does |
|---|---|
| `deny` | Refuses the request. Deny overrides allow, unconditionally. |
| `allow` | Authorises it. At least one must match — there is no implicit allow. |
| `constrain` | Prunes destinations via `require`, without touching authorisation. The request is fine; only some places are. |
| `prefer` | Reshapes the objective weights, so batch and checkout can optimise for different things under one router. |

Attributes are `subject.*`, `request.*`, `backend.*` and `time.*`; operators are the
usual comparisons plus `in`, `not in`, `matches`, `startswith`, `endswith`, `contains`;
builtins are `lower`, `upper`, `len`, `has`, `ip_in_cidr`, `split`.

Some deliberate choices:

- **`matches` is a glob, not a regex.** This evaluates on every request, and an
  operator-authored regular expression is an operator-authored denial of service.
- **Evaluation errors deny.** A policy that cannot be evaluated is a policy whose intent
  is unknown, and an unknown intent is not permission.
- **Misspelled attributes are errors, not `false`.** `subject.trustdomain` fails loudly
  rather than silently making the enclosing policy stop matching.
- **Access to optional data is lenient.** `subject.claims["absent"]` is null, not an error.

### Backtesting

The question before applying a policy edit is not "does it compile" but "what does it
break". Sluice replays retained decisions through both the live document and the
candidate and diffs the outcomes, so SLO shedding and circuit breakers are held constant
and the difference is your edit alone:

```console
$ sluicectl policy test configs/policies.sluice
replayed 800 retained decisions through both documents

  newly denied   0
  newly allowed  0
  pool narrowed  91
  pool widened   0
  unchanged      709

changed decisions:
  narrowed-pool  /api/v1/profile/preferences  identity/profile-api (eligible 4→1)
```

`sluicectl policy test` exits non-zero when traffic that flows today would be refused, so
it drops into CI as a gate.

## Deployment

### With Envoy

No custom filter to build, no WASM to ship, no xDS server to operate. Envoy asks Sluice,
Sluice answers `200` with `x-sluice-backend: <cluster>`, and the router forwards there via
`cluster_header`.

```bash
docker compose -f deploy/docker-compose.yml up --build
```

That brings up a real Envoy terminating **mutual TLS**, three synthetic regions on
separate hosts, the control plane, and a load generator driving four distinct workload
identities. It generates its own local CA on first run. Dashboard on `:8080`, proxied
traffic on `:10000`, Envoy admin on `:9901`.

Measured through that stack, same fleet, same instant:

| Route | Objectives | Where traffic went |
|---|---|---|
| `interactive` | 60ms SLO, latency 55% | 40 us-central1 · 19 us-east-1 · 1 france |
| `batch` | no SLO, cost 45% + carbon 40% | **55 francecentral** · 4 us-east-1 · 1 us-central1 |
| `payments` | reliability 50%, mTLS required | 21 us-central1 · 9 us-east-1 · 0 france |

Batch put 92% of its traffic on the region with 27% cheaper egress and a grid roughly
seven times cleaner, and paid 88ms for it. The interactive SLO kept that same region out.

Two things in that configuration are load-bearing and non-obvious:

- **A filter must invalidate the cached route after ext_authz runs.** Envoy resolves the
  route *before* the filter chain, so the header ext_authz is about to set does not exist
  yet. Without that invalidation every request fails with the `NC` flag while ext_authz
  reports success — see [`deploy/envoy/envoy.yaml`](deploy/envoy/envoy.yaml).
- **Identity cannot come from a header the caller sets.** Envoy's
  `forward_client_cert_details` defaults to `SANITIZE` and strips inbound
  `x-forwarded-client-cert` — correctly, or anyone could assert any SPIFFE ID. The demo
  therefore terminates real mTLS and lets Envoy *generate* that header from the
  certificate it verified.

> `failure_mode_allow` is set to `false`. An authorisation service that cannot be reached
> has not authorised anything — which also means control-plane availability *is*
> data-plane availability. See the [runbook](docs/RUNBOOK.md).

### On Kubernetes

The whole stack — three regional namespaces, an Envoy data plane and the control
plane — into a throwaway kind cluster:

```bash
make kind-up      # create the cluster, build and load the image, deploy
make k8s-verify   # drive real mTLS traffic through it and assert where it landed
```

Against a cluster you already have:

```bash
make k8s-deploy   # namespace, Secrets, then workloads, in that order
make k8s-verify
```

`k8s-verify` is the part worth reading. It does not check that pods are Ready —
every defect found while first deploying this passed that check, including a
deployment with no data plane at all and one that denied 100% of legitimate
traffic. It asserts behaviour: that an anonymous caller is refused at the TLS
handshake, that an untrusted identity gets a 403, that 60 authorised mutual-TLS
requests return 200, that they **landed on upstream pods** (counted from each
region's own request counter *and* corroborated by Envoy's `upstream_rq_200`),
and that traffic reached more than one region. CI runs the same script against a
real kind cluster on every push.

```
==> routing
  ok    60/60 authorised requests reached an upstream
        aws-us-east-1          served 115
        gcp-us-central1        served 28
        azure-francecentral    served 7
  ok    traffic distributed across 3 regions
  ok    Envoy counted 150 successful upstream responses
```

Manifests, applied in dependency order by filename:

| file | what it is |
| --- | --- |
| `10-regions.yaml` | three namespaces with a `checkout` Service each, standing in for AWS, GCP and Azure regions |
| `15-envoy.yaml` | the data plane — **generated** from `deploy/envoy/envoy.yaml` by `scripts/render-k8s-envoy.sh`, so one bootstrap serves both Compose and Kubernetes |
| `20-control-plane.yaml` | Sluice itself, plus PDB, NetworkPolicy and RBAC |
| `30-servicemonitor.yaml` | optional; needs the Prometheus Operator's CRDs |

Or with Helm:

```bash
helm install sluice deploy/helm/sluice --set api.token="$(make -s token)"
```

Nothing sensitive is committed. `make k8s-certs` generates the API token and a
SPIFFE CA into Secrets, issuing under trust domain **`cluster.local`** — the
SPIFFE convention in Kubernetes, and what the shipped policy recognises. Issuing
under any other domain deploys cleanly, reports every pod healthy, and then
refuses every request as an unrecognised trust domain. That is the system
working as designed; it is also why `k8s-verify` exists.

The control plane is stateless — the ledger and signal history are in-memory and
deliberately disposable. Readiness requires a *computed traffic distribution*,
not just a listening socket: before the first control loop finishes, the router
would deny everything for want of a plan, and a pod in that state must not
receive traffic.

Prometheus rules for availability, guardrails and signal freshness are in
[`deploy/prometheus/alerts.yaml`](deploy/prometheus/alerts.yaml).

### Without Envoy

`sluiced --proxy :9443` runs a native mTLS reverse proxy that terminates the connection,
extracts SPIFFE identity from the verified peer certificate, routes, and feeds observed
latency and byte counts back into the signals. It exists so the system can be evaluated
end to end as one process.

## Observability

`/metrics` exposes decision rates by route/verdict/cloud, per-backend traffic share,
price, carbon, latency and breaker state, plan churn and generation, SLO compliance,
signal age, and cost and carbon deltas as separate avoided/added counters — because
savings are signed, and a router that spends extra to hold an SLO should say so rather
than reporting a smaller positive number.

The compose stack brings up Prometheus and Grafana with a dashboard provisioned against
those series — **[http://localhost:3000/d/sluice-overview](http://localhost:3000/d/sluice-overview)**,
no login. Thirty panels across six rows: the fleet at a glance, where traffic is going,
*the trade being made* (cost and carbon avoided versus added, and the latency paid for
them), SLO and breaker health, the raw signals, and control-plane cost.

Alerting and recording rules for the same series are in
[`deploy/prometheus/alerts.yaml`](deploy/prometheus/alerts.yaml), and every alert has a
documented response in the [runbook](docs/RUNBOOK.md).

```bash
make dashboard-lint                                  # static: do the queries name real metrics?
python3 scripts/check-dashboard.py --live http://localhost:3000   # live: do they return data?
```

That second check exists because a panel querying a renamed metric does not fail — it
renders an empty graph, which is indistinguishable from "nothing is happening" and is
silent exactly when someone is depending on it. CI runs both.

The exposition format is implemented directly rather than pulled in as a dependency.
Sluice has **zero third-party Go modules**, which for a component sitting in the
authorisation path of every request is worth more than the convenience.

## What this is not

- **Not a load balancer.** Sluice decides *which region*; Envoy still decides which host
  in it, and does connection pooling, retries and outlier detection far better.
- **Not a billing system.** Egress prices come from published list tables and, where an
  API answers, live queries. Real invoices reflect committed-use discounts and private
  pricing that only the account owner can see. Put those in `pricing.overrides`.
- **Not a carbon accounting tool.** The emissions model is transparent and defensible but
  it is a model, and the energy-intensity literature is not settled. It is good enough to
  rank regions against each other, which is what routing needs; it is not an audited
  disclosure figure.
- **Not multi-master.** Replicas each compute their own distribution from their own
  measurements. They converge because they observe the same world, but they do not
  coordinate, and nothing here is a consensus system.

## Repository layout

```
cmd/sluiced          control plane: signals, policy, routing, API, dashboard
cmd/sluicectl        operator CLI — status, backends, explain, policy test, watch
cmd/sluice-upstream  synthetic origin for the container and Kubernetes demos
internal/model       domain types; imports nothing else
internal/signals     price, carbon, latency and health providers; P² quantiles; breakers
internal/policy      lexer, Pratt parser, evaluator, decision cache
internal/router      scoring, softmax allocation, SLO guardrail, damping, engine
internal/telemetry   Prometheus registry, decision ledger, rollups
internal/authz       Envoy ext_authz HTTP service
internal/proxy       native mTLS data plane
internal/sim         synthetic clouds, drifting signals, incident injection
web/                 embedded dashboard — no build step, no CDN, no framework
deploy/              Dockerfile, compose, Envoy, Kubernetes, Helm, Prometheus rules
```

## Development

```bash
go test ./...              # unit tests
go vet ./...
go run ./cmd/sluiced --demo
```

Go 1.26+. No code generation, no build step for the frontend, no external services
required for the full test suite.

## License

MIT.
