# Runbook

What to do when Sluice pages you. Each section maps to an alert in
[`deploy/prometheus/alerts.yaml`](../deploy/prometheus/alerts.yaml).

**The one thing to know first:** Envoy's `ext_authz` filter is configured with
`failure_mode_allow: false`. If the control plane is unreachable, the data plane
refuses traffic rather than admitting it unauthorised. That is the correct
trade for a zero-trust router, and it means *control-plane availability is
data-plane availability*. Treat `SluiceDown` as a full outage.

---

## Triage in one command

```bash
sluicectl status      # version, policy hash, uptime, how many plans pushed
sluicectl backends    # every region: share, price, carbon, p95, breaker state
sluicectl routes      # per-route distribution and SLO compliance
```

If `plans pushed` is 0, the control loop has never completed — go to
[SluiceNoTrafficPlan](#sluicenotrafficplan).

---

## SluiceDown

**Meaning:** the control plane is not answering. Because ext_authz fails
closed, every request through Envoy is being refused right now.

1. **Confirm the blast radius.** `sluice_decisions_total` will have stopped
   entirely; Envoy's own `envoy_http_ext_authz_error` will be climbing.
2. **Check readiness rather than liveness.** `/readyz` requires a computed
   traffic distribution. A pod that is up but never became ready has a
   configuration or connectivity problem, not a crash:
   ```bash
   kubectl -n sluice get pods
   kubectl -n sluice logs deploy/sluice --tail=100
   ```
3. **Common causes, in the order they actually happen:**
   - **Refused to start on an auth posture error.** The log line names the fix.
     A pod binds every interface, so it needs `SLUICE_API_TOKEN`; the Secret
     may be missing or renamed.
   - **Policy file failed to compile at startup.** Startup compilation is
     fatal by design — see [Policy rollback](#policy-rollback).
   - **All replicas evicted or unschedulable.**
4. **Emergency mitigation.** If Sluice cannot be restored quickly and traffic
   loss is worse than losing policy enforcement, set `failure_mode_allow: true`
   on the Envoy filter and redeploy the data plane. **This admits
   unauthenticated traffic.** Record the decision, set a timer, and revert as
   soon as the control plane is back.

---

## SluiceNoTrafficPlan

**Meaning:** the control loop has not produced a distribution for a route, so
every request to it is denied for want of a plan.

1. `sluicectl routes` — a route with generation 0 has never published.
2. Check the route's candidate pool. A route whose `backendIds` reference
   backends that are absent has nothing to allocate over. Configuration
   validation catches unknown IDs at startup, so this usually means the
   backends exist but are all disabled.
3. Check the probes are reaching the upstreams:
   ```bash
   kubectl -n sluice logs deploy/sluice | grep "probe failed"
   ```
   A network policy that blocks egress to the upstream ports will produce
   exactly this: healthy control plane, no usable backends.

---

## SluiceAllBackendsEjected

**Meaning:** every backend on a route is at zero weight. Policy authorised the
traffic; there was nowhere to send it. Callers are seeing 503, not 403 — the
API distinguishes these deliberately.

1. `sluicectl backends` and read the **breaker** column.
   - All `open` → this is an upstream incident, not a routing one. Sluice
     ejected them because they were failing. Fix the upstreams.
   - All `closed` but zero share → the SLO guardrail or a policy constraint
     removed them. Check the next two sections.
2. If a residency constraint has eliminated the pool — for example EU personal
   data with every EU region down — the correct fix is capacity, not policy.
   Loosening a residency rule to restore traffic is a compliance decision, not
   an availability one; escalate rather than deciding it at 3am.

---

## SluiceSLOBreached

**Meaning:** the projected blended p95 for a route exceeds its target, *after*
the guardrail already shed the slowest candidates.

This alert is not "routing is misbehaving" — it is "even the fastest remaining
region is too slow". It is a capacity or upstream signal.

1. `sluicectl routes` shows projected p95 against the SLO and which backends
   were shed.
2. Look at `sluice_backend_latency_p95_seconds` per region. If one region has
   degraded, the guardrail should already have removed it; if *all* have, the
   problem is upstream of Sluice.
3. **Do not raise the SLO to silence the alert.** The SLO is the constraint
   that stops cost optimisation from eating the user experience; raising it
   converts a visible latency problem into an invisible one.
4. If a specific region is the cause and you want it out immediately rather
   than waiting for the breaker, drain it — see [Region drain](#region-drain).

---

## SluiceCircuitOpen

**Meaning:** a backend has been ejected for failing.

Usually self-resolving: the breaker moves to half-open after its interval,
takes a 5% trickle, and closes after three consecutive successes. The interval
doubles on each failed recovery, capped at two minutes, so a persistently sick
region is probed less and less rather than hammered.

Escalate if:
- **Trips keep climbing** (`sluice_backend_circuit_state` oscillating). The
  region is flapping — half-healthy is worse than down, because it keeps
  earning its way back and failing again. Drain it explicitly.
- **Several regions in one cloud open together.** That is a provider incident,
  and the remaining clouds are now carrying all the traffic. Check the
  remaining capacity holds.

---

## SluicePlanChurnHigh

**Meaning:** the traffic distribution is moving more than a third of its mass
on a sustained basis. The data plane is being rewritten constantly and no
region's latency signal is stable enough to trust.

1. Find the oscillating signal. `sluice_backend_egress_usd_per_gb` and
   `sluice_backend_latency_p95_seconds` per region — one of them is swinging.
2. **Tuning, in order of preference:**
   - Raise `router.smoothing` toward 1.0 *slowly* — it is the fraction of each
     new target folded in per cycle, so lower is calmer. Try 0.2.
   - Raise `router.deadband` (default 0.04) so small movements are not pushed.
   - Raise `route.temperature` to spread traffic more evenly, which makes the
     allocation less sensitive to score differences.
3. If churn is caused by two backends being genuinely near-identical, that is
   the softmax working correctly and a higher deadband is the right answer.

---

## Policy rollback

The most likely bad change anyone makes. Policy is loaded three ways —
mounted file, `PUT /api/policy`, or the dashboard editor — and all three
compile before installing.

**A document that fails to compile is rejected and the running set keeps
serving.** You cannot disarm the policy engine with a typo. What you *can* do
is install a document that compiles and is wrong.

To roll back:

```bash
# What is live right now?
sluicectl status                       # policy hash
sluicectl policy get > /tmp/live.sluice

# Restore from source control and check what it would change first.
git show HEAD~1:configs/policies.sluice > /tmp/previous.sluice
sluicectl policy test /tmp/previous.sluice

# Apply.
sluicectl policy apply /tmp/previous.sluice
```

`sluicectl policy test` replays retained decisions through both the live and
candidate documents and reports what would change. It **exits non-zero when
traffic that flows today would be refused**, so it belongs in CI as a gate on
policy changes.

In Kubernetes the file is a ConfigMap and is polled, not watched, so a rollback
takes effect within a couple of seconds of `kubectl apply`:

```bash
kubectl -n sluice rollout undo deploy/sluice     # if the change shipped with the image
kubectl -n sluice edit configmap sluice-policy    # if it is only the document
```

**Verifying a rollback landed:** the policy hash in `sluicectl status` must
change. If it has not, the file did not reach the pod.

---

## Region drain

Taking a region out of rotation for maintenance, without waiting for a breaker.

**Preferred — configuration:** set `enabled: false` on the backend and reload.
The control loop drops it on the next tick, and because ejection bypasses the
damping deadband it takes effect immediately rather than decaying in.

**Faster — capacity:** set `capacityRps` low. The router caps its share and
redistributes the overflow, which bleeds traffic off gradually rather than
cutting it.

**Do not** delete the backend from configuration to drain it. That discards its
signal history, so when you add it back the router has no latency distribution
for it and has to relearn — during which the exploration floor will send it
real traffic.

Confirm the drain: `sluicectl backends` shows 0.0% share, and
`sluice_backend_traffic_share` for that backend goes to zero across every
route.

---

## SluiceStaleSignal

**Meaning:** a price or carbon quote is over two hours old. The router is still
making confident decisions against it.

1. The dashboard's **Signals → Provenance** table shows every quote's source
   and age. `sluicectl backends` shows the values themselves.
2. Sources fall back in order, so staleness means *every* source failed:
   - **Egress price:** live Azure Retail API → bundled list table. The bundled
     table never goes stale, so a stale price means the store stopped being
     written at all — check for a panic in the refresh loop.
   - **Grid carbon:** Electricity Maps → bundled zone dataset with a diurnal
     model. A missing or expired `SLUICE_ELECTRICITY_MAPS_TOKEN` degrades to
     the model silently *by design*; the provenance column is how you notice.
3. Stale signals degrade decision quality but do not stop traffic. This is an
   accuracy alert, not an availability one.

---

## SluiceDecisionLatencyHigh

**Meaning:** decisions are taking over 5ms at p99. This sits in the request
path, so it is latency every caller pays.

1. Check `sluice_policy_cache_hit_ratio`. Below 50% usually means a policy is
   reading a high-cardinality attribute — a unique request header, say — which
   makes every request a distinct cache key.
2. Check the policy document's size against the candidate count. Cost is
   roughly *constraints × backends*; fifty constraints over twelve regions is
   six hundred expression evaluations per uncached request.
3. `sluice_decision_duration_seconds` is a histogram; compare across routes to
   find which one is expensive.

---

## Someone is probing the admin API

`sluice_denials_total` and the `deniedApiRequests` counter in `/api/status`
count refused control-plane calls. The dashboard shows a chip when it is
non-zero.

Refusals are logged with peer address and user-agent — never with the presented
token, which is one typo away from being a valid one sitting in your logs.

```bash
kubectl -n sluice logs deploy/sluice | grep "control-plane API request refused"
```

If the peer is not something you recognise, the control-plane port is reachable
from somewhere it should not be. Check the NetworkPolicy.

---

## Restarting safely

The control plane is stateless. The decision ledger and signal history are
in-memory and deliberately disposable.

What a restart costs:
- **The ledger is lost.** Recent decisions are no longer explainable, and
  `sluicectl policy test` has no traffic to replay until it refills.
- **Signal history resets.** Latency distributions have to rewarm; the first
  few seconds route on probe data alone.
- **Readiness gates the rollout.** `/readyz` fails until a plan exists, so a
  rolling update with `maxUnavailable: 0` will not remove the old pod until the
  new one can actually route.

Because of that last point, an ordinary rolling restart is safe. Deleting all
replicas at once is not.
