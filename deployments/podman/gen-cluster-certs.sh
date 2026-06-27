#!/usr/bin/env bash
# gen-cluster-certs.sh — generate the internal CA + mTLS certs for the
# vornik cluster compose or Helm install.
#
# Produces (in deployments/podman/certs/):
#   ca.crt / ca.key                     — internal CA (self-signed, dev only)
#   worker-server.crt / worker-server.key — server cert for the worker relay-ingress
#   webhook-client.crt / webhook-client.key — client cert for vornik-webhook
#
# The compose file (cluster.compose.yaml) mounts these into the worker and
# webhook containers. The CA cert is mounted into both so each side can
# verify the other.  For a Helm install, create the Kubernetes Secret from
# these files (see deployments/RKE2.md §11.4).
#
# Run this ONCE before `podman-compose -f cluster.compose.yaml up`.
# Regenerate if certs expire or if you rotate the CA.
#
# Requirements: openssl (available on most Linux/macOS distros).
#
# Usage:
#   # Podman compose path (default — worker DNS = vornik-worker):
#   bash deployments/podman/gen-cluster-certs.sh
#
#   # Helm install: pass the Kubernetes Service DNS for the worker tier.
#   # The service is named <release>-vornik-worker by the chart helpers.
#   VORNIK_WORKER_DNS="vornik-vornik-worker" bash deployments/podman/gen-cluster-certs.sh
#
#   # Or using the flag form (both env and flag work; flag takes precedence):
#   bash deployments/podman/gen-cluster-certs.sh --worker-dns vornik-vornik-worker
#
#   # Multiple names (covers both compose and helm in one cert):
#   VORNIK_WORKER_DNS="vornik-worker,vornik-vornik-worker" ./gen-cluster-certs.sh
#
# The generated server cert SAN always includes:
#   - every name listed in VORNIK_WORKER_DNS (comma-separated)
#   - DNS:localhost
#   - IP:127.0.0.1
#
# Output certs are valid for 365 days (development use only).
# For production, use your own PKI / cert-manager / Vault.

set -euo pipefail

# ---------------------------------------------------------------------------
# Argument parsing
# ---------------------------------------------------------------------------
WORKER_DNS="${VORNIK_WORKER_DNS:-vornik-worker}"   # default: podman compose name

while [[ $# -gt 0 ]]; do
    case "$1" in
        --worker-dns)
            WORKER_DNS="$2"
            shift 2
            ;;
        --worker-dns=*)
            WORKER_DNS="${1#--worker-dns=}"
            shift
            ;;
        -h|--help)
            grep '^#' "$0" | sed 's/^# \?//'
            exit 0
            ;;
        *)
            echo "ERROR: unknown argument: $1" >&2
            echo "Usage: $0 [--worker-dns <name>[,<name>...]]" >&2
            exit 1
            ;;
    esac
done

# ---------------------------------------------------------------------------
# Build the SAN string: every provided name + localhost + 127.0.0.1
# ---------------------------------------------------------------------------
SAN="DNS:localhost,IP:127.0.0.1"
IFS=',' read -ra DNS_NAMES <<< "$WORKER_DNS"
for name in "${DNS_NAMES[@]}"; do
    name="${name// /}"   # trim spaces
    [[ -z "$name" ]] && continue
    SAN="DNS:${name},${SAN}"
done

echo "Worker DNS names : ${WORKER_DNS}"
echo "Full SAN         : ${SAN}"

# ---------------------------------------------------------------------------

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CERT_DIR="${SCRIPT_DIR}/certs"

if ! command -v openssl &>/dev/null; then
    echo "ERROR: openssl not found. Install it and re-run." >&2
    exit 1
fi

mkdir -p "${CERT_DIR}"

echo "--- Generating internal CA ---"
openssl req -x509 -newkey rsa:4096 -sha256 -days 365 -nodes \
    -keyout "${CERT_DIR}/ca.key" \
    -out    "${CERT_DIR}/ca.crt" \
    -subj   "/CN=vornik-cluster-internal-ca/O=vornik-dev"

echo "--- Generating worker server key + CSR ---"
openssl req -newkey rsa:4096 -sha256 -nodes \
    -keyout "${CERT_DIR}/worker-server.key" \
    -out    "${CERT_DIR}/worker-server.csr" \
    -subj   "/CN=${DNS_NAMES[0]}/O=vornik-dev"

# SAN extension: covers all provided worker DNS names plus localhost/127.0.0.1
# so smoke tests against 127.0.0.1:8443 work out of the box.
cat > "${CERT_DIR}/worker-server.ext" <<EOF
subjectAltName=${SAN}
extendedKeyUsage=serverAuth
EOF

echo "--- Signing worker server cert with internal CA ---"
openssl x509 -req -sha256 -days 365 \
    -in  "${CERT_DIR}/worker-server.csr" \
    -CA  "${CERT_DIR}/ca.crt" \
    -CAkey "${CERT_DIR}/ca.key" \
    -CAcreateserial \
    -extfile "${CERT_DIR}/worker-server.ext" \
    -out "${CERT_DIR}/worker-server.crt"

echo "--- Generating webhook client key + CSR ---"
openssl req -newkey rsa:4096 -sha256 -nodes \
    -keyout "${CERT_DIR}/webhook-client.key" \
    -out    "${CERT_DIR}/webhook-client.csr" \
    -subj   "/CN=vornik-webhook/O=vornik-dev"

cat > "${CERT_DIR}/webhook-client.ext" <<EOF
extendedKeyUsage=clientAuth
EOF

echo "--- Signing webhook client cert with internal CA ---"
openssl x509 -req -sha256 -days 365 \
    -in  "${CERT_DIR}/webhook-client.csr" \
    -CA  "${CERT_DIR}/ca.crt" \
    -CAkey "${CERT_DIR}/ca.key" \
    -CAcreateserial \
    -extfile "${CERT_DIR}/webhook-client.ext" \
    -out "${CERT_DIR}/webhook-client.crt"

# Clean up intermediate files — not needed at runtime.
rm -f "${CERT_DIR}/worker-server.csr" \
      "${CERT_DIR}/worker-server.ext" \
      "${CERT_DIR}/webhook-client.csr" \
      "${CERT_DIR}/webhook-client.ext" \
      "${CERT_DIR}/ca.srl"

echo ""
echo "Certs written to ${CERT_DIR}/"
echo ""
echo "  ca.crt               — CA cert  (mounted into both worker and webhook)"
echo "  worker-server.crt/key — server cert for worker relay-ingress :8443"
echo "  webhook-client.crt/key — client cert for vornik-webhook outbound relay"
echo ""
echo "Worker server cert SAN:"
openssl x509 -in "${CERT_DIR}/worker-server.crt" -noout -text \
    | grep -A1 "Subject Alternative Name" || true
echo ""
echo "NOTE: These are self-signed dev certs. Valid 365 days."
echo "      For production use your org PKI / cert-manager / Vault."
echo ""
echo "Podman compose next step:"
echo "  podman-compose -f deployments/podman/cluster.compose.yaml up -d"
echo ""
echo "Helm next step (see deployments/RKE2.md §11.4):"
echo "  kubectl create secret generic <secretName> -n <namespace> \\"
echo "    --from-file=ca.crt=${CERT_DIR}/ca.crt \\"
echo "    --from-file=server.crt=${CERT_DIR}/worker-server.crt \\"
echo "    --from-file=server.key=${CERT_DIR}/worker-server.key \\"
echo "    --from-file=client.crt=${CERT_DIR}/webhook-client.crt \\"
echo "    --from-file=client.key=${CERT_DIR}/webhook-client.key"
