package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestCoolifyComposeFile pins what the Coolify deployment relies on (the
// repository's root docker-compose.yaml, deploy/coolify.md): Coolify's
// proxy terminates TLS, so Mailroom serves plain HTTP on 8080, publishes no
// host ports (other services may use them), keeps its data in a named
// volume and health-checks with the binary itself.
func TestCoolifyComposeFile(t *testing.T) {
	path := filepath.Join(appRoot, "..", "docker-compose.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("no root docker-compose.yaml (%v)", err)
	}
	var c struct {
		Services map[string]struct {
			Build struct {
				Context string `yaml:"context"`
			} `yaml:"build"`
			Ports       []string          `yaml:"ports"`
			Expose      []string          `yaml:"expose"`
			Environment map[string]string `yaml:"environment"`
			Volumes     []string          `yaml:"volumes"`
			Healthcheck struct {
				Test []string `yaml:"test"`
			} `yaml:"healthcheck"`
		} `yaml:"services"`
		Volumes map[string]any `yaml:"volumes"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, path)), &c); err != nil {
		t.Fatal(err)
	}
	svc, ok := c.Services["mailroom"]
	if !ok {
		t.Fatal("no service mailroom")
	}
	if svc.Build.Context != "./app" {
		t.Errorf("build context = %q, want ./app", svc.Build.Context)
	}
	if len(svc.Ports) != 0 {
		t.Errorf("ports = %v: the Coolify file must not publish host ports", svc.Ports)
	}
	if !slices.Contains(svc.Expose, "8080") {
		t.Errorf("expose = %v, want 8080", svc.Expose)
	}
	env := svc.Environment
	if env["HTTP_ONLY"] != "1" || env["HTTP_ADDR"] != ":8080" || env["MAILROOM_DATA_DIR"] != "/data" {
		t.Errorf("HTTP_ONLY=%q HTTP_ADDR=%q MAILROOM_DATA_DIR=%q", env["HTTP_ONLY"], env["HTTP_ADDR"], env["MAILROOM_DATA_DIR"])
	}
	for _, k := range []string{"DOMAIN", "FROM_NAME", "FROM_EMAIL", "REPLY_TO_EMAIL", "COMPANY_FOOTER", "OWNER_EMAIL",
		"TRUSTED_PROXIES", "BASE_URL", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "SNS_TOPIC_ARN",
		"MAILROOM_ADMIN_EMAIL", "MAILROOM_ADMIN_PASSWORD", "MAILROOM_BOOTSTRAP_TOKEN"} {
		if _, ok := env[k]; !ok {
			t.Errorf("environment lacks %s", k)
		}
	}
	if !slices.Contains(svc.Volumes, "mailroom-data:/data") {
		t.Errorf("volumes = %v, want mailroom-data:/data", svc.Volumes)
	}
	if _, ok := c.Volumes["mailroom-data"]; !ok {
		t.Error("named volume mailroom-data is not declared")
	}
	if !slices.Equal(svc.Healthcheck.Test, []string{"CMD", "/mailroom", "healthcheck"}) {
		t.Errorf("healthcheck = %v", svc.Healthcheck.Test)
	}
}
