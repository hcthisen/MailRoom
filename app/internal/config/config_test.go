package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// envMap returns a getenv backed by m.
func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func baseEnv(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"MAILROOM_DATA_DIR": t.TempDir(),
		"DOMAIN":            "example.com",
	}
}

func TestLoadDefaults(t *testing.T) {
	env := baseEnv(t)
	c, err := LoadFrom(envMap(env))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	checks := []struct{ name, got, want string }{
		{"BaseURL", c.BaseURL, "https://newsletter.example.com"},
		{"AppSubdomain", c.AppSubdomain, DefaultAppSubdomain},
		{"MailFromDomain", c.MailFromDomain(), "bounce.example.com"},
		{"AppHost", c.AppHost(), "newsletter.example.com"},
		{"BaseHost", c.BaseHost(), "newsletter.example.com"},
		{"SESConfigurationSet", c.SESConfigurationSet, "mailroom"},
		{"HTTPAddr", c.HTTPAddr, ":8080"},
		{"Timezone", c.Timezone, "UTC"},
		{"DBPath", c.DBPath(), filepath.Join(env["MAILROOM_DATA_DIR"], "mailroom.db")},
		{"AutocertDir", c.AutocertDir(), filepath.Join(env["MAILROOM_DATA_DIR"], "autocert")},
	}
	for _, ch := range checks {
		if ch.got != ch.want {
			t.Errorf("%s = %q, want %q", ch.name, ch.got, ch.want)
		}
	}
	if c.HTTPOnly {
		t.Error("HTTPOnly should default to false")
	}
	if len(c.TrustedProxies) != 0 {
		t.Errorf("TrustedProxies should default to none, got %v", c.TrustedProxies)
	}
	if c.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want info", c.LogLevel)
	}
	if len(c.Secret) < MinSecretLength {
		t.Errorf("Secret too short: %d", len(c.Secret))
	}
}

func TestLoadOverrides(t *testing.T) {
	env := baseEnv(t)
	env["BASE_URL"] = "http://LOCALHOST:8080/"
	env["HTTP_ONLY"] = "1"
	env["HTTP_ADDR"] = "127.0.0.1:9000"
	env["APP_SUBDOMAIN"] = "mail"
	env["MAIL_FROM_SUBDOMAIN"] = "mf"
	env["FROM_EMAIL"] = " News@Example.com "
	env["REPLY_TO_EMAIL"] = "hello@example.com"
	env["OWNER_EMAIL"] = "owner@gmail.com"
	env["TIMEZONE"] = "Europe/Copenhagen"
	env["MAILROOM_LOG_LEVEL"] = "debug"
	env["MAILROOM_SECRET"] = strings.Repeat("k", 40)
	env["SNS_TOPIC_ARN"] = "arn:aws:sns:eu-west-1:123456789012:mailroom"

	c, err := LoadFrom(envMap(env))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if c.BaseURL != "http://localhost:8080" {
		t.Errorf("BaseURL = %q", c.BaseURL)
	}
	if c.BaseHost() != "localhost" {
		t.Errorf("BaseHost = %q", c.BaseHost())
	}
	if !c.HTTPOnly || c.HTTPAddr != "127.0.0.1:9000" {
		t.Errorf("HTTPOnly/HTTPAddr = %v/%q", c.HTTPOnly, c.HTTPAddr)
	}
	if c.FromEmail != "news@example.com" {
		t.Errorf("FromEmail = %q", c.FromEmail)
	}
	if c.MailFromDomain() != "mf.example.com" {
		t.Errorf("MailFromDomain = %q", c.MailFromDomain())
	}
	if c.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %v", c.LogLevel)
	}
	if string(c.Secret) != env["MAILROOM_SECRET"] {
		t.Error("Secret should come from MAILROOM_SECRET")
	}
	if _, err := os.Stat(c.SecretKeyPath()); !os.IsNotExist(err) {
		t.Error("secret.key must not be written when MAILROOM_SECRET is set")
	}
	if c.Location().String() != "Europe/Copenhagen" {
		t.Errorf("Location = %v", c.Location())
	}
}

