#!/bin/sh
# Assert that a Kubernetes deployment of Sluice is actually routing traffic,
# not merely running.
#
#   ./scripts/verify-k8s.sh
#
# "The pods are Ready" is a weaker claim than it looks. Readiness only proves
# the control loop produced a traffic plan; it stayed green through a
# deployment that denied every legitimate request, because the certificates
# were issued under a trust domain the policy did not recognise. So this drives
# real mutual-TLS traffic through the data plane from inside the cluster and
# checks where it landed.
#
# Exits non-zero on the first failed assertion, so it works as a gate.

set -eu

NS="${NS:-sluice}"
PF_PORT="${PF_PORT:-18080}"
REGIONS="aws-us-east-1 gcp-us-central1 azure-francecentral"
PROBE=sluice-verify

fail() { printf '  FAIL  %s\n' "$1" >&2; exit 1; }
ok()   { printf '  ok    %s\n' "$1"; }

cleanup() {
    [ -n "${PF:-}" ] && kill "$PF" 2>/dev/null || true
    kubectl -n "$NS" delete pod "$PROBE" --ignore-not-found \
        --grace-period=0 --force >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "==> workloads"
kubectl -n "$NS" rollout status deploy/sluice --timeout=120s >/dev/null \
    || fail "control plane did not become available"
ok "control plane rolled out"

kubectl -n "$NS" rollout status deploy/envoy --timeout=120s >/dev/null \
    || fail "data plane did not become available"
ok "data plane rolled out"

for region in $REGIONS; do
    kubectl -n "$region" rollout status deploy/checkout --timeout=120s >/dev/null \
        || fail "$region did not become available"
done
ok "all three regions rolled out"

# Every replica, not just the deployment's minimum.
ready=$(kubectl -n "$NS" get deploy sluice -o jsonpath='{.status.readyReplicas}')
want=$(kubectl -n "$NS" get deploy sluice -o jsonpath='{.spec.replicas}')
[ "${ready:-0}" = "$want" ] || fail "only ${ready:-0}/$want control-plane replicas ready"
ok "$ready/$want control-plane replicas ready"

echo "==> identity"
kubectl -n "$NS" get secret sluice-client-certs >/dev/null 2>&1 \
    || fail "sluice-client-certs missing; run: make k8s-certs"

# The probe has to satisfy the namespace's restricted Pod Security admission,
# the same as everything else here.
kubectl -n "$NS" delete pod "$PROBE" --ignore-not-found --grace-period=0 --force >/dev/null 2>&1 || true
kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $PROBE
  namespace: $NS
  labels: { app.kubernetes.io/part-of: sluice }
spec:
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    runAsUser: 65532
    seccompProfile: { type: RuntimeDefault }
  containers:
    - name: curl
      image: curlimages/curl:latest
      command: ["sleep", "300"]
      securityContext:
        allowPrivilegeEscalation: false
        capabilities: { drop: ["ALL"] }
      volumeMounts: [{ name: certs, mountPath: /certs, readOnly: true }]
  volumes:
    - name: certs
      secret: { secretName: sluice-client-certs, defaultMode: 0444 }
EOF
kubectl -n "$NS" wait --for=condition=Ready "pod/$PROBE" --timeout=120s >/dev/null \
    || fail "verification probe never became ready"

probe() { kubectl -n "$NS" exec "$PROBE" -- sh -c "$1"; }

# A caller with no certificate must not get as far as a policy decision: the
# TLS handshake itself is the first gate. curl reports 000 when it fails.
code=$(probe 'curl -s -o /dev/null -w "%{http_code}" --max-time 10 \
    --cacert /certs/ca.crt https://envoy:10000/api/v1/feed || true')
[ "$code" = "000" ] || fail "a request with no client certificate got $code, want a refused handshake"
ok "an anonymous caller is refused at the TLS handshake"

# A certificate that chains to a CA Envoy does not trust, or an identity from a
# trust domain the policy does not recognise, reaches the policy and is denied.
code=$(probe 'curl -s -o /dev/null -w "%{http_code}" --max-time 10 \
    --cacert /certs/ca.crt --cert /certs/intruder.crt --key /certs/intruder.key \
    https://envoy:10000/api/v1/feed || true')
[ "$code" = "403" ] || fail "an untrusted identity got $code, want 403"
ok "an untrusted identity is denied"
echo "==> reachability"
kubectl -n "$NS" port-forward svc/sluice "$PF_PORT:8080" >/dev/null 2>&1 &
PF=$!
i=0
while [ "$i" -lt 40 ]; do
    curl -sf "http://127.0.0.1:$PF_PORT/readyz" >/dev/null 2>&1 && break
    i=$((i + 1)); sleep 1
done
[ "$i" -lt 40 ] || fail "the API never answered through the Service"
ok "/readyz answering through the Service"

api() { curl -sf "http://127.0.0.1:$PF_PORT$1"; }

healthy=$(api /api/overview | tr ',' '\n' | grep -o '"healthyBackends":[0-9]*' | cut -d: -f2 | head -1)
[ "${healthy:-0}" -ge 3 ] || fail "only ${healthy:-0} healthy backends, want 3"
ok "$healthy backends healthy and probed through cluster DNS"

echo "==> routing"

# Whether the router distributes load is a property of the plan it computed,
# not of where a handful of random draws happened to land. Asserting on the
# sample was flaky by construction: the exploration floor is 0.02, so a region
# sitting on the floor receives none of 60 requests about 30% of the time, and
# CI failed on exactly that. The plan is deterministic, and it is also the
# stronger claim — a router that computed a single-backend distribution is
# broken even when a lucky sample spreads across three.
spread=$(api /api/routes | python3 -c '
import json, sys
routes = json.load(sys.stdin)["routes"]
best = 0
for r in routes:
    nonzero = sum(1 for w in r.get("weights", {}).values() if w > 0)
    best = max(best, nonzero)
    print("        %-14s %s" % (r["route"]["id"],
          "  ".join("%s=%.3f" % (k, v) for k, v in sorted(r.get("weights", {}).items()))),
          file=sys.stderr)
print(best)
')
[ "${spread:-0}" -ge 2 ] \
    || fail "the computed plan puts weight on ${spread:-0} backend(s); the router is not distributing load"
ok "the computed traffic plan spans $spread backends"

served() {
    total=0
    for pod in $(kubectl -n "$1" get pods -l app.kubernetes.io/name=checkout -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
        n=$(kubectl -n "$1" exec "$pod" -- wget -qO- http://127.0.0.1:8080/stats 2>/dev/null \
            | grep -o '"served":[0-9]*' | cut -d: -f2)
        total=$((total + ${n:-0}))
    done
    echo "$total"
}

codes=$(probe 'for i in $(seq 1 60); do
    curl -s -o /dev/null -w "%{http_code}\n" --max-time 10 \
      --cacert /certs/ca.crt --cert /certs/feed.crt --key /certs/feed.key \
      https://envoy:10000/api/v1/feed
done')
allowed=$(echo "$codes" | grep -c '^200$' || true)
[ "$allowed" -ge 55 ] || fail "only $allowed/60 authorised requests succeeded: $(echo "$codes" | sort | uniq -c | tr '\n' ' ')"
ok "$allowed/60 authorised requests reached an upstream"

# Zero here is the exact symptom of the route-cache bug: every decision looks
# healthy and no traffic ever arrives.
reached=0
for region in $REGIONS; do
    n=$(served "$region")
    printf '        %-22s served %s\n' "$region" "$n"
    reached=$((reached + n))
done
[ "$reached" -gt 0 ] || fail "no region served any request"
ok "$reached requests landed on upstreams"

echo "==> observability"
api /metrics | grep -q '^sluice_decisions_total' || fail "/metrics has no decision series"

ok "/metrics exporting"
# Envoy's own view has to agree that it forwarded, not merely that it answered
# — an upstream 200 counted by the proxy is independent evidence from the
# upstream's own request counter.
#
# Asked per pod rather than through the Service: the counters are per-process,
# so a load-balanced query reads one replica's while the traffic went to the
# other. The probe pod does the asking because the Envoy image ships no curl.
envoy_rq=0
for ip in $(kubectl -n "$NS" get pods -l app.kubernetes.io/name=envoy \
              -o jsonpath='{.items[*].status.podIP}'); do
    n=$(probe "curl -s --max-time 10 http://$ip:9901/stats 2>/dev/null" \
        | grep -E '^cluster\.[a-z0-9-]+\.upstream_rq_200:' \
        | awk -F': ' '{s += $2} END {print s + 0}')
    envoy_rq=$((envoy_rq + ${n:-0}))
done
[ "$envoy_rq" -gt 0 ] || fail "Envoy recorded no successful upstream responses"
ok "Envoy counted $envoy_rq successful upstream responses"

echo
echo "Kubernetes deployment verified."
