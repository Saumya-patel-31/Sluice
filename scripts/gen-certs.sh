#!/bin/sh
# Generate a local CA and SPIFFE workload certificates for the mTLS demo.
#
#   ./scripts/gen-certs.sh
#   sluiced --demo --proxy :9443 \
#     --config <(...)               # or set the TLS paths in your config
#
# Sluice derives identity from the URI SAN of a *verified* client certificate,
# which means you cannot exercise that path without a CA and certificates that
# actually chain to it. This script produces both, so the identity code can be
# run rather than only read.
#
# These are for local use. They are self-signed, valid for a year, and the keys
# are written unencrypted to ./certs — which .gitignore excludes.

set -eu

# Git Bash / MSYS rewrites any argument that looks like a POSIX path into a
# Windows one, which turns `-subj /CN=sluice-demo-ca` into
# `-subj C:/Program Files/Git/CN=sluice-demo-ca` and makes openssl reject the
# subject. Nothing in this script wants that translation.
MSYS_NO_PATHCONV=1
MSYS2_ARG_CONV_EXCL='*'
export MSYS_NO_PATHCONV MSYS2_ARG_CONV_EXCL

# openssl writes its progress dots to stderr, so the calls below discard it —
# which also discards the error message when one fails. Route it through a log
# instead and print it if the script dies, or a failure looks like a script
# that silently produced no certificates.
# Absolute: the script cd's into the output directory below, and a relative
# log would be created there and shipped with the certificates.
ERRLOG=$(mktemp 2>/dev/null) || ERRLOG="$PWD/.gen-certs.err"
report_failure() {
    status=$?
    if [ "$status" -ne 0 ] && [ -s "$ERRLOG" ]; then
        echo "gen-certs: openssl failed:" >&2
        tail -5 "$ERRLOG" >&2
    fi
    rm -f "$ERRLOG"
    exit "$status"
}
trap report_failure EXIT

OUT="${1:-certs}"
TRUST_DOMAIN="${TRUST_DOMAIN:-prod.internal}"
DAYS="${DAYS:-365}"

if ! command -v openssl >/dev/null 2>&1; then
    echo "gen-certs: openssl is required" >&2
    exit 1
fi

mkdir -p "$OUT"
cd "$OUT"

echo "==> certificate authority"
openssl req -x509 -newkey rsa:2048 -nodes -days "$DAYS" \
    -keyout ca.key -out ca.crt \
    -subj "/CN=sluice-demo-ca/O=Sluice" \
    -addext "basicConstraints=critical,CA:TRUE,pathlen:0" \
    -addext "keyUsage=critical,keyCertSign,cRLSign" \
    2>>"$ERRLOG"

# issue <name> <spiffe-path> <extra-san>
#
# The SPIFFE ID goes in a URI SAN, which is what Sluice reads. The Common Name
# is set too, as a fallback for tooling that has not caught up.
issue() {
    name="$1"
    path="$2"
    extra="${3:-}"
    uri="spiffe://${TRUST_DOMAIN}${path}"

    san="URI:${uri}"
    [ -n "$extra" ] && san="${san},${extra}"

    openssl req -newkey rsa:2048 -nodes \
        -keyout "${name}.key" -out "${name}.csr" \
        -subj "/CN=${name}/O=Sluice" 2>>"$ERRLOG"

    cat > "${name}.ext" <<EOF
basicConstraints = CA:FALSE
keyUsage = critical,digitalSignature,keyEncipherment
extendedKeyUsage = serverAuth,clientAuth
subjectAltName = ${san}
EOF

    openssl x509 -req -in "${name}.csr" -CA ca.crt -CAkey ca.key \
        -CAcreateserial -out "${name}.crt" -days "$DAYS" \
        -extfile "${name}.ext" 2>>"$ERRLOG"

    rm -f "${name}.csr" "${name}.ext"
    echo "    ${name}.crt  ${uri}"
}

echo "==> server certificate (the data-plane listener)"
# The server certificate needs DNS and IP SANs covering every name a client
# will actually dial, or hostname verification rejects the connection before
# identity is ever considered — the mutual handshake succeeds and the request
# still fails, which reads like an authorisation problem and is not one.
#
# `envoy` and `sluiced` are the compose service names; `localhost` and
# 127.0.0.1 cover a host-side curl. SERVER_SANS extends the list for a
# deployment with its own hostnames.
SERVER_SANS="${SERVER_SANS:-DNS:localhost,DNS:envoy,DNS:sluiced,DNS:sluice,DNS:sluice.local,IP:127.0.0.1,IP:::1}"
issue server "/ns/sluice/sa/proxy" "$SERVER_SANS"

echo "==> workload certificates (clients)"
issue checkout "/ns/payments/sa/checkout"
issue feed     "/ns/web/sa/feed"
issue etl      "/ns/data/sa/etl"

# A certificate from a different trust domain, to prove the trust-domain
# check actually refuses one. Signed by the same CA on purpose: the point is
# that chaining to a trusted CA is not sufficient on its own.
echo "==> an identity from an untrusted domain (should be refused)"
TRUST_DOMAIN=evil.example issue intruder "/ns/attacker/sa/probe"

chmod 600 ./*.key
cd - >/dev/null

cat <<EOF

Certificates are in ./${OUT}

Run the data plane with mutual TLS:

  sluiced --demo --proxy :9443 --no-demo=false \\
    --config configs/mtls.jsonc

or point the environment at them:

  export SLUICE_LISTEN_PROXY=:9443
  export SLUICE_TLS_CERT_FILE=${OUT}/server.crt
  export SLUICE_TLS_KEY_FILE=${OUT}/server.key
  export SLUICE_TLS_CLIENT_CA_FILE=${OUT}/ca.crt
  export SLUICE_TLS_TRUST_DOMAINS=${TRUST_DOMAIN}
  sluiced --demo

Then exercise the identity path:

  # authorised — a payments workload, over mTLS
  curl --cacert ${OUT}/ca.crt --cert ${OUT}/checkout.crt --key ${OUT}/checkout.key \\
       https://localhost:9443/api/v1/feed -D-

  # refused — no client certificate at all
  curl --cacert ${OUT}/ca.crt https://localhost:9443/api/v1/feed

  # refused — valid chain, wrong trust domain
  curl --cacert ${OUT}/ca.crt --cert ${OUT}/intruder.crt --key ${OUT}/intruder.key \\
       https://localhost:9443/api/v1/feed -D-

EOF
