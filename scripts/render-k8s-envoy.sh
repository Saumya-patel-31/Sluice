#!/bin/sh
# Generate deploy/k8s/15-envoy.yaml from the canonical Envoy bootstrap.
#
#   ./scripts/render-k8s-envoy.sh
#
# The Kubernetes data plane differs from the Compose one in exactly four
# endpoint addresses — Compose container names become cluster-DNS Service
# names. Everything else, including the route-cache invalidation that the whole
# integration hangs on, is identical.
#
# Keeping a second hand-maintained copy of a 327-line bootstrap would mean a
# fix applied to one and not the other, and a bootstrap that fails to load
# takes the entire data plane with it. So there is one source file and this
# renders it. `make envoy-render-check` fails if the checked-in output has
# drifted, which is what stops the two from silently diverging.

set -eu

SRC="deploy/envoy/envoy.yaml"
OUT="${1:-deploy/k8s/15-envoy.yaml}"

[ -f "$SRC" ] || { echo "render-k8s-envoy: $SRC not found (run from the repo root)" >&2; exit 1; }

# Indent the bootstrap for embedding in a ConfigMap literal block.
bootstrap=$(sed \
    -e 's|address: sluiced, port_value: 8081|address: sluice.sluice.svc.cluster.local, port_value: 8081|' \
    -e 's|address: upstream-aws, port_value: 8080|address: checkout.aws-us-east-1.svc.cluster.local, port_value: 8080|' \
    -e 's|address: upstream-gcp, port_value: 8080|address: checkout.gcp-us-central1.svc.cluster.local, port_value: 8080|' \
    -e 's|address: upstream-azure, port_value: 8080|address: checkout.azure-francecentral.svc.cluster.local, port_value: 8080|' \
    "$SRC" | sed 's/^/    /')

# A rewrite that silently matched nothing would produce a data plane pointing
# at Compose hostnames that do not resolve in a cluster, and the symptom would
# be 503s long after this script reported success.
for want in sluice.sluice.svc checkout.aws-us-east-1.svc checkout.gcp-us-central1.svc checkout.azure-francecentral.svc; do
    echo "$bootstrap" | grep -q "$want" || {
        echo "render-k8s-envoy: rewrite for '$want' matched nothing; $SRC has changed shape" >&2
        exit 1
    }
done
if echo "$bootstrap" | grep -qE 'address: (sluiced|upstream-)'; then
    echo "render-k8s-envoy: a Compose hostname survived the rewrite" >&2
    exit 1
fi

cat > "$OUT" <<EOF
# Envoy data plane — GENERATED, DO NOT EDIT.
#
#   source:    $SRC
#   regenerate: ./scripts/render-k8s-envoy.sh
#
# Edit the source bootstrap and re-render. \`make envoy-render-check\` fails if
# this file has drifted from it.
#
# The mTLS material comes from a Secret rather than this manifest: a private key
# committed to git is a private key in everyone's clone. \`make k8s-deploy\`
# generates it with scripts/gen-certs.sh.
apiVersion: v1
kind: ConfigMap
metadata:
  name: envoy-config
  namespace: sluice
  labels:
    app.kubernetes.io/name: envoy
    app.kubernetes.io/part-of: sluice
data:
  envoy.yaml: |
$bootstrap
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: envoy
  namespace: sluice
  labels:
    app.kubernetes.io/name: envoy
    app.kubernetes.io/part-of: sluice
