// Package config loads Mailroom's infrastructure configuration from the
// environment (ARCHITECTURE.md §5). It is loaded once at start.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	_ "time/tzdata" // TIMEZONE must resolve in minimal containers and on Windows

	"mailroom/internal/core"
)

// Defaults for optional variables.
const (
	DefaultDataDir           = "/data"
	DefaultAppSubdomain      = "newsletter"
	DefaultMailFromSubdomain = "bounce"
	DefaultConfigurationSet  = "mailroom"
	DefaultHTTPAddr          = ":8080"
	DefaultTimezone          = "UTC"

	// MinSecretLength is the minimum length of MAILROOM_SECRET in bytes.
	MinSecretLength = 32
)

// Config is the validated process configuration.
type Config struct {
	DataDir string // MAILROOM_DATA_DIR

	Domain            string // DOMAIN: the one sending domain
	AppSubdomain      string // APP_SUBDOMAIN
	BaseURL           string // BASE_URL or https://APP_SUBDOMAIN.DOMAIN; never ends in "/"
	MailFromSubdomain string // MAIL_FROM_SUBDOMAIN

	// Business settings, used only to seed the settings table on first start.
	FromName      string
	FromEmail     string
	ReplyToEmail  string
	CompanyFooter string
	OwnerEmail    string
	Timezone      string

	AWSRegion           string
	AWSAccessKeyID      string
	AWSSecretAccessKey  string // secret: never logged
	SESConfigurationSet string
	SNSTopicARN         string
	BackupBucket        string

	// Secret is the HMAC key material (MAILROOM_SECRET or <data>/secret.key).
	Secret []byte

	HTTPOnly bool
	HTTPAddr string
	// TrustedProxies (TRUSTED_PROXIES) are the reverse proxies whose
	// X-Forwarded-For header is believed for client IPs (rate limits,
	// login lockout). Empty = none: the connection's address is used.
	TrustedProxies []netip.Prefix

	LogLevel slog.Level
}

// Load reads the configuration from the process environment. See LoadFrom.
func Load() (Config, error) {
	return LoadFrom(os.Getenv)
}

// LoadFrom reads the configuration through getenv, applies defaults and
// validates it. When MAILROOM_SECRET is empty it reads <data>/secret.key,
// generating and persisting a new random key (mode 0600) on first use.
// All validation problems are reported together.
func LoadFrom(getenv func(string) string) (Config, error) {
	env := func(key, def string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return def
	}
	c := Config{
		DataDir:             env("MAILROOM_DATA_DIR", DefaultDataDir),
		Domain:              strings.ToLower(strings.TrimSuffix(env("DOMAIN", ""), ".")),
		AppSubdomain:        strings.ToLower(env("APP_SUBDOMAIN", DefaultAppSubdomain)),
		MailFromSubdomain:   strings.ToLower(env("MAIL_FROM_SUBDOMAIN", DefaultMailFromSubdomain)),
		FromName:            env("FROM_NAME", ""),
		FromEmail:           env("FROM_EMAIL", ""),
		ReplyToEmail:        env("REPLY_TO_EMAIL", ""),
		CompanyFooter:       env("COMPANY_FOOTER", ""),
		OwnerEmail:          env("OWNER_EMAIL", ""),
		Timezone:            env("TIMEZONE", DefaultTimezone),
		AWSRegion:           env("AWS_REGION", ""),
		AWSAccessKeyID:      env("AWS_ACCESS_KEY_ID", ""),
		AWSSecretAccessKey:  env("AWS_SECRET_ACCESS_KEY", ""),
		SESConfigurationSet: env("SES_CONFIGURATION_SET", DefaultConfigurationSet),
		SNSTopicARN:         env("SNS_TOPIC_ARN", ""),
		BackupBucket:        env("BACKUP_BUCKET", ""),
		HTTPAddr:            env("HTTP_ADDR", DefaultHTTPAddr),
	}

	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	httpOnly, err := parseBool(getenv("HTTP_ONLY"))
	if err != nil {
		fail("HTTP_ONLY: %v", err)
	}
	c.HTTPOnly = httpOnly

	if c.TrustedProxies, err = parsePrefixes(getenv("TRUSTED_PROXIES")); err != nil {
		fail("TRUSTED_PROXIES: %v", err)
	}

	if err := c.LogLevel.UnmarshalText([]byte(env("MAILROOM_LOG_LEVEL", "info"))); err != nil {
		fail("MAILROOM_LOG_LEVEL: must be debug, info, warn or error")
	}

	if !filepath.IsAbs(c.DataDir) && !strings.HasPrefix(c.DataDir, "/") {
		abs, err := filepath.Abs(c.DataDir)
		if err != nil {
			fail("MAILROOM_DATA_DIR: %v", err)
		}
		c.DataDir = abs
	}

	switch {
	case c.Domain == "":
		fail("DOMAIN: is required")
	case !validHostname(c.Domain) || !strings.Contains(c.Domain, "."):
		fail("DOMAIN: %q is not a valid domain name", c.Domain)
	}
	if !validHostname(c.AppSubdomain) {
		fail("APP_SUBDOMAIN: %q is not a valid DNS label", c.AppSubdomain)
	}
	if !validLabel(c.MailFromSubdomain) {
		fail("MAIL_FROM_SUBDOMAIN: %q is not a valid DNS label", c.MailFromSubdomain)
	}

	c.BaseURL, err = baseURL(getenv("BASE_URL"), c.AppSubdomain, c.Domain, c.HTTPOnly)
	if err != nil {
		fail("BASE_URL: %v", err)
	}

	for _, f := range []struct {
		name     string
		val      *string
		onDomain bool
	}{
		{"FROM_EMAIL", &c.FromEmail, true},
		{"REPLY_TO_EMAIL", &c.ReplyToEmail, true},
		{"OWNER_EMAIL", &c.OwnerEmail, false},
	} {
		if *f.val == "" {
			continue
		}
		norm, err := core.NormalizeEmail(*f.val)
		if err != nil {
			fail("%s: %q is not a valid email address", f.name, *f.val)
			continue
		}
		*f.val = norm
		if f.onDomain && c.Domain != "" && !strings.HasSuffix(norm, "@"+c.Domain) {
			fail("%s: %q must be on the sending domain %s", f.name, norm, c.Domain)
		}
	}

	if _, err := time.LoadLocation(c.Timezone); err != nil {
		fail("TIMEZONE: %q is not a known IANA time zone", c.Timezone)
	}
	if c.SNSTopicARN != "" && !validSNSTopicARN(c.SNSTopicARN) {
		fail("SNS_TOPIC_ARN: %q is not an SNS topic ARN", c.SNSTopicARN)
	}
	if (c.AWSAccessKeyID == "") != (c.AWSSecretAccessKey == "") {
		fail("AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY must be set together")
	}

	if len(errs) > 0 {
		return Config{}, fmt.Errorf("invalid configuration: %w", errors.Join(errs...))
	}

	secret, err := resolveSecret(getenv("MAILROOM_SECRET"), c.SecretKeyPath())
	if err != nil {
		return Config{}, err
	}
	c.Secret = secret
	return c, nil
}

