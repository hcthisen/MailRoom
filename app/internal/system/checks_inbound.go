package system

import (
	"context"
	"fmt"
	"strings"
	"time"

	"mailroom/internal/db"
)

// inboundStuckAfter is how long a received message may wait for the
// fetcher before the doctor warns.
const inboundStuckAfter = 15 * time.Minute

// checkInbound checks receiving (N-1), when it is set up: DOMAIN's MX
// record points to SES receiving, and received mail is being fetched (no
// message pending for long, none failed).
func (r *doctorRun) checkInbound(ctx context.Context) Check {
	s := r.s
	if !s.cfg.Receiving() {
		return skipped("receiving email is not set up (INBOUND_BUCKET and INBOUND_DOMAIN are empty)")
	}
	var problems, warnings []string
	hint := ""
	want := "inbound-smtp." + s.cfg.AWSRegion + ".amazonaws.com"
	if s.resolver != nil {
		mxs, err := s.resolver.LookupMX(ctx, s.cfg.InboundDomain)
		found := false
		var hosts []string
		for _, mx := range mxs {
			h := normHost(mx.Host)
			hosts = append(hosts, h)
			if h == want {
				found = true
			}
		}
		switch {
		case err != nil || len(mxs) == 0:
			problems = append(problems, fmt.Sprintf("no MX record on %s (%s)", s.cfg.InboundDomain, lookupErr(err)))
			hint = fmt.Sprintf("Create MX 10 %s on %s (mailroom dns records lists it)", want, s.cfg.InboundDomain)
		case !found:
			problems = append(problems, fmt.Sprintf("the MX of %s is %s, not %s, so mail goes elsewhere", s.cfg.InboundDomain, strings.Join(hosts, ", "), want))
			hint = fmt.Sprintf("Point the MX record of %s to %s, or turn receiving off with mailroom provision ses --no-receive", s.cfg.InboundDomain, want)
		}
	}
	if s.db != nil {
		var stuck, failed int
		err := s.db.Read.QueryRowContext(ctx, `SELECT
			coalesce(sum(status = 'pending' AND created_at < ?), 0), coalesce(sum(status = 'failed'), 0) FROM inbound_messages`,
			db.FormatTime(r.now.Add(-inboundStuckAfter))).Scan(&stuck, &failed)
		if err != nil {
			problems = append(problems, "the inbox could not be read: "+err.Error())
		}
		if stuck > 0 {
			warnings = append(warnings, fmt.Sprintf("%d received emails have waited over 15 minutes to be fetched from %s", stuck, s.cfg.InboundBucket))
			if hint == "" {
				hint = "Check the logs for inbound: errors; the sending user needs s3:GetObject on the inbound bucket (run mailroom provision ses again)"
			}
		}
		if failed > 0 {
			warnings = append(warnings, fmt.Sprintf("%d received emails could not be fetched (Inbox → All)", failed))
		}
	}
	switch {
	case len(problems) > 0:
		return fail(hint, "%s", strings.Join(append(problems, warnings...), "; "))
	case len(warnings) > 0:
		return warn(hint, "%s", strings.Join(warnings, "; "))
	}
	return ok("mail to %s reaches Mailroom's inbox (MX → %s)", s.cfg.InboundDomain, want)
}
