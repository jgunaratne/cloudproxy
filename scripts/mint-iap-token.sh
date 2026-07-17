#!/usr/bin/env bash
# Mint a 1-hour token that passes IAP on the Cloud Run service, for
# machine clients (the Pi publisher). Requires:
#   - gcloud auth application-default login (once)
#   - roles/iam.serviceAccountTokenCreator on the SA below
#
# IAP on Cloud Run uses a Google-managed OAuth client, which rejects
# plain OIDC identity tokens. The flow that works is a service-account
# signed JWT with the resource URL + /* wildcard as audience
# (https://cloud.google.com/iap/docs/authentication-howto).
#
# Usage:  ./scripts/mint-iap-token.sh            # prints the token
#         GCP_IDENTITY_TOKEN=$(./scripts/mint-iap-token.sh)
set -euo pipefail

SA="cloudproxy-pi@hansel-487018.iam.gserviceaccount.com"
AUD="https://cloudproxy-server-530731599092.us-west1.run.app/*"

NOW=$(date +%s)
PAYLOAD=$(python3 -c "import json; print(json.dumps({'payload': json.dumps({'iss':'$SA','sub':'$SA','aud':'$AUD','iat':$NOW,'exp':$NOW+3600})}))")

# curl (not python urllib) for the network call: some local Pythons lack
# SSL certs, and gcloud's own bundled Python may be missing cryptography.
curl -sf -X POST \
  -H "Authorization: Bearer $(gcloud auth application-default print-access-token)" \
  -H "Content-Type: application/json" \
  -d "$PAYLOAD" \
  "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/$SA:signJwt" \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["signedJwt"])'
