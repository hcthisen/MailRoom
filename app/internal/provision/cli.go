package provision

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
)

// Exit codes of the command funcs.
const (
	ExitOK    = 0
	ExitError = 1
	// ExitAttention means the command ran but something needs a human:
	// DNS conflicts that were not (and will never be) changed.
	ExitAttention = 2
)

// CLI holds the injectable dependencies of the command funcs. The zero
// value uses the real AWS SDK and the Cloudflare API.
type CLI struct {
	// NewClients builds the AWS clients; nil means NewClients.
	NewClients func(ctx context.Context, getenv func(string) string) (Clients, error)
	// CloudflareBaseURL overrides DefaultCloudflareBaseURL.
	CloudflareBaseURL string
	// HTTPClient is used for Cloudflare; nil means a 30 s timeout client.
	HTTPClient *http.Client
}

var defaultCLI = &CLI{}

// ProvisionCommand runs "mailroom provision ses|dns ..." with the real AWS
// and Cloudflare clients and returns the process exit code.
func ProvisionCommand(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	return defaultCLI.Provision(ctx, args, stdout, stderr, getenv)
}

// DNSCommand runs "mailroom dns records ..." and returns the exit code.
func DNSCommand(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	return defaultCLI.DNS(ctx, args, stdout, stderr, getenv)
}

const provisionUsage = `usage:
  mailroom provision ses [--env-file FILE] [--no-backup-bucket] [--no-receive] [--rotate-keys] [--json]
  mailroom provision ses --request-production --use-case FILE [--json]
  mailroom provision dns --cloudflare [--ipv4 IP] [--ipv6 IP] [--dmarc] [--yes] [--json]

"provision ses" needs the setup user's AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY
and AWS_REGION for this run only, plus DOMAIN (and MAIL_FROM_SUBDOMAIN).
"provision dns" needs CLOUDFLARE_API_TOKEN for this run only.`

const dnsUsage = `usage:
  mailroom dns records [--ipv4 IP] [--ipv6 IP] [--dmarc] [--json]`

// Provision dispatches "provision ses" and "provision dns".
func (c *CLI) Provision(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, provisionUsage)
		return ExitError
	}
	o := newOutput(args, stdout, stderr)
	switch args[0] {
	case "ses":
		return c.provisionSES(ctx, args[1:], stdout, stderr, getenv)
	case "dns":
		return c.provisionDNS(ctx, args[1:], stdout, stderr, getenv)
	case "help", "-h", "--help":
		fmt.Fprintln(stdout, provisionUsage)
		return ExitOK
	default:
		fmt.Fprintln(stderr, provisionUsage)
		return o.fail(fmt.Errorf("unknown provision target %q", args[0]))
	}
}

// DNS dispatches "dns records".
func (c *CLI) DNS(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, dnsUsage)
		return ExitError
	}
	o := newOutput(args, stdout, stderr)
	switch args[0] {
	case "records":
		return c.dnsRecords(ctx, args[1:], stdout, stderr, getenv)
	case "help", "-h", "--help":
		fmt.Fprintln(stdout, dnsUsage)
		return ExitOK
	default:
		fmt.Fprintln(stderr, dnsUsage)
		return o.fail(fmt.Errorf("unknown dns command %q", args[0]))
	}
}

func (c *CLI) clients(ctx context.Context, getenv func(string) string) (Clients, error) {
	if c.NewClients != nil {
		return c.NewClients(ctx, getenv)
	}
	return NewClients(ctx, getenv)
}

// installEnv is what the commands read from the environment.
type installEnv struct {
	Domain, AppSubdomain, MailFromSubdomain, OwnerEmail, ConfigurationSet string
}

func loadInstallEnv(getenv func(string) string) installEnv {
	get := func(k, def string) string {
		if v := strings.TrimSpace(getenv(k)); v != "" {
			return v
		}
		return def
	}
	return installEnv{
		Domain:            normalizeDomain(get("DOMAIN", "")),
		AppSubdomain:      strings.ToLower(get("APP_SUBDOMAIN", "newsletter")),
		MailFromSubdomain: strings.ToLower(get("MAIL_FROM_SUBDOMAIN", "bounce")),
		OwnerEmail:        get("OWNER_EMAIL", ""),
		ConfigurationSet:  get("SES_CONFIGURATION_SET", DefaultConfigurationSet),
	}
}

// newFlags returns a flag set whose errors and usage go to stderr.
func newFlags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("mailroom "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// parseFlags parses args, returning an exit code when parsing ends the
// command (help or a bad flag). The flag package has already explained a
// bad flag on stderr; o repeats it as JSON when --json was asked for.
func parseFlags(fs *flag.FlagSet, args []string, o *output) (int, bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK, false
		}
		o.failJSON(err)
		return ExitError, false
	}
	if fs.NArg() > 0 {
		return o.fail(fmt.Errorf("unexpected argument %q", fs.Arg(0))), false
	}
	return 0, true
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// errorOutput is the --json document of a command that failed before it
// produced a result of its own.
type errorOutput struct {
	Error string `json:"error"`
}

