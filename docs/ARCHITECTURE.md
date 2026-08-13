# Architecture

How Sluice is put together, and why each piece is the shape it is.

## The two loops

The single most important structural decision is that routing runs in two loops
operating at different frequencies against different data.

```
  ┌─── CONTROL LOOP (1 Hz) ───────────────────────────────────────────────┐
  │                                                                        │
  │  signals.Store.Snapshot()      one coherent read of every signal       │
  │           ↓                                                            │
  │  ScoreCandidates()             min-max normalise, weight, score        │
  │           ↓                                                            │
  │  softmaxWeights()              scores → a traffic distribution         │
  │           ↓                                                            │
  │  SLO guard → capacity caps → exploration floor                         │
  │           ↓                                                            │
  │  damp against the previous cycle, publish past a deadband              │
  │           ↓                                                            │
  │  atomic swap of the whole state pointer                                │
  └────────────────────────────────────────────────────────────────────────┘

  ┌─── REQUEST PATH (per request, 10–30µs) ───────────────────────────────┐
  │                                                                        │
  │  policy.Evaluate()             cached; deny / allow / prune / reshape  │
  │           ↓                                                            │
  │  select the plan matching the resolved objectives                      │
  │           ↓                                                            │
  │  restrict the published distribution to what policy permits            │
  │           ↓                                                            │
  │  renormalise, sample, record                                           │
  └────────────────────────────────────────────────────────────────────────┘
```

The request path does **no scoring**. Normalisation and the softmax already ran against a
coherent snapshot; the request only applies policy and samples from the result. That buys
three properties:

1. **Per-request work is O(candidates)**, not O(objectives × candidates).
2. **Two simultaneous requests cannot disagree** about the weights, because they read the
   same immutable state pointer.
3. **A signal update cannot be observed half-applied.** The control loop builds an entirely
   new `state` and swaps the pointer; nothing mutates in place.

## Package graph

```
model  ←  signals  ←  router  ←  telemetry
  ↑         ↑          ↑            ↑
  └── policy ┘         └──── app ───┴─── api ── web
                       ↑      ↑
                identity   sim, authz, proxy, config
```

`model` imports nothing. `policy` and `signals` are independent of each other. `router`
depends on both but knows nothing about HTTP, metrics or the dashboard — it defines a
one-method `DecisionSink` interface and lets the app layer wire the ledger and the metrics
collector into it.

## Why these choices

### Softmax rather than argmax

Argmax picks the highest-scoring backend and sends it everything. That is wrong three ways:

- **It oscillates.** Two backends within measurement noise of each other trade 100% of the
  traffic back and forth every cycle.
- **It starves its own inputs.** A backend at zero traffic produces no latency or error
  samples, so the only evidence about it is synthetic — and synthetic probes exercise a
  different code path than real requests.
- **It amplifies error.** A 1% error in one measurement becomes a 100% traffic shift.

A softmax over scores degrades gracefully: near-ties split, a clear winner still takes
nearly everything, and the temperature is the operator's dial between the two.

### Damping and a deadband

Weights move a fraction of the way toward the target each cycle, and are only published
once they have moved past a threshold. Without this the control plane rewrites the data
plane's configuration every second in response to noise.

Two things deliberately bypass the deadband: **ejections** and **SLO sheds**. Those are
safety actions, and a backend that just started failing must stop receiving traffic now,
not in four seconds. The damping code makes that explicit — ineligible backends have their
smoothed weight forced to zero rather than decayed.

### The SLO guardrail

Cost and carbon optimisation is only legitimate inside the latency budget. When the
projected blend would breach the route's p95 target, the router drops the slowest
candidate and re-solves, repeating until it fits. Each round removes exactly one backend,
so it terminates, and it degrades in the right direction: savings are given up before the
SLO is.

The projection is the traffic-weighted mean of per-backend p95s. That is an approximation
— the true p95 of a mixture needs each backend's full distribution — and the plan reports
`worstP95` alongside it as the pessimistic bound rather than pretending otherwise.

### Objective profiles

A `prefer` policy reshapes the objectives for one class of traffic, so batch and checkout
optimise for different things under one router. That means a *different traffic
distribution*, not a different number recorded against the same one.

The set of reachable objective vectors is combinatorial in the number of `prefer` rules
(several can match one request and their overrides compose), so enumerating them from the
document is not tractable. Instead the engine observes vectors as they appear on the
request path and builds a plan for each on the next control loop. A profile seen for the
first time uses the route's base plan for one interval; unused profiles expire after ten
minutes.

