package provision

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// dnsOutput is the --json output of "provision dns".
type dnsOutput struct {
	Zone    Zone        `json:"zone"`
	Plan    Plan        `json:"plan"`
	Applied bool        `json:"applied"`
	Created []DNSRecord `json:"created"`
	Error   string      `json:"error,omitempty"`
}

func (c *CLI) provisionDNS(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	o := newOutput(args, stdout, stderr)
	fs := newFlags("provision dns", stderr)
	cloudflare := fs.Bool("cloudflare", false, "create the records through the Cloudflare API (CLOUDFLARE_API_TOKEN)")
	ipv4 := fs.String("ipv4", "", "the server's IPv4 address (adds the A record)")
	ipv6 := fs.String("ipv6", "", "the server's IPv6 address (adds the AAAA record)")
	dmarc := fs.Bool("dmarc", false, "also propose a DMARC record (p=none; reports to OWNER_EMAIL when it is on DOMAIN) if _dmarc has no TXT record")
	yes := fs.Bool("yes", false, "create the missing records; without it only the plan is printed")
	jsonOut := fs.Bool("json", false, "print machine-readable JSON")
	if code, ok := parseFlags(fs, args, o); !ok {
		return code
	}
	o.json = *jsonOut
	if !*cloudflare {
		return o.fail(errors.New("provision dns supports only --cloudflare; for other DNS providers print the records with mailroom dns records"))
	}
	token := strings.TrimSpace(getenv("CLOUDFLARE_API_TOKEN"))
	if token == "" {
		return o.fail(errors.New("CLOUDFLARE_API_TOKEN is required for this run"))
	}
	recs, env, err := c.desiredRecords(ctx, getenv, *ipv4, *ipv6, *dmarc)
	if err != nil {
		return o.fail(err)
	}
	cf := &Cloudflare{BaseURL: c.CloudflareBaseURL, Token: token, HTTP: c.HTTPClient}
	zone, err := cf.FindZone(ctx, env.Domain)
	if err != nil {
		return o.fail(err)
	}
	existing, err := cf.ListRecords(ctx, zone.ID)
	if err != nil {
		return o.fail(err)
	}
	plan := PlanDNS(existing, recs)

	out := dnsOutput{Zone: zone, Plan: plan, Created: []DNSRecord{}}
	if !*jsonOut {
		printPlan(stdout, zone, plan)
		if *ipv4 == "" && *ipv6 == "" {
			fmt.Fprintf(stdout, "\nNote: no --ipv4/--ipv6 given, so the A/AAAA record for %s.%s is not included.\n", env.AppSubdomain, env.Domain)
		}
	}
	if *yes && len(plan.Create) > 0 {
		out.Created, err = ApplyDNS(ctx, cf, zone.ID, plan)
		out.Applied = err == nil
		if err != nil {
			out.Error = err.Error()
		}
	} else if *yes {
		out.Applied = true
	}

	if *jsonOut {
		_ = writeJSON(stdout, out)
	} else {
		switch {
		case !*yes && len(plan.Create) > 0:
			fmt.Fprintf(stdout, "\nNothing was changed. Run again with --yes to create the %d missing record(s).\n", len(plan.Create))
		case !*yes:
			fmt.Fprintln(stdout, "\nNothing to create.")
		default:
			for _, r := range out.Created {
				fmt.Fprintf(stdout, "created %s %s\n", r.Type, r.Name)
			}
		}
		if len(plan.Conflicts) > 0 {
			fmt.Fprintln(stdout, "\nConflicting records were left untouched. Resolve them by hand, then run mailroom doctor.")
		}
	}
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return ExitError
	}
	if len(plan.Conflicts) > 0 {
		return ExitAttention
	}
	return ExitOK
}

func printPlan(w io.Writer, zone Zone, p Plan) {
	fmt.Fprintf(w, "Cloudflare zone %s. Existing records are never changed or deleted.\n", zone.Name)
	if len(p.Create) > 0 {
		fmt.Fprintf(w, "\nTo create (%d):\n", len(p.Create))
		for _, r := range p.Create {
			fmt.Fprintf(w, "  + %s\n", formatRecord(r))
		}
	}
	if len(p.Unchanged) > 0 {
		fmt.Fprintf(w, "\nAlready present (%d):\n", len(p.Unchanged))
		for _, m := range p.Unchanged {
			line := "  = " + formatRecord(m.Record)
			if m.Note != "" {
				line += "  (" + m.Note + ": " + m.Existing.Content + ")"
			}
			fmt.Fprintln(w, line)
		}
	}
	if len(p.Conflicts) > 0 {
		fmt.Fprintf(w, "\nConflicts, not changed (%d):\n", len(p.Conflicts))
		for _, c := range p.Conflicts {
			fmt.Fprintf(w, "  ! %s\n    %s; existing:\n", formatRecord(c.Record), c.Reason)
			for _, e := range c.Existing {
				fmt.Fprintf(w, "      %s %s %s\n", e.Type, e.Name, e.Content)
			}
		}
	}
}

func formatRecord(r Record) string {
	if r.Type == "MX" {
		return fmt.Sprintf("%s %s %d %s", r.Type, r.Name, r.Priority, r.Value)
	}
	return fmt.Sprintf("%s %s %s", r.Type, r.Name, r.Value)
}
