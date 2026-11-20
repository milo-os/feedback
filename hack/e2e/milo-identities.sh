#!/usr/bin/env bash
# Adds feedback's service identities and the e2e test users to the static
# token file of the Milo in the test-infra cluster, then restarts Milo's API
# server if that changed anything. Run after Milo's `task dev:deploy`, which
# resets the token file.
set -euo pipefail
cd "$(dirname "$0")/../.."
export KUBECONFIG="${INFRA_KUBECONFIG:-.test-infra/kubeconfig}"

current=$(kubectl -n milo-system get secret milo-apiserver-auth-tokens -o jsonpath='{.data.tokens\.csv}' | base64 -d)
want="$current"
while IFS= read -r line; do
  case "$line" in ''|'#'*) continue ;; esac
  grep -qxF "$line" <<<"$want" || want+=$'\n'"$line"
done < config/overlays/e2e/milo/tokens.csv

if [ "$want" = "$current" ]; then
  echo "Milo already has feedback's e2e tokens"
else
  kubectl -n milo-system patch secret milo-apiserver-auth-tokens --type merge \
    -p "$(jq -n --arg t "$want"$'\n' '{data: {"tokens.csv": ($t | @base64)}}')"
  echo "Added feedback's e2e tokens; restarting Milo's API server"
  kubectl -n milo-system rollout restart deployment/milo-apiserver
fi
kubectl -n milo-system rollout status deployment/milo-apiserver --timeout=300s

# The new API server takes a moment to serve once it is Ready.
for _ in $(seq 1 60); do
  code=$(curl -sk -o /dev/null -w '%{http_code}' -H "Authorization: Bearer e2e-feedback-operator-token" https://localhost:30443/apis || true)
  if [ "$code" = 200 ]; then
    echo "Milo accepts the operator's token"
    exit 0
  fi
  sleep 2
done
echo "Milo doesn't accept the operator's token (last answer: $code)" >&2
exit 1
