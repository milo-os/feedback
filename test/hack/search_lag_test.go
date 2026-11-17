// Package hack tests the scripts under hack/.
package hack

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// stubKubectl stands in for kubectl against one post. It keeps the post's
// spec.hidden in $STUB/hidden ("absent" when unset), logs each merge patch to
// $STUB/patches and answers ResourceSearchQuery creates according to
// $STUB/mode:
//
//	ok                found unless hidden (the index catches up at once)
//	missing           never found
//	stuck             always found, so hiding never takes effect
//	error             the query fails
//	error-after-hide  the query fails while the post is hidden
//	denied            the posts index isn't ready
const stubKubectl = `#!/usr/bin/env bash
set -euo pipefail
hidden=$(cat "$STUB/hidden")
case "$1" in
get)
	if [[ $hidden == absent ]]; then spec='{}'; else spec="{\"hidden\":$hidden}"; fi
	echo "{\"metadata\":{\"name\":\"$3\"},\"spec\":$spec,\"status\":{\"title\":\"Wildcard hostnames\"}}" ;;
patch)
	p=$6
	echo "$p" >>"$STUB/patches"
	h=$(jq -c '.spec.hidden' <<<"$p")
	if [[ $h == null ]]; then echo absent >"$STUB/hidden"; else echo "$h" >"$STUB/hidden"; fi
	echo '{}' ;;
create)
	cat >/dev/null
	mode=$(cat "$STUB/mode")
	found=false
	case $mode in
	ok) [[ $hidden == true ]] || found=true ;;
	missing) ;;
	stuck) found=true ;;
	error) exit 1 ;;
	error-after-hide) [[ $hidden != true ]] || exit 1; found=true ;;
	denied) echo '{"status":{"deniedTargetResources":[{"kind":"Post"}]}}'; exit 0 ;;
	esac
	results='[]'
	$found && results='[{"resource":{"metadata":{"name":"a.b.1"}}}]'
	echo "{\"status\":{\"results\":$results}}" ;;
*) echo "unexpected kubectl $*" >&2; exit 1 ;;
esac
`

type run struct {
	stub string
	cmd  *exec.Cmd
	out  strings.Builder
}

func newRun(t *testing.T, hidden, mode, timeout string) *run {
	t.Helper()
	for _, bin := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not found", bin)
		}
	}
	stub := t.TempDir()
	bin := filepath.Join(stub, "bin")
	must(t, os.Mkdir(bin, 0o755))
	must(t, os.WriteFile(filepath.Join(bin, "kubectl"), []byte(stubKubectl), 0o755))
	must(t, os.WriteFile(filepath.Join(stub, "hidden"), []byte(hidden+"\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(stub, "mode"), []byte(mode+"\n"), 0o644))

	r := &run{stub: stub}
	r.cmd = exec.Command("bash", "../../hack/search-lag.sh", "a.b.1", timeout)
	r.cmd.Env = append(os.Environ(), "STUB="+stub, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	r.cmd.Stdout = &r.out
	r.cmd.Stderr = &r.out
	return r
}

func (r *run) exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}

func (r *run) patches() []string {
	data, _ := os.ReadFile(filepath.Join(r.stub, "patches"))
	return strings.Fields(string(data))
}

func (r *run) hidden() string {
	data, _ := os.ReadFile(filepath.Join(r.stub, "hidden"))
	return strings.TrimSpace(string(data))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestSearchLag(t *testing.T) {
	const hide = `{"spec":{"hidden":true}}`
	for _, c := range []struct {
		name, hidden, mode string
		wantCode           int
		wantPatches        []string
	}{
		{"hidden unset", "absent", "ok", 0, []string{hide, `{"spec":{"hidden":null}}`}},
		{"hidden false", "false", "ok", 0, []string{hide, `{"spec":{"hidden":false}}`}},
		{"already hidden", "true", "ok", 1, nil},
		{"not indexed", "absent", "missing", 1, nil},
		{"search fails at the start", "false", "error", 1, nil},
		{"index not ready", "false", "denied", 1, nil},
		{"never leaves the index", "absent", "stuck", 1, []string{hide, `{"spec":{"hidden":null}}`}},
		{"search fails while hidden", "false", "error-after-hide", 1, []string{hide, `{"spec":{"hidden":false}}`}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRun(t, c.hidden, c.mode, "2")
			code := r.exitCode(r.cmd.Run())
			if code != c.wantCode {
				t.Errorf("exit %d, want %d\n%s", code, c.wantCode, r.out.String())
			}
			if got := r.patches(); !slices.Equal(got, c.wantPatches) {
				t.Errorf("patches %v, want %v\n%s", got, c.wantPatches, r.out.String())
			}
			if got := r.hidden(); got != c.hidden {
				t.Errorf("spec.hidden ends as %s, want the original %s", got, c.hidden)
			}
		})
	}

	t.Run("interrupted while hidden", func(t *testing.T) {
		r := newRun(t, "false", "stuck", "60")
		must(t, r.cmd.Start())
		deadline := time.Now().Add(10 * time.Second)
		for len(r.patches()) == 0 && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		must(t, r.cmd.Process.Signal(syscall.SIGTERM))
		if code := r.exitCode(r.cmd.Wait()); code != 143 {
			t.Errorf("exit %d, want 143\n%s", code, r.out.String())
		}
		if got := r.hidden(); got != "false" {
			t.Errorf("spec.hidden ends as %s, want false\n%s", got, r.out.String())
		}
	})
}
