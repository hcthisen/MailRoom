package provision

import (
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strconv"
	"strings"
	"text/tabwriter"

	"mailroom/internal/core"
)

// DefaultTTL is the TTL of every record Mailroom proposes.
const DefaultTTL = 300

// SPFValue is the SPF record of the MAIL FROM domain.
const SPFValue = "v=spf1 include:amazonses.com ~all"

// Record is one DNS record Mailroom needs.
type Record struct {
	Type     string `json:"type"`
	Name     string `json:"name"` // fully qualified, without a trailing dot
	Value    string `json:"value"`
	TTL      int    `json:"ttl"`
	Priority int    `json:"priority,omitempty"` // MX only
	Purpose  string `json:"purpose"`
}

// RecordsInput describes the install whose records Records lists.
type RecordsInput struct {
	Domain            string
	AppSubdomain      string // A/AAAA records need it together with IPv4/IPv6
	MailFromSubdomain string
	Region            string
	OwnerEmail        string // DMARC aggregate reports go here when it is on Domain
	IPv4, IPv6        string // the server's addresses; empty = no A/AAAA
	DKIMTokens        []string
	// DKIMHostedZone is the identity's DKIM signing zone as SES reports it
	// (for example "dkim.amazonses.com"); empty = derived from Region.
	DKIMHostedZone string
	DMARC          bool
}

// Validate checks the addresses and names of in.
func (in RecordsInput) Validate() error {
	var errs []error
	if err := validateDomain(normalizeDomain(in.Domain)); err != nil {
		errs = append(errs, err)
	}
	if !validLabel(in.MailFromSubdomain) {
		errs = append(errs, fmt.Errorf("MAIL_FROM_SUBDOMAIN %q is not a valid DNS label", in.MailFromSubdomain))
	}
	if in.Region == "" {
		errs = append(errs, errors.New("AWS_REGION is required"))
	}
	if in.IPv4 != "" || in.IPv6 != "" {
		for _, l := range strings.Split(in.AppSubdomain, ".") {
			if !validLabel(l) {
				errs = append(errs, fmt.Errorf("APP_SUBDOMAIN %q is not a valid host name", in.AppSubdomain))
				break
			}
		}
	}
	if in.IPv4 != "" {
		if a, err := netip.ParseAddr(in.IPv4); err != nil || !a.Is4() {
			errs = append(errs, fmt.Errorf("--ipv4 %q is not an IPv4 address", in.IPv4))
		}
	}
	if in.IPv6 != "" {
		if a, err := netip.ParseAddr(in.IPv6); err != nil || !a.Is6() || a.Is4In6() {
			errs = append(errs, fmt.Errorf("--ipv6 %q is not an IPv6 address", in.IPv6))
		}
	}
	return errors.Join(errs...)
}

// Records lists the DNS records of an install: A/AAAA for the app host
// (when addresses are given), the three Easy DKIM CNAMEs, the MAIL FROM MX
// and SPF TXT, and optionally a monitoring-only DMARC record.
func Records(in RecordsInput) []Record {
	domain := normalizeDomain(in.Domain)
	app := strings.ToLower(in.AppSubdomain) + "." + domain
	mailFrom := strings.ToLower(in.MailFromSubdomain) + "." + domain
	var out []Record
	if in.IPv4 != "" {
		out = append(out, Record{Type: "A", Name: app, Value: canonicalIP(in.IPv4), TTL: DefaultTTL, Purpose: "Mailroom web app (HTTPS, forms, unsubscribe, SES webhook)"})
	}
	if in.IPv6 != "" {
		out = append(out, Record{Type: "AAAA", Name: app, Value: canonicalIP(in.IPv6), TTL: DefaultTTL, Purpose: "Mailroom web app over IPv6"})
	}
	dkim := dkimDomain(in.DKIMHostedZone, in.Region)
	for _, t := range in.DKIMTokens {
		out = append(out, Record{Type: "CNAME", Name: t + "._domainkey." + domain, Value: t + "." + dkim, TTL: DefaultTTL, Purpose: "DKIM signing key (SES Easy DKIM)"})
	}
	out = append(out,
		Record{Type: "MX", Name: mailFrom, Value: "feedback-smtp." + in.Region + ".amazonses.com", TTL: DefaultTTL, Priority: 10, Purpose: "custom MAIL FROM: receives bounces"},
		Record{Type: "TXT", Name: mailFrom, Value: SPFValue, TTL: DefaultTTL, Purpose: "SPF for the MAIL FROM domain"},
	)
	if in.DMARC {
		out = append(out, dmarcRecord(domain, in.OwnerEmail))
	}
	return out
}

// dmarcRecord is a monitoring-only DMARC record. Aggregate reports (rua)
// go to the owner only when that address is on the domain (or one of its
// subdomains): a report address on another domain needs an authorization
// record at that domain (RFC 7489 §7.1), without which receivers send
// nothing, so the record then has no rua.
func dmarcRecord(domain, ownerEmail string) Record {
	r := Record{Type: "TXT", Name: "_dmarc." + domain, Value: "v=DMARC1; p=none", TTL: DefaultTTL,
		Purpose: "DMARC policy (monitoring only; no aggregate reports, as OWNER_EMAIL is not on the domain)"}
	if owner, err := core.NormalizeEmail(ownerEmail); err == nil {
		at := owner[strings.LastIndexByte(owner, '@')+1:]
		if at == domain || strings.HasSuffix(at, "."+domain) {
			r.Value += "; rua=mailto:" + owner
			r.Purpose = "DMARC policy (monitoring only)"
		}
	}
	return r
}

func canonicalIP(s string) string {
	if a, err := netip.ParseAddr(s); err == nil {
		return a.String()
	}
	return s
}

// dkimDomain is the zone the DKIM CNAMEs point into (core.SESDKIMDomain).
func dkimDomain(hostedZone, region string) string { return core.SESDKIMDomain(hostedZone, region) }

// WriteRecordsTable prints records as an aligned copy-paste table.
func WriteRecordsTable(w io.Writer, recs []Record) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TYPE\tNAME\tVALUE\tPRIORITY\tTTL\tPURPOSE")
	for _, r := range recs {
		prio := ""
		if r.Type == "MX" {
			prio = strconv.Itoa(r.Priority)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n", r.Type, r.Name, r.Value, prio, r.TTL, r.Purpose)
	}
	return tw.Flush()
}
