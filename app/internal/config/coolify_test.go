package config

import "testing"

// TestLoadCoolifyEnv loads the environment the repository's
// docker-compose.yaml (Coolify) produces: plain HTTP on :8080 behind
// Coolify's TLS proxy, empty optional values, an https BASE_URL.
func TestLoadCoolifyEnv(t *testing.T) {
	env := map[string]string{
		"HTTP_ONLY":             "1",
		"HTTP_ADDR":             ":8080",
		"MAILROOM_DATA_DIR":     t.TempDir(),
		"TRUSTED_PROXIES":       "10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,fc00::/7",
		"BASE_URL":              "https://newsletter.example.com",
		"APP_SUBDOMAIN":         "newsletter",
		"DOMAIN":                "example.com",
		"FROM_NAME":             "Example",
		"FROM_EMAIL":            "news@example.com",
		"REPLY_TO_EMAIL":        "hello@example.com",
		"COMPANY_FOOTER":        "Example Ltd, 1 Street, City",
		"OWNER_EMAIL":           "owner@example.com",
		"TIMEZONE":              "Europe/Copenhagen",
		"MAIL_FROM_SUBDOMAIN":   "bounce",
		"AWS_REGION":            "eu-central-1",
		"AWS_ACCESS_KEY_ID":     "",
		"AWS_SECRET_ACCESS_KEY": "",
		"SNS_TOPIC_ARN":         "",
		"SES_CONFIGURATION_SET": "mailroom",
		"BACKUP_BUCKET":         "",
		"MAILROOM_LOG_LEVEL":    "info",
	}
	c, err := LoadFrom(envMap(env))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if !c.HTTPOnly || c.HTTPAddr != ":8080" {
		t.Fatalf("HTTPOnly=%v HTTPAddr=%q, want plain HTTP on :8080", c.HTTPOnly, c.HTTPAddr)
	}
	if c.BaseURL != "https://newsletter.example.com" {
		t.Fatalf("BaseURL = %q", c.BaseURL)
	}
	if len(c.TrustedProxies) != 4 {
		t.Fatalf("TrustedProxies = %v, want 4 prefixes", c.TrustedProxies)
	}

	// An empty BASE_URL falls back to https://APP_SUBDOMAIN.DOMAIN.
	env["BASE_URL"] = ""
	if c, err = LoadFrom(envMap(env)); err != nil || c.BaseURL != "https://newsletter.example.com" {
		t.Fatalf("empty BASE_URL: BaseURL=%q err=%v", c.BaseURL, err)
	}
}
