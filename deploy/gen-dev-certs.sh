#!/usr/bin/env bash
# Creates a throwaway CA, a server certificate for wilbur, and client certificates for mutual TLS,
# for trying out TLS and auth.mtls locally. Not for production: keys are unencrypted and long-lived.
#   deploy/gen-dev-certs.sh [outdir] [client CNs...]     (default clients: SFU DefaultStreamer)
set -euo pipefail
out=${1:-certs}; shift || true
if [ $# -gt 0 ]; then clients=("$@"); else clients=(SFU DefaultStreamer); fi
mkdir -p "$out" && cd "$out"
umask 077

openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 365 \
  -keyout ca.key -out ca.pem -subj "/CN=pixelstreaming dev CA" 2>/dev/null

openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -keyout server.key -out server.csr \
  -subj "/CN=localhost" 2>/dev/null
openssl x509 -req -in server.csr -CA ca.pem -CAkey ca.key -CAcreateserial -days 365 -out server.pem \
  -extfile <(printf "subjectAltName=DNS:localhost,IP:127.0.0.1\nextendedKeyUsage=serverAuth") 2>/dev/null

for cn in "${clients[@]}"; do
  openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -keyout "client-$cn.key" -out "client-$cn.csr" \
    -subj "/CN=$cn" 2>/dev/null
  openssl x509 -req -in "client-$cn.csr" -CA ca.pem -CAkey ca.key -CAcreateserial -days 365 \
    -out "client-$cn.pem" -extfile <(printf "extendedKeyUsage=clientAuth") 2>/dev/null
done
rm -f ./*.csr ./*.srl
echo "wrote $(pwd): ca.pem, server.{pem,key}, client-<cn>.{pem,key} for: ${clients[*]}"
