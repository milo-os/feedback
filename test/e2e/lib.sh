# Helpers for the feedback e2e scripts. Chainsaw runs each script in its
# test's directory: source this with `. ../lib.sh`.
#
# Milo is https://localhost:30443 (Milo's test-infra gateway). Callers:
#   test-user      Milo's own test user (token test-user-token)
#   other-user, limit-user          more users (config/overlays/e2e/milo)
#   staff-user     feedback reviewer; security-user: Security Team
#   sync           system:feedback-sync@feedback.miloapis.com
#   admin          system:masters
set -euo pipefail

E2E_DIR=$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" 2>/dev/null && pwd || pwd)
[ -f "$E2E_DIR/kubeconfig-milo" ] || E2E_DIR=$(cd .. && pwd)
REPO_DIR=$(cd "$E2E_DIR/../.." && pwd)
MILO=${MILO_URL:-https://localhost:30443}
FB=apis/feedback.miloapis.com/v1alpha1
INFRA_KUBECONFIG=${INFRA_KUBECONFIG:-$REPO_DIR/.test-infra/kubeconfig}
OUT=$(mktemp)
trap 'rm -f "$OUT"' EXIT

token() {
  case $1 in
    test-user) echo test-user-token ;;
    other-user) echo e2e-other-user-token ;;
    limit-user) echo e2e-limit-user-token ;;
    staff-user) echo e2e-staff-token ;;
    security-user) echo e2e-security-token ;;
    sync) echo e2e-feedback-sync-token ;;
    admin) echo test-admin-token ;;
    *) echo "unknown caller $1" >&2; return 1 ;;
  esac
}

# url <context> <resource path>: <context> is a user name for that user's own
# context, or "platform".
url() {
  if [ "$1" = platform ]; then
    echo "$MILO/$FB/$2"
  else
    echo "$MILO/apis/iam.miloapis.com/v1alpha1/users/$1/control-plane/$FB/$2"
  fi
}

# call <caller> <method> <url> [json body]: prints the HTTP status; the
# response body is in $OUT.
call() {
  local who=$1 method=$2 u=$3 body=${4:-} ctype=application/json
  case $method in PATCH) ctype=application/merge-patch+json ;; esac
  curl -sk -o "$OUT" -w '%{http_code}' -X "$method" \
    -H "Authorization: Bearer $(token "$who")" -H "Content-Type: $ctype" \
    ${body:+--data "$body"} "$u"
}

# expect <code> <what> <call args...>
expect() {
  local want=$1 what=$2 got
  shift 2
  got=$(call "$@")
  if [ "$got" != "$want" ]; then
    echo "FAIL: $what: got HTTP $got, want $want" >&2
    cat "$OUT" >&2; echo >&2
    exit 1
  fi
  echo "ok: $what ($got)"
}

# jqo <filter>: applies filter to the last response.
jqo() { jq -r "$@" "$OUT"; }

fail() { echo "FAIL: $*" >&2; exit 1; }

# milo / infra: kubectl as Milo's admin, or against the kind cluster.
milo() { kubectl --kubeconfig "$E2E_DIR/kubeconfig-milo" "$@"; }
infra() { kubectl --kubeconfig "$INFRA_KUBECONFIG" "$@"; }

# eventually <seconds> <what> <command...>: retries command every second.
eventually() {
  local secs=$1 what=$2 i
  shift 2
  for ((i = 0; i < secs; i++)); do
    if "$@" >/dev/null 2>&1; then echo "ok: $what"; return 0; fi
    sleep 1
  done
  echo "FAIL: $what (after ${secs}s); last try:" >&2
  "$@" >&2 || true
  exit 1
}

# jget <kind> <name> <jsonpath>: a field of a feedback object, read as admin.
jget() { milo get "$1.feedback.miloapis.com" "$2" -o "jsonpath=$3"; }

# is <want> <command...>: true when the command prints want.
is() { local want=$1; shift; [ "$("$@" 2>/dev/null)" = "$want" ]; }

# gone <kind> <name>: true when the object doesn't exist.
gone() { ! milo get "$1.feedback.miloapis.com" "$2" >/dev/null 2>&1; }

# post <name> [title]: feedback-sync creates a post, as it does from GitHub.
post() {
  expect 201 "sync creates post $1" sync POST "$(url platform posts)" \
    "{\"apiVersion\":\"feedback.miloapis.com/v1alpha1\",\"kind\":\"Post\",\"metadata\":{\"name\":\"$1\"},\"spec\":{}}"
  expect 200 "sync sets post $1's title" sync PATCH "$(url platform posts/$1/status)" \
    "{\"status\":{\"title\":\"${2:-$1}\",\"state\":\"backlog\"}}"
}

# vote <user> <post>: the user votes in their own context.
vote() {
  expect 201 "$1 votes on $2" "$1" POST "$(url "$1" votes)" \
    "{\"apiVersion\":\"feedback.miloapis.com/v1alpha1\",\"kind\":\"Vote\",\"metadata\":{\"name\":\"$2.$1\"},\"spec\":{\"postRef\":{\"name\":\"$2\"}}}"
}

# cleanup_posts <post...>: deletes the posts and every vote and follow on
# them, as admin.
cleanup_posts() {
  local p
  for p in "$@"; do
    milo delete votes.feedback.miloapis.com --field-selector "spec.postRef.name=$p" --ignore-not-found --wait=false || true
    milo delete subscriptions.feedback.miloapis.com --field-selector "spec.postRef.name=$p" --ignore-not-found --wait=false || true
    milo delete posts.feedback.miloapis.com "$p" --ignore-not-found --wait=false || true
  done
}

# submit <user> <name> [extra spec json]: the user sends feedback.
submit() {
  local extra=${3:-}
  expect 201 "$1 sends submission $2" "$1" POST "$(url "$1" submissions)" \
    "{\"apiVersion\":\"feedback.miloapis.com/v1alpha1\",\"kind\":\"Submission\",\"metadata\":{\"name\":\"$2\"},\"spec\":{\"kind\":\"idea\",\"title\":\"e2e $2\",\"body\":\"Sent by the feedback e2e tests.\"${extra:+,$extra}}}"
}

# minio_ls <prefix>: lists the attachments bucket under prefix, with the
# MinIO client in a pod in the cluster.
minio_ls() {
  local pod="e2e-mc-$RANDOM$RANDOM" phase i
  infra -n feedback-system run "$pod" --restart=Never --quiet \
    --image=chainguard/minio-client@sha256:43a0d3eae573e611d6b07a13255e3c4f4161220232fbcfb9951d8910b9a72331 \
    --env=MC_CONFIG_DIR=/tmp/mc \
    --env=MC_HOST_local=http://feedback-e2e:feedback-e2e-secret@minio.feedback-system.svc.cluster.local:9000 \
    -- ls --recursive "local/feedback-attachments/$1" >/dev/null
  for ((i = 0; i < 60; i++)); do
    phase=$(infra -n feedback-system get pod "$pod" -o jsonpath='{.status.phase}')
    case $phase in Succeeded|Failed) break ;; esac
    sleep 1
  done
  infra -n feedback-system logs "$pod"
  infra -n feedback-system delete pod "$pod" --wait=false >/dev/null
  [ "$phase" = Succeeded ] || { echo "mc ls failed ($phase)" >&2; return 1; }
}
