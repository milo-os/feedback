#!/usr/bin/env bash
# Measures how long a post change takes to reach Milo's search index.
#
# It hides a visible post, waits until search no longer finds it, then
# unhides it and times how long until search finds it again. Hiding is
# visible to users while it lasts, so run it against staging.
#
#   KUBECONFIG=<milo staff kubeconfig> hack/search-lag.sh <post-name> [timeout-seconds]
#
# Needs: kubectl, jq; staff permission to patch posts and create
# resourcesearchqueries.
set -euo pipefail

post=${1:?usage: search-lag.sh <post-name> [timeout-seconds]}
timeout=${2:-300}

title=$(kubectl get post "$post" -o jsonpath='{.status.title}')
[[ -n $title ]] || { echo "post $post has no title" >&2; exit 1; }

found() {
	kubectl create -o json -f - <<JSON | jq -e --arg n "$post" '.status.results // [] | any(.resource.metadata.name == $n)' >/dev/null
{"apiVersion": "search.miloapis.com/v1alpha1", "kind": "ResourceSearchQuery",
 "metadata": {"generateName": "search-lag-"},
 "spec": {"query": $(jq -Rn --arg t "$title" '$t'), "limit": 100,
          "targetResources": [{"group": "feedback.miloapis.com", "version": "v1alpha1", "kind": "Post"}]}}
JSON
}

wait_for() { # wait_for <want-found: 0|1>
	local start=$SECONDS
	while (( SECONDS - start < timeout )); do
		if found; then [[ $1 == 1 ]] && return 0; else [[ $1 == 0 ]] && return 0; fi
		sleep 1
	done
	echo "timed out after ${timeout}s" >&2
	return 1
}

trap 'kubectl patch post "$post" --type=merge -p "{\"spec\":{\"hidden\":false}}" >/dev/null' EXIT

found || { echo "search doesn't find $post by its title; is it indexed?" >&2; exit 1; }

kubectl patch post "$post" --type=merge -p '{"spec":{"hidden":true}}' >/dev/null
t0=$SECONDS
wait_for 0
echo "removed from the index after about $((SECONDS - t0))s"

kubectl patch post "$post" --type=merge -p '{"spec":{"hidden":false}}' >/dev/null
t0=$SECONDS
wait_for 1
echo "back in the index after about $((SECONDS - t0))s"
