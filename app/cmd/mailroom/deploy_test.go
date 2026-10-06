package main

import (
	"bufio"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"mailroom/internal/provision"
)

// The deploy files can't be exercised without Docker, so these tests pin
// what the binary and the runbook rely on: the Dockerfile builds this
// command with the version stamp and runs it as the entrypoint, and the
// compose file mounts what "provision ses" writes and health-checks with a
// command that exists.

const appRoot = "../.."

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// goVersion returns major*1000+minor of a "1.26" or "1.26.0" version.
func goVersion(t *testing.T, v string) int {
	t.Helper()
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		t.Fatalf("version %q", v)
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		t.Fatalf("version %q", v)
	}
	return major*1000 + minor
}

func TestDockerfile(t *testing.T) {
	df := readFile(t, filepath.Join(appRoot, "Dockerfile"))
	mod := readFile(t, filepath.Join(appRoot, "go.mod"))

	goLine := regexp.MustCompile(`(?m)^go (\d+\.\d+(?:\.\d+)?)$`).FindStringSubmatch(mod)
	arg := regexp.MustCompile(`(?m)^ARG GO_VERSION=(\d+\.\d+)$`).FindStringSubmatch(df)
	if goLine == nil || arg == nil {
		t.Fatalf("go.mod go line %v, Dockerfile GO_VERSION %v", goLine, arg)
	}
	if goVersion(t, arg[1]) < goVersion(t, goLine[1]) {
		t.Fatalf("the Dockerfile builds with Go %s, go.mod needs %s", arg[1], goLine[1])
	}
	for _, want := range []string{
		"FROM golang:${GO_VERSION}-alpine AS build",
		"CGO_ENABLED=0",
		"GOTOOLCHAIN=local",
		"-trimpath",
		`-ldflags "-s -w -X main.version=${VERSION}"`,
		"./cmd/mailroom",
		"vendor/modules.txt",
		"-mod=vendor",
		"go mod download",
		"FROM gcr.io/distroless/static-debian12:nonroot",
		"USER 65532:65532",
		`VOLUME ["/data"]`,
		"EXPOSE 80 443 8080",
		`ENTRYPOINT ["/mailroom"]`,
		`CMD ["serve"]`,
	} {
		if !strings.Contains(df, want) {
			t.Errorf("Dockerfile lacks %q", want)
		}
	}
	// -X main.version needs the variable it sets.
	if version != "dev" {
		t.Errorf("default version = %q", version)
	}
	if _, ok := lookup("serve"); !ok {
		t.Error("CMD serve is not a command")
	}

	// vendor/ must reach the build context.
	f, err := os.Open(filepath.Join(appRoot, ".dockerignore"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(strings.TrimPrefix(line, "/"), "vendor") || line == "*" {
			t.Errorf(".dockerignore excludes the vendored modules: %q", line)
		}
	}
}

// composeFile is the part of docker-compose.yml the runbook relies on.
type composeFile struct {
	Services map[string]struct {
		Build struct {
			Context string `yaml:"context"`
		} `yaml:"build"`
		User        string   `yaml:"user"`
		EnvFile     string   `yaml:"env_file"`
		Volumes     []string `yaml:"volumes"`
		Ports       []string `yaml:"ports"`
		Restart     string   `yaml:"restart"`
		Healthcheck struct {
			Test []string `yaml:"test"`
		} `yaml:"healthcheck"`
	} `yaml:"services"`
	Networks map[string]struct {
		EnableIPv6 bool `yaml:"enable_ipv6"`
		IPAM       struct {
			Config []struct {
				Subnet string `yaml:"subnet"`
			} `yaml:"config"`
		} `yaml:"ipam"`
	} `yaml:"networks"`
}

func TestComposeFile(t *testing.T) {
	path := filepath.Join(appRoot, "..", "deploy", "docker-compose.yml")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("no kit deploy/ next to app/ (%v)", err)
	}
	var c composeFile
	if err := yaml.Unmarshal([]byte(readFile(t, path)), &c); err != nil {
		t.Fatal(err)
	}
	svc, ok := c.Services["mailroom"]
	if !ok {
		t.Fatalf("no service mailroom: %+v", c)
	}
	// The file lives in /opt/mailroom next to app/, .env and data/.
	if svc.Build.Context != "./app" || svc.EnvFile != ".env" || svc.Restart != "unless-stopped" {
		t.Fatalf("service = %+v", svc)
	}
	for _, v := range []string{"./data:/data", "./.env:" + provision.DefaultEnvFile} {
		if !slices.Contains(svc.Volumes, v) {
			t.Errorf("volumes %v lack %q", svc.Volumes, v)
		}
	}
	for _, p := range []string{"80:80", "443:443"} {
		if !slices.Contains(svc.Ports, p) {
			t.Errorf("ports %v lack %q", svc.Ports, p)
		}
	}
	if svc.User != "${MAILROOM_UID:-65532}:${MAILROOM_GID:-65532}" {
		t.Errorf("user = %q, want the image's user by default", svc.User)
	}
	// IPv6 clients keep their own address (per-IP rate limits): the default
	// network has IPv6 with a private subnet.
	def, ok := c.Networks["default"]
	if !ok || !def.EnableIPv6 || len(def.IPAM.Config) != 1 {
		t.Fatalf("default network = %+v, want IPv6 enabled with one subnet", c.Networks)
	}
	if pfx, err := netip.ParsePrefix(def.IPAM.Config[0].Subnet); err != nil || !pfx.Addr().Is6() || !pfx.Addr().IsPrivate() {
		t.Fatalf("IPv6 subnet %q is not a private IPv6 prefix (%v)", def.IPAM.Config[0].Subnet, err)
	}
	test := svc.Healthcheck.Test
	if len(test) != 3 || test[0] != "CMD" || test[1] != "/mailroom" {
		t.Fatalf("healthcheck = %v", test)
	}
	if _, ok := lookup(test[2]); !ok {
		t.Fatalf("healthcheck runs %q, which is not a command", test[2])
	}
}