func TestLoadValidation(t *testing.T) {
	tests := []struct {
		name    string
		set     map[string]string
		wantErr string
	}{
		{"missing domain", map[string]string{"DOMAIN": ""}, "DOMAIN: is required"},
		{"bad domain", map[string]string{"DOMAIN": "exa mple.com"}, "DOMAIN"},
		{"single label domain", map[string]string{"DOMAIN": "localhost"}, "DOMAIN"},
		{"bad subdomain", map[string]string{"APP_SUBDOMAIN": "-news"}, "APP_SUBDOMAIN"},
		{"bad mail from", map[string]string{"MAIL_FROM_SUBDOMAIN": "a.b"}, "MAIL_FROM_SUBDOMAIN"},
		{"base url path", map[string]string{"BASE_URL": "https://x.example.com/app"}, "BASE_URL"},
		{"base url relative", map[string]string{"BASE_URL": "x.example.com"}, "BASE_URL"},
		{"http base url without http only", map[string]string{"BASE_URL": "http://localhost:8080"}, "HTTP_ONLY=1"},
		{"from off domain", map[string]string{"FROM_EMAIL": "news@other.com"}, "FROM_EMAIL"},
		{"reply-to off domain", map[string]string{"REPLY_TO_EMAIL": "hi@sub.example.com"}, "REPLY_TO_EMAIL"},
		{"bad owner", map[string]string{"OWNER_EMAIL": "owner"}, "OWNER_EMAIL"},
		{"bad timezone", map[string]string{"TIMEZONE": "Mars/Olympus"}, "TIMEZONE"},
		{"bad http only", map[string]string{"HTTP_ONLY": "maybe"}, "HTTP_ONLY"},
		{"bad log level", map[string]string{"MAILROOM_LOG_LEVEL": "loud"}, "MAILROOM_LOG_LEVEL"},
		{"bad topic", map[string]string{"SNS_TOPIC_ARN": "mailroom"}, "SNS_TOPIC_ARN"},
		{"half aws keys", map[string]string{"AWS_ACCESS_KEY_ID": "AKIA123"}, "AWS_SECRET_ACCESS_KEY"},
		{"short secret", map[string]string{"MAILROOM_SECRET": "short"}, "MAILROOM_SECRET"},
		{"bad trusted proxy", map[string]string{"TRUSTED_PROXIES": "10.0.0.0/8, proxy.local"}, "TRUSTED_PROXIES"},
		{"bad trusted prefix", map[string]string{"TRUSTED_PROXIES": "10.0.0.0/33"}, "TRUSTED_PROXIES"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := baseEnv(t)
			for k, v := range tt.set {
				env[k] = v
			}
			_, err := LoadFrom(envMap(env))
			if err == nil {
				t.Fatalf("want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadReportsAllErrors(t *testing.T) {
	env := baseEnv(t)
	env["DOMAIN"] = ""
	env["TIMEZONE"] = "Nowhere/Land"
	_, err := LoadFrom(envMap(env))
	if err == nil || !strings.Contains(err.Error(), "DOMAIN") || !strings.Contains(err.Error(), "TIMEZONE") {
		t.Fatalf("want both errors, got %v", err)
	}
}

func TestSecretPersistence(t *testing.T) {
	env := baseEnv(t)
	c1, err := LoadFrom(envMap(env))
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	info, err := os.Stat(c1.SecretKeyPath())
	if err != nil {
		t.Fatalf("secret.key not written: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("secret.key mode = %v, want 0600", info.Mode().Perm())
	}
	c2, err := LoadFrom(envMap(env))
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if string(c1.Secret) != string(c2.Secret) {
		t.Fatal("secret must be stable across loads")
	}

	// A fresh data dir gets a different key.
	env2 := baseEnv(t)
	c3, err := LoadFrom(envMap(env2))
	if err != nil {
		t.Fatalf("third load: %v", err)
	}
	if string(c3.Secret) == string(c1.Secret) {
		t.Fatal("two installs must not share a generated secret")
	}
}

func TestSecretFileTooShort(t *testing.T) {
	env := baseEnv(t)
	if err := os.WriteFile(filepath.Join(env["MAILROOM_DATA_DIR"], "secret.key"), []byte("abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFrom(envMap(env)); err == nil {
		t.Fatal("want error for a short secret.key")
	}
}

func TestLogValueRedactsSecrets(t *testing.T) {
	env := baseEnv(t)
	env["AWS_ACCESS_KEY_ID"] = "AKIAEXAMPLE"
	env["AWS_SECRET_ACCESS_KEY"] = "super-secret-aws-key"
	env["MAILROOM_SECRET"] = strings.Repeat("s", 40)
	c, err := LoadFrom(envMap(env))
	if err != nil {
		t.Fatal(err)
	}
	s := c.String()
	for _, secret := range []string{"super-secret-aws-key", strings.Repeat("s", 40), "AKIAEXAMPLE"} {
		if strings.Contains(s, secret) {
			t.Fatalf("String() leaks %q: %s", secret, s)
		}
	}
}

func TestTrustedProxies(t *testing.T) {
	env := baseEnv(t)
	env["HTTP_ONLY"] = "1" // HTTP_ONLY alone must not trust X-Forwarded-For
	c, err := LoadFrom(envMap(env))
	if err != nil || len(c.TrustedProxies) != 0 {
		t.Fatalf("HTTP_ONLY=1 = %v, %v", c.TrustedProxies, err)
	}
	env["TRUSTED_PROXIES"] = " 172.16.0.0/12, 127.0.0.1 ,::1, 10.1.2.3/8 ,"
	c, err = LoadFrom(envMap(env))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range c.TrustedProxies {
		got = append(got, p.String())
	}
	if want := "172.16.0.0/12 127.0.0.1/32 ::1/128 10.0.0.0/8"; strings.Join(got, " ") != want {
		t.Fatalf("TrustedProxies = %v, want %s", got, want)
	}
}
