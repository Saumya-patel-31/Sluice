# Design decisions

The choices that were not obvious, and what was given up for each.

---

## 1. Envoy `ext_authz` over HTTP, not gRPC or a custom filter

**Considered:** a WASM filter, a gRPC `ext_authz` service, or an xDS control plane pushing
cluster weights.

**Chose:** the HTTP variant of `ext_authz`, returning the destination as a response header
that Envoy's own route table matches on.

**Why:** the gRPC variant needs a protobuf toolchain, generated code and a dependency on
`go-control-plane` — in the authorisation path of every request. A WASM filter needs a
build pipeline and pins you to Envoy's ABI. An xDS server is the most "correct" answer
architecturally but is a large amount of machinery to operate, and it pushes *weights*
rather than *decisions*, which means per-request policy has to live somewhere else anyway.

The HTTP variant is one filter block against a stock Envoy, works with any version that
supports `ext_authz`, and keeps the whole project at zero third-party Go modules.

**Given up:** a round trip per request instead of an in-process filter. The decision itself
costs 10–30µs and the loopback call dominates that; for a mesh where Envoy and Sluice are
co-located it is acceptable. A latency-critical deployment would want the gRPC variant
with connection reuse, which is a drop-in replacement for the same engine.

**Load-bearing detail:** Envoy clears its route cache when `ext_authz` appends upstream
headers. Without that the header would arrive upstream but the route would already have
been chosen, and the whole scheme silently does nothing.

---

## 2. A purpose-built policy language, not CEL, Rego or Cedar

**Considered:** embedding CEL (`cel-go`), OPA/Rego, or Cedar.

**Chose:** a small expression language with a hand-written lexer, Pratt parser and
tree-walking evaluator.

**Why:** none of the three has the effect that matters most here. Sluice needs a policy to
say "this request is fine, but only some *destinations* are" — a `constrain` effect that
prunes the candidate set — and to reshape routing objectives per traffic class. Those are
routing concepts, not authorisation concepts, and bolting them onto a general-purpose
authorisation engine means encoding them in return values and interpreting them outside
the policy anyway.

Secondary reasons: `cel-go` and OPA are large dependency trees, and the whole language
here is ~900 lines including the parser.

**Given up:** ecosystem. No existing Rego library, no Cedar tooling, and operators have to
learn one more syntax. Mitigated by keeping the surface tiny — four effects, one page of
operators — and by shipping a backtester so nobody has to reason about the semantics
abstractly.

**Deliberate restriction:** `matches` is a glob, not a regular expression. This evaluates
on every request against operator-authored patterns, and a catastrophically backtracking
regex on the request path is a denial of service with extra steps.

---

## 3. Softmax allocation instead of picking the best backend

**Considered:** argmax with hysteresis; weighted round-robin over a hand-tuned table.

**Chose:** softmax over scores with a temperature, plus damping and a deadband.

**Why:** argmax oscillates between near-ties, starves losing backends of the traffic that
keeps their latency signals honest, and converts a small measurement error into a total
traffic shift. Softmax degrades continuously and the temperature is a single interpretable
dial from "spread evenly" to "winner takes all".

**Given up:** the simplicity of an obviously-correct choice. A softmax distribution is
harder to explain to an operator than "it picked the cheapest one", which is precisely why
the decision ledger records the full derivation.

---

## 4. Two loops, and no scoring on the request path

**Chose:** a 1 Hz control loop computes the traffic distribution; the request path applies
policy and samples from it.

**Why:** per-request work becomes O(candidates) rather than O(objectives × candidates),
and two simultaneous requests cannot disagree about the weights because they read the same
immutable state pointer. It also mirrors how the data plane would work under xDS, so
moving to a push model later does not change the shape of the system.

**Given up:** the distribution is up to one control interval stale. That is the correct
trade — the signals it is derived from have far more latency than that — but it does mean
a backend failing between ticks is caught by Envoy's outlier detection first, which is why
the shipped Envoy config keeps its own conservative ejection settings.

---

## 5. Objective profiles are discovered, not enumerated

**Problem:** a `prefer` policy reshapes objectives per request, but the plan is computed
per control loop. Recording the adjusted objectives on a decision whose scores came from
the route's objectives produces an explainability trace whose arithmetic does not add up —
and, worse, means `prefer` has no actual effect on routing.

**Considered:** enumerate every reachable objective vector from the policy document.

**Chose:** observe vectors as they appear on the request path and build a plan for each on
the next control loop.

**Why:** several `prefer` policies can match one request and their overrides compose, so
the reachable set is combinatorial in the number of rules. Observation bounds the set by
actual usage instead.

**Given up:** a profile seen for the first time uses the route's base plan for up to one
control interval. Bounded, self-correcting, and reported honestly — the decision always
records the objectives its scores were actually computed with.

---

## 6. Savings measured against a latency-only counterfactual

**Considered:** comparing against the most expensive eligible backend (flattering), or
against a fixed baseline region (arbitrary).

**Chose:** compare against what a conventional load balancer — optimising latency alone,
subject to the same policy constraints — would have chosen for that same request.

**Why:** it is the only comparison that answers the question anyone actually has, which is
"what did this system buy me over the one I already have". Where the latency winner is
also the cheapest, the reported saving is correctly zero.

**Consequence:** savings are **signed**. When the router deliberately spends more to hold
an SLO or to cut emissions, the number goes negative and the dashboard shows it. The
Prometheus metrics split into separate `avoided` and `added` counters rather than reporting
a netted figure, because a netted counter cannot be used with `rate()` and hides the
trade being made.

---

## 7. The carbon model is configurable and stated on the page

Published estimates for the network energy intensity of moving a gigabyte span roughly
**0.004 to 0.06 kWh** depending on methodology, vintage, and how much of the access network
is attributed to the transfer. Any single figure is a choice.

Rather than bury one, Sluice takes it as configuration, defaults to a mid-range 0.015, and
surfaces the value in `/api/status` and on the dashboard. A carbon number without its
assumption attached is a number nobody can check.

The same reasoning drives the grid-zone mapping: AWS `us-east-1` and Azure `eastus` are
both on PJM, and a carbon-aware router that did not know that would report a fictional
saving for "failing over to the other cloud" in Northern Virginia.

---

## 8. Zero third-party Go modules

**Given up:** `prometheus/client_golang`, a chart library, a YAML parser, `cel-go`, a
router, a logging library.

**Why:** this component sits in the authorisation path of every request, so its supply
chain is part of its threat model. The exposition format is a stable documented contract;
the charts are five shapes; configuration uses JSONC (JSON with comments stripped by a
40-line preprocessor) rather than taking on a YAML dependency.

**Cost:** roughly 1,200 lines that would otherwise be `import`. Each of those pieces is
also less capable than the library it replaces — the metrics registry has no exemplars or
native histograms, and the chart code draws exactly the five things the dashboard needs.
That is the trade, taken knowingly.

---

## 9. In-memory ledger, no persistence

The decision ledger is a fixed-capacity ring buffer. Nothing is written to disk.

**Why:** the ledger's job is to answer "why did that happen" minutes to hours after the
fact, and to give the policy backtester real traffic to replay. Both are satisfied by a
bounded recent window. Persisting it would make the control plane stateful, which would
make it a thing that needs backups, migrations and capacity planning — for data whose
value decays in hours.

Long-term analysis belongs in the metrics pipeline, which is why the ledger's aggregates
are also exported to Prometheus.

**Given up:** decisions age out. The API says so explicitly when asked for one that has,
rather than returning an empty record.
