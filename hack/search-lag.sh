#!/usr/bin/env bash
# Measures how long a post change takes to reach Milo's search index.
#
# It hides a visible post, waits until search no longer finds it, then puts
# back the post's original spec.hidden and times how long until search finds
# it again. Hiding is visible to users while it lasts, so run it against
# staging.
#
#   KUBECONFIG=<milo staff kubeconfig> hack/search-lag.sh <post-name> [timeout-seconds]
#
# The kubeconfig must point at Milo's platform root, not a user, organization
# or project control plane: search only returns posts to platform-context
# queries (see "Who can search posts" in README.md).
#
# The post must be visible and indexed; the script exits without changing
# anything otherwise. Once it has hidden the post, it restores the original
# spec.hidden on every exit, including errors, timeouts and Ctrl-C.
#
# Needs: kubectl, jq; staff permission to patch posts and create
# resourcesearchqueries.
set -euo pipefail

post=${1:?usage: search-lag.sh <post-name> [timeout-seconds]}
timeout=${2:-300}

obj=$(kubectl get post "$post" -o json)
title=$(jq -r '.status.title // ""' <<<"$obj")
# true, false, or null when the field is unset; a merge patch with null
# removes the field again, so restoring gives back exactly what was there.
# (Not `.spec.hidden // null`: jq's // would turn false into null.)
original=$(jq -c '.spec // {} | if has("hidden") then .hidden else null end' <<<"$obj")

[[ -n $title ]] || { echo "post $post has no title" >&2; exit 1; }
[[ $original != true ]] || { echo "post $post is hidden, so it isn't indexed; pick a visible post" >&2; exit 1; }

# found exits 0 if search finds the post by its title, 1 if it doesn't, and 2
# if the query fails or the posts index isn't ready.
found() {
	local query out denied hit
	query=$(jq -cn --arg t "$title" '{
		apiVersion: "search.miloapis.com/v1alpha1", kind: "ResourceSearchQuery",
		metadata: {generateName: "search-lag-"},
		spec: {query: $t, limit: 100,
		       targetResources: [{group: "feedback.miloapis.com", version: "v1alpha1", kind: "Post"}]}}') || return 2
	out=$(kubectl create -o json -f - <<<"$query") || return 2
	denied=$(jq '.status.deniedTargetResources // [] | length' <<<"$out") || return 2
	if ((denied > 0)); then
		echo "search has no ready index for posts" >&2
		return 2
	fi
	hit=$(jq --arg n "$post" '.status.results // [] | any(.resource.metadata.name == $n)' <<<"$out") || return 2
	[[ $hit == true ]]
}

wait_for() { # wait_for <want: 0 found | 1 not found>
	local start=$SECONDS rc
	while ((SECONDS - start < timeout)); do
		rc=0
		found || rc=$?
		((rc == $1)) && return 0
		((rc == 2)) && { echo "search query failed" >&2; return 1; }
		sleep 1
	done
	echo "timed out after ${timeout}s" >&2
	return 1
}

set_hidden() { # set_hidden <true|false|null>
	kubectl patch post "$post" --type=merge -p "$(jq -cn --argjson h "$1" '{spec: {hidden: $h}}')" >/dev/null
}

changed=0
restore() {
	local rc=$?
	if ((changed)); then
		if set_hidden "$original"; then
			echo "restored spec.hidden to $original" >&2
		else
			echo "FAILED to restore $post: set spec.hidden back to $original by hand" >&2
			rc=1
		fi
	fi
	exit "$rc"
}
trap restore EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

rc=0
found || rc=$?
case $rc in
0) ;;
1) echo "search doesn't find $post by its title; is it indexed?" >&2; exit 1 ;;
*) echo "search query failed" >&2; exit 1 ;;
esac

# Set before patching: if the patch fails partway, restoring is harmless.
changed=1
set_hidden true
t0=$SECONDS
wait_for 1
echo "removed from the index after about $((SECONDS - t0))s"

set_hidden "$original"
changed=0
t0=$SECONDS
wait_for 0
echo "back in the index after about $((SECONDS - t0))s"
