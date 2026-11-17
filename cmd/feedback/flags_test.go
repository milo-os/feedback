package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
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
