#!/usr/bin/env bash
# Checks which Milo contexts can find a post in search, as README.md's "Who
# can search posts" describes: a query at the platform root and a query in a
# user's own context both find a visible, indexed post.
#
#   PLATFORM_KUBECONFIG=<milo platform-root kubeconfig> \
#   USER_KUBECONFIG=<kubeconfig for a user's control plane> \
#     hack/search-scope.sh <post-name>
#
# USER_KUBECONFIG's server must end in
# /apis/iam.miloapis.com/v1alpha1/users/<id>/control-plane, which is how
# datumctl and the cloud portal reach the user context, and its user needs the
# searcher permission there. Run it against staging. It only reads posts and
# creates ResourceSearchQueries, which aren't stored.
#
# Exits 0 when search behaves as documented, 1 when it doesn't, and 2 when a
# discovery request or query fails.
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

# served <kubeconfig> returns 0 when that context serves ResourceSearchQuery,
# 1 when discovery succeeds without it, and 2 when discovery fails. kubectl
# exits non-zero when any API group fails discovery, so a listed
# resourcesearchqueries still counts as served.
served() {
	local out rc=0
	out=$(KUBECONFIG=$1 kubectl api-resources --api-group=search.miloapis.com -o name 2>&1) || rc=$?
	if grep -qx 'resourcesearchqueries.search.miloapis.com' <<<"$out"; then
		return 0
	fi
	if ((rc)); then
		printf 'discovery failed:\n%s\n' "$out" >&2
		return 2
	fi
	return 1
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

rc=0
served "$USER_KUBECONFIG" || rc=$?
case $rc in
0)
	user=$(search "$USER_KUBECONFIG") || exit 2
	if grep -qxF "$post" <<<"$user"; then
		echo "user context: finds $post"
	else
		echo "user context: doesn't find $post; check the policies' visibility"
		ok=0
	fi
	;;
1)
	echo "user context: no search API; is milo-os/search#175 deployed?"
	ok=0
	;;
*) exit 2 ;;
esac
((ok)) || exit 1