// DBPath is the SQLite database file.
func (c Config) DBPath() string { return filepath.Join(c.DataDir, "mailroom.db") }

// MediaDir holds uploaded media files.
func (c Config) MediaDir() string { return filepath.Join(c.DataDir, "media") }

// BackupsDir holds local backups.
func (c Config) BackupsDir() string { return filepath.Join(c.DataDir, "backups") }

// AutocertDir is the ACME certificate cache.
func (c Config) AutocertDir() string { return filepath.Join(c.DataDir, "autocert") }

// SecretKeyPath is where a generated HMAC key is stored.
func (c Config) SecretKeyPath() string { return filepath.Join(c.DataDir, "secret.key") }

// BaseHost is the host (without port) of BaseURL.
func (c Config) BaseHost() string {
	u, err := url.Parse(c.BaseURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// AppHost is APP_SUBDOMAIN.DOMAIN, the public host name of the install.
func (c Config) AppHost() string { return c.AppSubdomain + "." + c.Domain }

// MailFromDomain is MAIL_FROM_SUBDOMAIN.DOMAIN, the SES custom MAIL FROM.
func (c Config) MailFromDomain() string { return c.MailFromSubdomain + "." + c.Domain }

// Location returns the configured time zone (UTC if it cannot be loaded).
func (c Config) Location() *time.Location {
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// LogValue implements slog.LogValuer so a Config can be logged without
// leaking secrets.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("data_dir", c.DataDir),
		slog.String("domain", c.Domain),
		slog.String("base_url", c.BaseURL),
		slog.String("mail_from_domain", c.MailFromDomain()),
		slog.String("aws_region", c.AWSRegion),
		slog.Bool("aws_credentials", c.AWSAccessKeyID != ""),
		slog.String("ses_configuration_set", c.SESConfigurationSet),
		slog.String("sns_topic_arn", c.SNSTopicARN),
		slog.String("backup_bucket", c.BackupBucket),
		slog.Bool("http_only", c.HTTPOnly),
		slog.String("http_addr", c.HTTPAddr),
		slog.Any("trusted_proxies", c.TrustedProxies),
		slog.String("timezone", c.Timezone),
		slog.String("log_level", c.LogLevel.String()),
	)
}

// String returns the redacted form of the configuration.
func (c Config) String() string { return c.LogValue().String() }
