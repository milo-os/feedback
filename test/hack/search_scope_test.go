package hack

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// scopeKubectl stands in for kubectl against a platform root and a user
// control plane, told apart by $KUBECONFIG. $STUB/discovery sets how the user
// context answers discovery:
//
//	served   lists resourcesearchqueries
//	empty    succeeds without it
//	fail     fails, as a TLS or authentication error does
//	partial  fails for another group but still lists resourcesearchqueries
//
// $STUB/platform and $STUB/user set each context's query: found, missing or
// error.
const scopeKubectl = `#!/usr/bin/env bash
set -euo pipefail
ctx=$KUBECONFIG
case "$1" in
config) echo "$STUB_SERVER" ;;
get) echo "{\"metadata\":{\"name\":\"$3\"},\"spec\":{},\"status\":{\"title\":\"Wildcard hostnames\"}}" ;;
api-resources)
	[[ $ctx == user ]] || { echo "unexpected discovery in $ctx" >&2; exit 1; }
	case $(cat "$STUB/discovery") in
	served) echo resourcesearchqueries.search.miloapis.com ;;
	empty) ;;
	fail) echo 'Unable to connect to the server: tls: failed to verify certificate' >&2; exit 1 ;;
	partial)
		echo 'error: unable to retrieve the complete list of server APIs: example.miloapis.com/v1: the server is currently unable to handle the request' >&2
		echo resourcesearchqueries.search.miloapis.com
		exit 1 ;;
	esac ;;
create)
	cat >/dev/null
	results='[]'
	case $(cat "$STUB/$ctx") in
	found) results='[{"resource":{"metadata":{"name":"a.b.1"}}}]' ;;
	missing) ;;
	error) echo 'Error from server (Forbidden)' >&2; exit 1 ;;
	esac
	echo "{\"status\":{\"results\":$results}}" ;;
*) echo "unexpected kubectl $*" >&2; exit 1 ;;
esac
`

const userServer = "https://milo.example/apis/iam.miloapis.com/v1alpha1/users/u1/control-plane"

func runScope(t *testing.T, server, discovery, platform, user string) (int, string) {
	t.Helper()
	for _, bin := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not found", bin)
		}
	}
	stub := t.TempDir()
	bin := filepath.Join(stub, "bin")
	must(t, os.Mkdir(bin, 0o755))
	must(t, os.WriteFile(filepath.Join(bin, "kubectl"), []byte(scopeKubectl), 0o755))
	for name, v := range map[string]string{"discovery": discovery, "platform": platform, "user": user} {
		must(t, os.WriteFile(filepath.Join(stub, name), []byte(v+"\n"), 0o644))
	}

	cmd := exec.Command("bash", "../../hack/search-scope.sh", "a.b.1")
	cmd.Env = append(os.Environ(),
		"STUB="+stub, "STUB_SERVER="+server,
		"PLATFORM_KUBECONFIG=platform", "USER_KUBECONFIG=user",
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	r := &run{}
	return r.exitCode(cmd.Run()), out.String()
}

func TestSearchScope(t *testing.T) {
	for _, c := range []struct {
		name, server, discovery, platform, user string
		wantCode                                int
		wantOut                                 string
	}{
		{"both contexts find the post", userServer, "served", "found", "found", 0, "user context: finds a.b.1"},
		{"another group fails discovery", userServer, "partial", "found", "found", 0, "user context: finds a.b.1"},
		{"user discovery fails", userServer, "fail", "found", "found", 2, "discovery failed"},
		{"user context has no search API", userServer, "empty", "found", "found", 1, "user context: no search API"},
		{"user context doesn't find the post", userServer, "served", "found", "missing", 1, "user context: doesn't find a.b.1"},
		{"platform root doesn't find the post", userServer, "served", "missing", "found", 1, "platform root: doesn't find a.b.1"},
		{"user query fails", userServer, "served", "found", "error", 2, "Forbidden"},
		{"platform query fails", userServer, "served", "error", "found", 2, "Forbidden"},
		{"not a user control plane", "https://milo.example", "served", "found", "found", 2, "isn't a user control plane"},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, out := runScope(t, c.server, c.discovery, c.platform, c.user)
			if code != c.wantCode {
				t.Errorf("exit %d, want %d\n%s", code, c.wantCode, out)
			}
			if !strings.Contains(out, c.wantOut) {
				t.Errorf("output doesn't contain %q\n%s", c.wantOut, out)
			}
		})
	}
}
