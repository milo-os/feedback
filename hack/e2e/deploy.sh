#!/usr/bin/env bash
# Installs feedback in the Milo in the test-infra kind cluster and runs the
# operator, feedback-uploads and MinIO next to it (task e2e:deploy). Safe to
# run again.
#
#   1. In Milo: the CRDs, RBAC for feedback's components, the e2e users and
#      their roles (config/overlays/e2e/milo, hack/e2e/iam-rbac.sh).
#   2. In the cluster: config/overlays/e2e, with Milo's CA as the milo-ca
#      ConfigMap, and the attachments bucket in MinIO.
#   3. Once the operator is ready, the admission webhooks in Milo, with the
#      webhook CA as their caBundle.
set -euo pipefail
cd "$(dirname "$0")/../.."
INFRA_KUBECONFIG="${INFRA_KUBECONFIG:-.test-infra/kubeconfig}"
MILO_KUBECONFIG="${MILO_KUBECONFIG:-test/e2e/kubeconfig-milo}"
OVERLAY="${OVERLAY:-config/overlays/e2e}"
IMAGE="${IMAGE:-ghcr.io/milo-os/feedback:e2e}"
k() { kubectl --kubeconfig "$INFRA_KUBECONFIG" "$@"; }
m() { kubectl --kubeconfig "$MILO_KUBECONFIG" "$@"; }
webhooks='.kind == "MutatingWebhookConfiguration" or .kind == "ValidatingWebhookConfiguration"'

echo "➤ CRDs in Milo"
m apply --server-side --force-conflicts -k config/crd
for crd in $(kustomize build config/crd | yq -N '.metadata.name'); do
  m wait --for=condition=Established "crd/$crd" --timeout=60s
done

echo "➤ RBAC and e2e users in Milo"
hack/e2e/iam-rbac.sh | m apply -f -
kustomize build "$OVERLAY/milo" | yq "select(($webhooks) | not)" | m apply -f -

# test-infra's Milo authorizes with RBAC, so the IAM roles and protected
# resources grant nothing there; check Milo accepts them. (config/milo/bindings
# names groups each environment provides, so it isn't checked.)
echo "➤ Milo accepts config/milo/iam (server dry run)"
iam=$(mktemp -d)
cp -R config/milo "$iam/milo"
printf 'apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\ncomponents: [milo/iam]\n' > "$iam/kustomization.yaml"
kustomize build "$iam" | m apply --dry-run=server -f - >/dev/null
rm -rf "$iam"

# The admission policy that names the post in vote and follow delete audit
# events. The ActivityPolicies and search policies need services test-infra
# doesn't run.
if [ -f config/milo/activity/post-ref.yaml ]; then
  echo "➤ post-ref admission policy in Milo"
  m apply -f config/milo/activity/post-ref.yaml
fi

echo "➤ Stand-in audit sink for Milo"
k apply -f "$OVERLAY/audit-sink.yaml"
k -n milo-system rollout status deployment/vector-audit-log-processor --timeout=120s

echo "➤ feedback in the cluster ($IMAGE)"
k apply -f "$OVERLAY/namespace.yaml"
k -n milo-system get secret milo-apiserver-tls -o jsonpath='{.data.ca\.crt}' | base64 -d > "${TMPDIR:-/tmp}/feedback-e2e-milo-ca.crt"
k -n feedback-system create configmap milo-ca --from-file=ca.crt="${TMPDIR:-/tmp}/feedback-e2e-milo-ca.crt" \
  --dry-run=client -o yaml | k apply -f -
# cert-manager's webhook can lag its rollout on a new cluster.
# A Job's template can't change; it is only there to make the bucket once.
k -n feedback-system delete job minio-bucket --ignore-not-found >/dev/null
until k apply -k "$OVERLAY"; do echo "  retrying in 5s"; sleep 5; done
k -n feedback-system set image deployment/feedback-operator operator="$IMAGE" >/dev/null
k -n feedback-system set image deployment/feedback-uploads uploads="$IMAGE" >/dev/null

echo "➤ Attachments bucket in MinIO"
k -n feedback-system rollout status deployment/minio --timeout=180s
k -n feedback-system wait --for=condition=Complete job/minio-bucket --timeout=180s

echo "➤ Waiting for the operator and feedback-uploads"
k -n feedback-system wait --for=condition=Ready certificate/feedback-operator-webhook --timeout=120s
k -n feedback-system rollout status deployment/feedback-operator --timeout=300s
# Not fatal: the attachments test reports a feedback-uploads that won't run.
if ! k -n feedback-system rollout status deployment/feedback-uploads --timeout=180s; then
  echo "⚠️  feedback-uploads isn't ready; its last log lines:" >&2
  k -n feedback-system logs deployment/feedback-uploads --tail=5 >&2 || true
fi

echo "➤ Admission webhooks in Milo"
CA=$(k -n feedback-system get secret feedback-e2e-ca -o jsonpath='{.data.ca\.crt}')
export CA
kustomize build "$OVERLAY/milo" |
  yq "select($webhooks) | .webhooks[].clientConfig.caBundle = strenv(CA)" | m apply -f -

echo "✅ feedback is deployed. Run the tests with: task e2e:test"
