#!/usr/bin/env bash
# Prints a ClusterRole for each Milo IAM role in config/milo/iam/roles, named
# feedback-e2e:<role>, with the role's permissions and those of the roles it
# inherits. test-infra's Milo authorizes with RBAC, so the e2e tests grant
# these instead of the IAM roles. A permission is
# feedback.miloapis.com/<resource>[/<subresource>].<verb>.
set -euo pipefail
cd "$(dirname "$0")/../.."
dir=config/milo/iam/roles

perms() { # perms <role name>: the role's permissions, inherited ones included
  local f
  f=$(grep -l "^  name: $1\$" "$dir"/*.yaml)
  yq '.spec.includedPermissions[]' "$f"
  for parent in $(yq '.spec.inheritedRoles[].name' "$f"); do perms "$parent"; done
}

for f in "$dir"/*.yaml; do
  [ "$(yq '.kind' "$f")" = Role ] || continue
  name=$(yq '.metadata.name' "$f")
  echo "---"
  perms "$name" | sort -u | jq -R -s --arg name "$name" '
    split("\n") | map(select(. != "") | capture("^(?<group>[^/]+)/(?<resource>.+)\\.(?<verb>[a-z]+)$"))
    | group_by([.group, .resource])
    | {apiVersion: "rbac.authorization.k8s.io/v1", kind: "ClusterRole",
       metadata: {name: ("feedback-e2e:" + $name), labels: {"app.kubernetes.io/part-of": "feedback-e2e"}},
       rules: map({apiGroups: [.[0].group], resources: [.[0].resource], verbs: map(.verb)})}' | yq -P
done
