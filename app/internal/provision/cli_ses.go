package provision

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// sesOutput is the --json output of "provision ses".
type sesOutput struct {
	Result
	Error string `json:"error,omitempty"`
}

// productionOutput is the --json output of "provision ses --request-production".
type productionOutput struct {
	ProductionResult
	Error string `json:"error,omitempty"`
}

func (c *CLI) provisionSES(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	o := newOutput(args, stdout, stderr)
	fs := newFlags("provision ses", stderr)
	envFile := fs.String("env-file", DefaultEnvFile, "server env file to write the sending user's keys to")
	noBucket := fs.Bool("no-backup-bucket", false, "do not create the S3 backup bucket")
	noReceive := fs.Bool("no-receive", false, "do not set up receiving email (it is also skipped when DOMAIN already receives mail elsewhere)")
	rotate := fs.Bool("rotate-keys", false, "replace the sending user's access key (new key, env file, then delete the old one)")
	requestProd := fs.Bool("request-production", false, "submit the SES production access request instead of provisioning")
	useCase := fs.String("use-case", "", "file with the reviewed production access text (with --request-production)")
	jsonOut := fs.Bool("json", false, "print machine-readable JSON")
	if code, ok := parseFlags(fs, args, o); !ok {
		return code
	}
	o.json = *jsonOut
	if *requestProd {
		return c.requestProduction(ctx, *useCase, o, getenv)
	}
	if *useCase != "" {
		return o.fail(errors.New("--use-case is only used with --request-production"))
	}

	env := loadInstallEnv(getenv)
	if err := validateDomain(env.Domain); err != nil {
		return o.fail(err)
	}
	cl, err := c.clients(ctx, getenv)
	if err != nil {
		return o.fail(err)
	}
	in := SESInput{
		Domain:            env.Domain,
		MailFromSubdomain: env.MailFromSubdomain,
		WebhookURL:        WebhookURL(getenv("BASE_URL"), env.AppSubdomain, env.Domain),
		ConfigurationSet:  env.ConfigurationSet,
		EnvFile:           *envFile,
		BackupBucket:      !*noBucket,
		Receive:           !*noReceive,
		RotateKeys:        *rotate,
	}
	if !*jsonOut {
		fmt.Fprintf(stdout, "Provisioning SES for %s in %s\n", env.Domain, cl.Region)
		in.OnStep = func(s Step) { printStep(stdout, s) }
	}
	res, err := New(cl).ProvisionSES(ctx, in)
	if *jsonOut {
		out := sesOutput{Result: res}
		if err != nil {
			out.Error = err.Error()
		}
		if werr := writeJSON(stdout, out); werr != nil && err == nil {
			err = werr
		}
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return ExitError
		}
		return ExitOK
	}
	if err != nil {
		return o.fail(err)
	}
	printSESSummary(stdout, res, env)
	return ExitOK
}

func printStep(w io.Writer, s Step) {
	fmt.Fprintf(w, "  %-10s %-40s %s\n", s.Action, s.Name, s.Detail)
}

func printSESSummary(w io.Writer, res Result, env installEnv) {
	fmt.Fprintf(w, "\nDone. Sending user %s uses access key %s; its secret is only in %s.\n", res.IAMUser, res.AccessKeyID, res.EnvFile)
	fmt.Fprintf(w, "SNS topic:         %s\n", res.TopicARN)
	fmt.Fprintf(w, "Configuration set: %s\n", res.ConfigurationSet)
	if res.BackupBucket != "" {
		fmt.Fprintf(w, "Backup bucket:     %s\n", res.BackupBucket)
	}
	if res.InboundDomain != "" {
		fmt.Fprintf(w, "Receiving:         on, mail to *@%s is stored in %s and shown in Mailroom's Inbox\n", res.InboundDomain, res.InboundBucket)
		fmt.Fprintf(w, "                   once the MX record below exists (it sends all mail for %s to Mailroom).\n", res.InboundDomain)
	} else {
		fmt.Fprintln(w, "Receiving:         off")
	}
	fmt.Fprintf(w, "\nDNS records needed:\n\n")
	_ = WriteRecordsTable(w, res.Records)
	fmt.Fprintf(w, "\nAlso add an A (and AAAA) record for %s.%s pointing to the server: see mailroom dns records --ipv4 IP.\n", env.AppSubdomain, env.Domain)
	fmt.Fprintf(w, "Create them with mailroom provision dns --cloudflare --ipv4 IP [--ipv6 IP], or by hand.\n\n")
	if res.IdentityVerified {
		fmt.Fprintln(w, "Identity verified:  yes")
	} else {
		fmt.Fprintf(w, "Identity verified:  not yet (DKIM %s, MAIL FROM %s); SES verifies it after the DNS records resolve.\n",
			orUnknown(res.DKIMStatus), orUnknown(res.MailFromStatus))
	}
	if res.ProductionAccessEnabled {
		fmt.Fprintln(w, "Production access:  enabled")
	} else {
		fmt.Fprintln(w, "Production access:  no, the account is in the SES sandbox.")
		fmt.Fprintln(w, "                    Request it with: mailroom provision ses --request-production --use-case FILE")
	}
	for _, warning := range res.Warnings {
		fmt.Fprintf(w, "Warning: %s.\n", warning)
	}
	fmt.Fprintln(w, RecreateHint)
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func (c *CLI) requestProduction(ctx context.Context, useCaseFile string, o *output, getenv func(string) string) int {
	stdout, stderr, jsonOut := o.stdout, o.stderr, o.json
	if useCaseFile == "" {
		return o.fail(errors.New("--request-production needs --use-case FILE with the reviewed text"))
	}
	text, err := readUseCase(useCaseFile)
	if err != nil {
		return o.fail(err)
	}
	env := loadInstallEnv(getenv)
	in := ProductionInput{Domain: env.Domain, OwnerEmail: env.OwnerEmail, UseCase: text}
	// Validate before touching AWS so a bad text never needs credentials.
	if _, err := validateUseCase(in.UseCase); err != nil {
		return o.fail(err)
	}
	cl, err := c.clients(ctx, getenv)
	if err != nil {
		return o.fail(err)
	}
	res, err := New(cl).RequestProduction(ctx, in)
	if jsonOut {
		out := productionOutput{ProductionResult: res}
		if err != nil {
			out.Error = err.Error()
		}
		_ = writeJSON(stdout, out)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return ExitError
		}
		return ExitOK
	}
	if err != nil {
		return o.fail(err)
	}
	switch {
	case !res.Submitted:
		fmt.Fprintln(stdout, "Production access is already enabled; nothing was submitted.")
	default:
		fmt.Fprintln(stdout, "The SES production access request was submitted.")
		if res.ReviewStatus != "" {
			fmt.Fprintf(stdout, "Review status: %s", res.ReviewStatus)
			if res.CaseID != "" {
				fmt.Fprintf(stdout, " (case %s)", res.CaseID)
			}
			fmt.Fprintln(stdout)
		}
		fmt.Fprintln(stdout, "AWS answers by email, usually within 24 hours; reply to any questions they ask.")
	}
	return ExitOK
}