spec:
  replicas: 2
  selector:
    matchLabels:
      app.kubernetes.io/name: envoy
  template:
    metadata:
      labels:
        app.kubernetes.io/name: envoy
        app.kubernetes.io/part-of: sluice
      annotations:
        prometheus.io/scrape: "true"
        prometheus.io/port: "9901"
        prometheus.io/path: /stats/prometheus
        # Roll the pods when the bootstrap changes. Without this a ConfigMap
        # edit leaves every replica running the previous config indefinitely,
        # because a projected volume update does not restart Envoy.
        checksum/config: "CONFIGSUM"
    spec:
      securityContext:
        runAsNonRoot: true
        runAsUser: 101
        runAsGroup: 101
        fsGroup: 101
        seccompProfile: { type: RuntimeDefault }
      containers:
        - name: envoy
          image: envoyproxy/envoy:v1.32-latest
          args:
            - --config-path
            - /etc/envoy/envoy.yaml
            - --log-level
            - warn
          ports:
            - { name: https, containerPort: 10000, protocol: TCP }
            - { name: admin, containerPort: 9901, protocol: TCP }
          # /ready is Envoy's own listener-level readiness. It goes green only
          # once the listeners are actually serving, which is the property this
          # probe is meant to assert.
          readinessProbe:
            httpGet: { path: /ready, port: admin }
            initialDelaySeconds: 2
            periodSeconds: 5
          livenessProbe:
            httpGet: { path: /ready, port: admin }
            initialDelaySeconds: 10
            periodSeconds: 15
          resources:
            requests: { cpu: 100m, memory: 96Mi }
            limits:   { memory: 512Mi }
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: { drop: ["ALL"] }
          volumeMounts:
            - { name: config, mountPath: /etc/envoy,        readOnly: true }
            - { name: certs,  mountPath: /etc/sluice/certs, readOnly: true }
            - { name: tmp,    mountPath: /tmp }
      volumes:
        - name: config
          configMap: { name: envoy-config }
        - name: certs
          secret:
            secretName: sluice-certs
            # Envoy runs as uid 101 and cannot read a 0600 key. This is the
            # same permission problem that broke the CI validation job.
            defaultMode: 0444
        - name: tmp
          emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: envoy
  namespace: sluice
  labels:
    app.kubernetes.io/name: envoy
    app.kubernetes.io/part-of: sluice
spec:
  selector:
    app.kubernetes.io/name: envoy
  ports:
    - { name: https, port: 10000, targetPort: https, protocol: TCP }
    - { name: admin, port: 9901,  targetPort: admin, protocol: TCP }
EOF

# Stamp the bootstrap's own hash so a config change rolls the pods.
sum=$(echo "$bootstrap" | sha256sum 2>/dev/null | cut -c1-16 \
      || echo "$bootstrap" | shasum -a 256 | cut -c1-16)
sed -i.bak "s/CONFIGSUM/$sum/" "$OUT" && rm -f "$OUT.bak"

echo "rendered $OUT (bootstrap $sum)"

# ---------------------------------------------------------------------------
# Helm
# ---------------------------------------------------------------------------
#
# The chart needs the same bootstrap with two things made dynamic: the control
# plane's address (release name and namespace are not known until install) and
# the upstream clusters (which follow .Values.backends, so a chart that adds a
# region gets an Envoy cluster for it rather than a route to nowhere).
#
# So the chart gets everything up to and including the sluice_authz cluster as
# a file rendered through `tpl`, and generates the region clusters itself.
# Splitting here rather than at `clusters:` keeps the keepalive and health-check
# rationale on the authz cluster where it was written.

HELM_FILE="deploy/helm/sluice/files/envoy-bootstrap.yaml"

if [ -d "deploy/helm/sluice" ]; then
    mkdir -p "$(dirname "$HELM_FILE")"

    # Everything before the upstream regions, with the authz endpoint templated.
    split_line=$(grep -n '^    # Upstream regions\.' "$SRC" | cut -d: -f1)
    [ -n "$split_line" ] || {
        echo "render-k8s-envoy: cannot find the upstream-regions marker in $SRC" >&2
        exit 1
    }

    {
        printf '%s\n' \
            '# GENERATED from deploy/envoy/envoy.yaml — DO NOT EDIT.' \
            '# Regenerate with ./scripts/render-k8s-envoy.sh' \
            '#' \
            '# Rendered through `tpl`, so Helm expressions below are evaluated at' \
            '# install time. The upstream clusters are appended by the template that' \
            '# includes this file, from .Values.backends.'
        sed -n "1,$((split_line - 1))p" "$SRC" | sed \
            -e 's|address: sluiced, port_value: 8081|address: {{ include "sluice.fullname" . }}.{{ .Release.Namespace }}.svc.cluster.local, port_value: {{ .Values.service.authzPort }}|'
    } > "$HELM_FILE"

    grep -q 'include "sluice.fullname"' "$HELM_FILE" || {
        echo "render-k8s-envoy: the Helm authz rewrite matched nothing" >&2
        exit 1
    }
    grep -q 'address: sluiced' "$HELM_FILE" && {
        echo "render-k8s-envoy: a Compose hostname survived into the Helm bootstrap" >&2
        exit 1
    }

    echo "rendered $HELM_FILE"
fi
