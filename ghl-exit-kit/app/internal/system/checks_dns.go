package system

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strings"

	"mailroom/internal/core"
)

const hintDNSRecords = "Create the missing records (`mailroom dns records` prints them all, `mailroom provision dns --cloudflare` creates them); DNS changes can take up to an hour to be visible"

const hintNoResolver = "Wire a DNS resolver (system.NewDefaultResolver) into the system service"

// isLocalHost reports whether host needs no public DNS (dev installs).
func isLocalHost(host string) bool {
	h := strings.ToLower(host)
	return h == "localhost" || strings.HasSuffix(h, ".localhost") || net.ParseIP(h) != nil
}

// normHost lowercases a DNS name and drops the trailing dot.
func normHost(s string) string { return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".") }

func (r *doctorRun) checkDNSApp(ctx context.Context) Check {
	s := r.s
	host := s.cfg.BaseHost()
	switch {
	case host == "":
		return fail("Set DOMAIN (and APP_SUBDOMAIN or BASE_URL) in .env", "BASE_URL has no host")
	case isLocalHost(host):
		return skipped("BASE_URL points to %s, which needs no public DNS record", host)
	case s.resolver == nil:
		return skipped("no DNS resolver configured")
	}
	addrs, err := s.resolver.LookupHost(ctx, host)
	if err != nil || len(addrs) == 0 {
		return fail(fmt.Sprintf("Create an A (and AAAA) record for %s pointing to this server's public IP address", host),
			"%s has no A/AAAA record: %s", host, lookupErr(err))
	}
	slices.Sort(addrs)
	return ok("%s resolves to %s", host, strings.Join(addrs, ", "))
}

func (r *doctorRun) checkDNSDKIM(ctx context.Context) Check {
	s := r.s
	if s.resolver == nil {
		return skipped("no DNS resolver configured")
	}
	id, err := r.identity()
	if err != nil {
		return skipped("needs the DKIM tokens of the SES identity, which couldn't be read (see ses_identity)")
	}
	var tokens []string
	var zone string
	if id.DkimAttributes != nil {
		tokens = id.DkimAttributes.Tokens
		if id.DkimAttributes.SigningHostedZone != nil {
			zone = *id.DkimAttributes.SigningHostedZone
		}
	}
	// The same target zone provision prints and creates the records with.
	target := core.SESDKIMDomain(zone, s.cfg.AWSRegion)
	if len(tokens) == 0 {
		return fail("Run `mailroom provision ses`: it sets up Easy DKIM for the domain", "SES has no DKIM tokens for %s", s.cfg.Domain)
	}
	var bad []string
	for _, tok := range tokens {
		name := tok + "._domainkey." + s.cfg.Domain
		want := tok + "." + target
		got, err := s.resolver.LookupCNAME(ctx, name)
		switch {
		case err != nil:
			bad = append(bad, fmt.Sprintf("%s: missing (%s)", name, lookupErr(err)))
		case normHost(got) != want:
			bad = append(bad, fmt.Sprintf("%s points to %s, not %s", name, normHost(got), want))
		}
	}
	if len(bad) > 0 {
		return fail(hintDNSRecords, "%d of %d DKIM CNAME records are wrong: %s", len(bad), len(tokens), strings.Join(bad, "; "))
	}
	return ok("%d of %d DKIM CNAME records point to SES", len(tokens), len(tokens))
}

var rxFeedbackMX = regexp.MustCompile(`^feedback-smtp\.[a-z0-9-]+\.amazonses\.com$`)

func (r *doctorRun) checkDNSMailFrom(ctx context.Context) Check {
	s := r.s
	if s.resolver == nil {
		return skipped("no DNS resolver configured")
	}
	name := s.cfg.MailFromDomain()
	var problems []string

	wantMX := "feedback-smtp." + s.cfg.AWSRegion + ".amazonses.com"
	mxs, err := s.resolver.LookupMX(ctx, name)
	if err != nil || len(mxs) == 0 {
		problems = append(problems, fmt.Sprintf("no MX record on %s (%s)", name, lookupErr(err)))
	} else {
		found := false
		var hosts []string
		for _, mx := range mxs {
			h := normHost(mx.Host)
			hosts = append(hosts, h)
			if (s.cfg.AWSRegion != "" && h == wantMX) || (s.cfg.AWSRegion == "" && rxFeedbackMX.MatchString(h)) {
				found = true
			}
		}
		if !found {
			want := wantMX
			if s.cfg.AWSRegion == "" {
				want = "feedback-smtp.<region>.amazonses.com"
			}
			problems = append(problems, fmt.Sprintf("MX on %s is %s, not %s", name, strings.Join(hosts, ", "), want))
		}
	}

	txts, err := s.resolver.LookupTXT(ctx, name)
	spf := ""
	for _, t := range txts {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(t)), "v=spf1") {
			spf = t
		}
	}
	switch {
	case spf == "":
		problems = append(problems, fmt.Sprintf("no SPF TXT record on %s (%s)", name, lookupErr(err)))
	case !strings.Contains(strings.ToLower(spf), "include:amazonses.com"):
		problems = append(problems, fmt.Sprintf("the SPF record on %s doesn't include amazonses.com", name))
	}

	if len(problems) > 0 {
		hint := fmt.Sprintf("Create MX 10 %s and TXT \"v=spf1 include:amazonses.com ~all\" on %s", wantMX, name)
		if s.cfg.AWSRegion == "" {
			// The MX host names the SES region: without it there is no
			// value to give (an empty region would read "feedback-smtp..").
			hint = fmt.Sprintf("AWS_REGION is not set, so the MX value is unknown: run `mailroom provision ses` (it writes AWS_REGION), "+
				"then `mailroom dns records` for the MX (feedback-smtp.<region>.amazonses.com) and the TXT \"v=spf1 include:amazonses.com ~all\" on %s", name)
		}
		return fail(hint, "%s", strings.Join(problems, "; "))
	}
	return ok("%s has the SES feedback MX and an SPF record including amazonses.com", name)
}

func (r *doctorRun) checkDNSDMARC(ctx context.Context) Check {
	s := r.s
	if s.resolver == nil {
		return skipped("no DNS resolver configured")
	}
	name := "_dmarc." + s.cfg.Domain
	txts, err := s.resolver.LookupTXT(ctx, name)
	for _, t := range txts {
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(t)), "V=DMARC1") {
			return ok("DMARC record found: %s", strings.TrimSpace(t))
		}
	}
	return warn(fmt.Sprintf("Add TXT %s \"v=DMARC1; p=none; rua=mailto:<owner address>\" (Gmail and Yahoo require DMARC for bulk senders)", name),
		"no DMARC record on %s (%s)", name, lookupErr(err))
}

// lookupErr describes a DNS lookup error briefly.
func lookupErr(err error) string {
	if err == nil {
		return "no records"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		if dnsErr.IsNotFound {
			return "not found"
		}
		if dnsErr.IsTimeout {
			return "lookup timed out"
		}
	}
	return err.Error()
}
