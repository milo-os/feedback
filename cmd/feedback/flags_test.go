package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/yaml"
)

// TestDeploymentArgsParse checks every Deployment in config/base passes only
// flags its command accepts, so a renamed or unbound flag fails here rather
// than as a crash loop in a cluster.
func TestDeploymentArgsParse(t *testing.T) {
	files, err := filepath.Glob("../../config/base/*/deployment.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no deployments found: %v", err)
	}
	envRef := regexp.MustCompile(`\$\([A-Z_]+\)`)
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var d appsv1.Deployment
		if err := yaml.Unmarshal(data, &d); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		for _, c := range d.Spec.Template.Spec.Containers {
			if len(c.Command) < 2 || filepath.Base(c.Command[0]) != "feedback" {
				continue
			}
			t.Run(d.Name+"/"+c.Name, func(t *testing.T) {
				root := newRootCommand()
				cmd, _, err := root.Find(c.Command[1:])
				if err != nil || cmd == root {
					t.Fatalf("command %v not found: %v", c.Command[1:], err)
				}
				args := make([]string, len(c.Args))
				for i, a := range c.Args {
					args[i] = envRef.ReplaceAllString(a, "x")
				}
				if err := cmd.ParseFlags(args); err != nil {
					t.Errorf("%s args %q: %v", filepath.Base(f), c.Args, err)
				}
			})
		}
	}
}

func TestCheckUserCap(t *testing.T) {
	for _, c := range []struct {
		perUser, replica int
		ok               bool
	}{
		{2, 4, true},
		{4, 4, false},
		{5, 4, false},
		{0, 4, true},
		{4, 0, true},
	} {
		if err := checkUserCap("uploads", c.perUser, c.replica); (err == nil) != c.ok {
			t.Errorf("per user %d, replica %d: %v", c.perUser, c.replica, err)
		}
	}
}

func TestAppID(t *testing.T) {
	for _, c := range []struct {
		flag int64
		env  string
		want int64
		ok   bool
	}{
		{flag: 7, env: "9", want: 7, ok: true},
		{env: "5890001\n", want: 5890001, ok: true},
		{env: " 12 ", want: 12, ok: true},
		{env: ""},
		{env: "abc"},
	} {
		t.Setenv("GITHUB_APP_ID", c.env)
		got, err := appID(c.flag)
		if got != c.want || (err == nil) != c.ok {
			t.Errorf("appID(%d) with GITHUB_APP_ID=%q = %d, %v", c.flag, c.env, got, err)
		}
	}
}

func TestLeaseConfig(t *testing.T) {
	inCluster := &rest.Config{Host: "https://kubernetes.default.svc"}
	found := func() (*rest.Config, error) { return inCluster, nil }
	missing := func() (*rest.Config, error) { return nil, rest.ErrNotInCluster }

	if cfg, err := leaseConfig(true, found); err != nil || cfg != inCluster {
		t.Errorf("leaseConfig(true, in cluster) = %v, %v", cfg, err)
	}
	if _, err := leaseConfig(true, missing); err == nil {
		t.Error("leaseConfig(true, not in cluster): want an error, not a lease in Milo")
	}
	if cfg, err := leaseConfig(false, missing); err != nil || cfg != nil {
		t.Errorf("leaseConfig(false) = %v, %v; want nothing", cfg, err)
	}
}
