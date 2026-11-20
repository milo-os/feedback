#!/usr/bin/env bash
# Checks which Milo contexts can find a post in search, as README.md's "Who
# can search posts" describes: a query at the platform root finds a visible,
# indexed post, and a user's own context has no search API yet. Once search
# serves ResourceSearchQuery there, the user context finds the post too
# (visibility: AllContexts), and this reports README.md as out of date.
#
#   PLATFORM_KUBECONFIG=<milo platform-root kubeconfig> \
#   USER_KUBECONFIG=<kubeconfig for a user's control plane> \
#     hack/search-scope.sh <post-name>
#
# USER_KUBECONFIG's server must end in
# /apis/iam.miloapis.com/v1alpha1/users/<id>/control-plane, which is how
# datumctl and the cloud portal reach the user context. Run it against
# staging. It only reads posts and creates ResourceSearchQueries, which
# aren't stored.
#
# Exits 0 when search behaves as documented, 1 when it doesn't (so the
# README is out of date), and 2 when a query fails.
#
# Needs: kubectl, jq.
set -euo pipefail

post=${1:?usage: search-scope.sh <post-name>}
: "${PLATFORM_KUBECONFIG:?set PLATFORM_KUBECONFIG}" "${USER_KUBECONFIG:?set USER_KUBECONFIG}"

server=$(KUBECONFIG=$USER_KUBECONFIG kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
[[ $server == */apis/iam.miloapis.com/v1alpha1/users/*/control-plane ]] || {
	echo "USER_KUBECONFIG's server $server isn't a user control plane" >&2
	exit 2
}

obj=$(KUBECONFIG=$PLATFORM_KUBECONFIG kubectl get post "$post" -o json)
title=$(jq -r '.status.title // ""' <<<"$obj")
[[ -n $title ]] || { echo "post $post has no title" >&2; exit 2; }
[[ $(jq '.spec.hidden == true' <<<"$obj") == false ]] || { echo "post $post is hidden; pick a visible post" >&2; exit 2; }

# served <kubeconfig> reports whether that context serves ResourceSearchQuery.
served() {
	KUBECONFIG=$1 kubectl api-resources --api-group=search.miloapis.com -o name 2>/dev/null | grep -qx 'resourcesearchqueries.search.miloapis.com'
}

# search <kubeconfig> prints the names of the posts the query returns.
search() {
	local query out
	query=$(jq -cn --arg t "$title" '{
		apiVersion: "search.miloapis.com/v1alpha1", kind: "ResourceSearchQuery",
		metadata: {generateName: "search-scope-"},
		spec: {query: $t, limit: 100,
		       targetResources: [{group: "feedback.miloapis.com", version: "v1alpha1", kind: "Post"}]}}')
	out=$(KUBECONFIG=$1 kubectl create -o json -f - <<<"$query") || return 1
	if [[ $(jq '.status.deniedTargetResources // [] | length' <<<"$out") != 0 ]]; then
		echo "search has no ready index for posts" >&2
		return 1
	fi
	jq -r '.status.results // [] | .[].resource.metadata.name' <<<"$out"
}

platform=$(search "$PLATFORM_KUBECONFIG") || exit 2

ok=1
if grep -qxF "$post" <<<"$platform"; then
	echo "platform root: finds $post"
else
	echo "platform root: doesn't find $post; is it indexed?"
	ok=0
fi
if ! served "$USER_KUBECONFIG"; then
	echo "user context: no search API"
else
	user=$(search "$USER_KUBECONFIG") || exit 2
	if grep -qxF "$post" <<<"$user"; then
		echo "user context: search is served and finds $post; README.md's \"Who can search posts\" is out of date"
	else
		echo "user context: search is served but doesn't find $post; check the policies' visibility"
	fi
	ok=0
fi
((ok)) || exit 1