The invariant this preserves: **a decision's recorded objectives are always the ones its
candidate scores were computed with**, so the explainability trace is arithmetic that
actually reproduces.

### Policy evaluates before routing, and prunes

Policy does not merely say yes or no. A `constrain` effect removes candidate backends
whose attributes fail a `require` clause — the request is fine, but only some destinations
are. That is how data residency is expressed, and it is why authorisation has to run
before allocation rather than as a gate after it.

Three failure modes are handled explicitly:

| Condition | Verdict | Why it is distinct |
|---|---|---|
| A policy denied it | `deny` | An authorisation failure. 403. |
| No policy allowed it | `deny` | Default-deny. There is no implicit allow. |
| Allowed, but constraints left nothing | `no_capacity` | An availability failure. 503, and it should page someone. |

### Fail-closed everywhere

- A policy expression that errors **denies**. A policy whose intent is unknown is not
  permission.
- A constraint that errors **prunes** the backend rather than leaving it eligible.
- Envoy's `failure_mode_allow` is `false`. An authorisation service that cannot be reached
  has not authorised anything.
- `/readyz` requires a computed traffic distribution, not just a listening socket.

### Zero dependencies

Sluice imports no third-party Go modules. The Prometheus exposition format, the policy
language, the quantile estimator and the dashboard are all implemented directly.

For a component that sits in the authorisation path of every request, the supply chain is
part of the threat model, and the exposition format is a stable documented contract worth
300 lines to implement. The dashboard has no build step for the same reason: no npm tree,
no bundler, no CDN, and a content security policy that forbids every external origin.

## Signals

| Signal | Source | Freshness |
|---|---|---|
| Egress price | Azure Retail Prices API (public, no auth) → bundled list tables → per-backend overrides | 15 min |
| Grid carbon | Electricity Maps (token) → bundled zone dataset shaped by a diurnal model | 1 min |
| Latency p95 | Active probes + real traffic, P-square estimator over a sliding window | continuous |
| Error rate | Real traffic and probes, time-decayed EWMA | continuous |

Every value carries its **source and age**. A routing decision made against a six-hour-old
price list is a different kind of claim than one made against a live API response, and the
dashboard's provenance table exists so an operator can tell those apart. `sluice_signal_age_seconds`
alerts on it.

### The P-square estimator

Latency p95 is tracked with Jain & Chlamtac's P-square algorithm: O(1) memory and O(1)
time per observation, no sample retention. Sluice probes every backend continuously and
reads the distribution on the hot path of every control loop; storing samples to sort
later would cost memory proportional to probe rate × backend count. P-square costs five
float64s per backend regardless, and its accuracy on a heavy-tailed latency distribution
is within a few percent — which is well inside the noise the router is reacting to anyway.

A plain P-square estimator weights an hour-old observation exactly as heavily as a
one-second-old one, so two are rotated on a sliding window. That bounds how long stale
data can influence the answer while always leaving a warmed estimator to read from.

## Data plane

Two implementations, one decision engine.

**Envoy** (intended for production) calls the `ext_authz` HTTP service. Sluice answers 200
with `x-sluice-backend: <id>`; Envoy appends the header, clears its route cache, and
re-matches against a header-keyed route table. No custom filter, no WASM, no xDS server.

The HTTP variant of `ext_authz` is used rather than gRPC specifically to avoid a protobuf
toolchain and generated code in the authorisation path.

**The native proxy** (`--proxy`) terminates mTLS itself, extracts SPIFFE identity from the
verified peer certificate, routes, and feeds observed latency and bytes back into the
signals. It exists so the whole system can be run and evaluated as one process.

Identity is only ever derived from verified material: `tls.ConnectionState.VerifiedChains`
(not `PeerCertificates`, which is merely what the client offered), or an
`X-Forwarded-Client-Cert` header from a hop that is trusted to set it and strip inbound
copies.

## What is deliberately absent

- **No consensus.** Replicas each compute their own distribution from their own
  measurements. They converge because they observe the same world; they do not coordinate.
- **No persistence.** The ledger and signal history are in-memory and disposable. A
  restarted pod rebuilds its distribution from live measurements within a few ticks.
- **No host-level load balancing.** Sluice picks the region; Envoy picks the host in it,
  and does connection pooling, retries and outlier detection far better than a
  reimplementation would.