// output is where a command reports: with --json every failure is also a
// JSON document on stdout, so a caller parsing stdout always gets one.
type output struct {
	stdout, stderr io.Writer
	json           bool
}

// newOutput guesses --json from the raw args, so that flag errors can be
// reported as JSON too; commands set json from the parsed flag afterwards.
func newOutput(args []string, stdout, stderr io.Writer) *output {
	return &output{stdout: stdout, stderr: stderr, json: wantsJSON(args)}
}

// wantsJSON reports whether args contain --json (or -json, --json=BOOL)
// before a "--" terminator.
func wantsJSON(args []string) bool {
	on := false
	for _, a := range args {
		if a == "--" {
			break
		}
		if !strings.HasPrefix(a, "-") {
			continue
		}
		name, val, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if name != "json" {
			continue
		}
		on = true
		if hasVal {
			b, err := strconv.ParseBool(val)
			on = err == nil && b
		}
	}
	return on
}

// fail reports err and returns ExitError.
func (o *output) fail(err error) int {
	o.failJSON(err)
	fmt.Fprintln(o.stderr, "error:", err)
	return ExitError
}

// failJSON writes err as the JSON document in --json mode.
func (o *output) failJSON(err error) {
	if o.json {
		_ = writeJSON(o.stdout, errorOutput{Error: err.Error()})
	}
}

// desiredRecords builds the install's records, reading the DKIM tokens
// from the SES identity.
func (c *CLI) desiredRecords(ctx context.Context, getenv func(string) string, ipv4, ipv6 string, dmarc bool) ([]Record, installEnv, error) {
	env := loadInstallEnv(getenv)
	in := RecordsInput{
		Domain:            env.Domain,
		AppSubdomain:      env.AppSubdomain,
		MailFromSubdomain: env.MailFromSubdomain,
		OwnerEmail:        env.OwnerEmail,
		Region:            strings.TrimSpace(getenv("AWS_REGION")),
		InboundDomain:     strings.TrimSpace(getenv("INBOUND_DOMAIN")),
		IPv4:              strings.TrimSpace(ipv4),
		IPv6:              strings.TrimSpace(ipv6),
		DMARC:             dmarc,
	}
	if err := in.Validate(); err != nil {
		return nil, env, err
	}
	cl, err := c.clients(ctx, getenv)
	if err != nil {
		return nil, env, err
	}
	in.Region = cl.Region
	ident, err := cl.SES.GetEmailIdentity(ctx, &sesv2.GetEmailIdentityInput{EmailIdentity: aws.String(env.Domain)})
	if isAPIError(err, sesNotFound...) {
		return nil, env, fmt.Errorf("the SES identity %s does not exist in %s yet: run mailroom provision ses first", env.Domain, cl.Region)
	}
	if err != nil {
		return nil, env, fmt.Errorf("ses get-email-identity %s: %w", env.Domain, err)
	}
	if ident.DkimAttributes == nil || len(ident.DkimAttributes.Tokens) == 0 {
		return nil, env, fmt.Errorf("the SES identity %s has no Easy DKIM tokens", env.Domain)
	}
	in.DKIMTokens = ident.DkimAttributes.Tokens
	in.DKIMHostedZone = aws.ToString(ident.DkimAttributes.SigningHostedZone)
	return Records(in), env, nil
}

func (c *CLI) dnsRecords(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	o := newOutput(args, stdout, stderr)
	fs := newFlags("dns records", stderr)
	ipv4 := fs.String("ipv4", "", "the server's IPv4 address (adds the A record)")
	ipv6 := fs.String("ipv6", "", "the server's IPv6 address (adds the AAAA record)")
	dmarc := fs.Bool("dmarc", false, "include a monitoring-only DMARC record (p=none; aggregate reports to OWNER_EMAIL when it is on DOMAIN)")
	jsonOut := fs.Bool("json", false, "print machine-readable JSON")
	if code, ok := parseFlags(fs, args, o); !ok {
		return code
	}
	o.json = *jsonOut
	recs, env, err := c.desiredRecords(ctx, getenv, *ipv4, *ipv6, *dmarc)
	if err != nil {
		return o.fail(err)
	}
	if *jsonOut {
		if err := writeJSON(stdout, struct {
			Records []Record `json:"records"`
		}{recs}); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return ExitError
		}
		return ExitOK
	}
	fmt.Fprintf(stdout, "DNS records for %s:\n\n", env.Domain)
	if err := WriteRecordsTable(stdout, recs); err != nil {
		return o.fail(err)
	}
	if *ipv4 == "" && *ipv6 == "" {
		fmt.Fprintf(stdout, "\nNo --ipv4/--ipv6 given: also add an A (and AAAA) record for %s.%s pointing to the server.\n", env.AppSubdomain, env.Domain)
	}
	fmt.Fprintln(stdout, "\nCreate only records that do not exist yet. If a record with the same name and type already exists with another value, ask before changing it.")
	return ExitOK
}

func readUseCase(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read the use-case file: %w", err)
	}
	return string(b), nil
}
