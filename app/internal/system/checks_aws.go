package system

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"
	"github.com/aws/aws-sdk-go-v2/service/sns"
)

// errAWSNotConfigured is the error of AWS calls without AWS configuration.
var errAWSNotConfigured = errors.New("AWS is not configured (AWS_REGION, AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY are empty)")

const hintAWSNotConfigured = "Run `mailroom provision ses`: it writes AWS_REGION and the sending user's keys (and SNS_TOPIC_ARN) to .env; then recreate the container with `sudo docker compose up -d` (a plain restart keeps the old environment)"

// awsFail reports a failed AWS call, with the not-configured hint when
// that is the cause.
func awsFail(call, permission string, err error) Check {
	if errors.Is(err, errAWSNotConfigured) {
		return fail(hintAWSNotConfigured, "%s", err)
	}
	return fail(fmt.Sprintf("Check the network and that the sending user may call %s (re-run `mailroom provision ses` to restore its policy)", permission),
		"%s failed: %s", call, err)
}

func (r *doctorRun) checkSESIdentity(context.Context) Check {
	s := r.s
	id, err := r.identity()
	if err != nil {
		return awsFail("SES GetEmailIdentity for "+s.cfg.Domain, "ses:GetEmailIdentity", err)
	}
	status := StatusOK
	var problems []string
	hint := "Run `mailroom doctor` again after the DNS checks pass; if SES still reports a failure, re-run `mailroom provision ses`"

	if !id.VerifiedForSendingStatus {
		status = StatusFail
		problems = append(problems, "the identity is not verified for sending")
	}
	dkim := types.DkimStatusNotStarted
	if id.DkimAttributes != nil {
		dkim = id.DkimAttributes.Status
	}
	switch dkim {
	case types.DkimStatusSuccess:
	case types.DkimStatusPending:
		status = worse(status, StatusWarn)
		problems = append(problems, "DKIM verification is pending (SES checks the CNAME records for up to 72 hours)")
	default:
		status = StatusFail
		problems = append(problems, fmt.Sprintf("DKIM status is %s", dkim))
		hint = hintDNSRecords + "; then SES verifies DKIM automatically"
	}
	wantMailFrom := s.cfg.MailFromDomain()
	mf := id.MailFromAttributes
	switch {
	case mf == nil || mf.MailFromDomain == nil || normHost(*mf.MailFromDomain) != wantMailFrom:
		status = StatusFail
		problems = append(problems, fmt.Sprintf("the custom MAIL FROM domain is not %s", wantMailFrom))
		hint = "Re-run `mailroom provision ses` to set the custom MAIL FROM domain"
	case mf.MailFromDomainStatus == types.MailFromDomainStatusSuccess:
	case mf.MailFromDomainStatus == types.MailFromDomainStatusPending:
		status = worse(status, StatusWarn)
		problems = append(problems, "MAIL FROM verification is pending")
	default:
		status = StatusFail
		problems = append(problems, fmt.Sprintf("MAIL FROM status is %s", mf.MailFromDomainStatus))
		hint = hintDNSRecords + "; SES re-checks the MAIL FROM MX record automatically"
	}
	if status == StatusOK {
		return ok("%s is verified for sending, DKIM and MAIL FROM (%s) succeeded", s.cfg.Domain, wantMailFrom)
	}
	return Check{Status: status, Message: s.cfg.Domain + ": " + strings.Join(problems, "; "), FixHint: hint}
}

func (r *doctorRun) checkSESProduction(context.Context) Check {
	acc, err := r.account()
	if err != nil {
		return awsFail("SES GetAccount", "ses:GetAccount", err)
	}
	if acc.ProductionAccessEnabled {
		return ok("the SES account has production access")
	}
	msg := "the SES account is in the sandbox: it can only send to verified addresses, at most 200 emails a day"
	if acc.Details != nil && acc.Details.ReviewDetails != nil && acc.Details.ReviewDetails.Status != "" {
		msg += fmt.Sprintf(" (production access request: %s)", acc.Details.ReviewDetails.Status)
	}
	return warn("Request production access with `mailroom provision ses --request-production --use-case <file>` and answer AWS's email", "%s", msg)
}

func (r *doctorRun) checkSESSending(context.Context) Check {
	acc, err := r.account()
	if err != nil {
		return awsFail("SES GetAccount", "ses:GetAccount", err)
	}
	if !acc.SendingEnabled {
		return fail("Open the SES console (Reputation metrics / Account dashboard) to see why AWS paused sending, fix the cause and ask AWS to resume",
			"sending is paused for the SES account")
	}
	enforcement := ""
	if acc.EnforcementStatus != nil {
		enforcement = strings.ToUpper(*acc.EnforcementStatus)
	}
	switch enforcement {
	case "SHUTDOWN":
		return fail("AWS shut down sending for the account; see the SES console and contact AWS support", "the SES account enforcement status is SHUTDOWN")
	case "PROBATION":
		return warn("Bounce or complaint rates are too high: clean the list and check the SES reputation dashboard", "the SES account is on probation")
	}
	var sent, max24, rate float64
	if q := acc.SendQuota; q != nil {
		sent, max24, rate = q.SentLast24Hours, q.Max24HourSend, q.MaxSendRate
	}
	if max24 <= 0 { // -1 means unlimited
		return ok("sending is enabled; %.0f sent in the last 24 h (no daily limit), max %.0f/s", sent, rate)
	}
	used := sent / max24
	msg := fmt.Sprintf("sending is enabled; %.0f of %.0f sent in the last 24 h (%.1f %%), max %.0f/s", sent, max24, used*100, rate)
	switch {
	case used >= 1:
		return fail("The daily quota is used up; sending resumes as the 24 h window moves on. Request a higher quota in the SES console", "%s", msg)
	case used >= 0.9:
		return warn("Request a higher sending quota in the SES console before the next large broadcast", "%s", msg)
	}
	return ok("%s", msg)
}

// maxSubscriptionPages bounds the SNS listing (100 subscriptions a page).
const maxSubscriptionPages = 50

func (r *doctorRun) checkSNSSubscription(ctx context.Context) Check {
	s := r.s
	if s.sns == nil {
		return fail(hintAWSNotConfigured, "%s", errAWSNotConfigured)
	}
	topic := s.cfg.SNSTopicARN
	if topic == "" {
		return fail("Run `mailroom provision ses`: it creates the SNS topic and writes SNS_TOPIC_ARN to .env", "SNS_TOPIC_ARN is not set, so bounces and complaints can't reach Mailroom")
	}
	endpoint := s.cfg.BaseURL + "/hooks/ses"
	var (
		next           *string
		found, pending bool
		others         []string // "protocol endpoint" of every other subscription
	)
	for page := 0; page < maxSubscriptionPages; page++ {
		out, err := s.sns.ListSubscriptionsByTopic(ctx, &sns.ListSubscriptionsByTopicInput{TopicArn: &topic, NextToken: next})
		if err != nil {
			return awsFail("SNS ListSubscriptionsByTopic", "sns:ListSubscriptionsByTopic on "+topic, err)
		}
		for _, sub := range out.Subscriptions {
			if sub.Endpoint == nil || *sub.Endpoint != endpoint {
				others = append(others, strings.TrimSpace(aws.ToString(sub.Protocol)+" "+aws.ToString(sub.Endpoint)))
				continue
			}
			arn := aws.ToString(sub.SubscriptionArn)
			found = true
			pending = pending || arn == "" || strings.EqualFold(arn, "PendingConfirmation")
		}
		if out.NextToken == nil || *out.NextToken == "" {
			break
		}
		next = out.NextToken
	}
	// The topic's events carry every recipient address and header. Mailroom
	// needs only its own subscription; another one may be a leaked sending
	// key's, and survives rotating that key.
	extra := ""
	if len(others) > 0 {
		extra = fmt.Sprintf("; %d other subscription(s) also receive every event with its recipient addresses: %s",
			len(others), strings.Join(others, ", "))
	}
	const hintOthers = "Delete the other subscriptions in the SNS console (Topics → " + snsTopicName + " → Subscriptions) unless you added them on purpose, " +
		"and rotate the sending key (mailroom provision ses --rotate-keys) if you don't know where they came from"
	switch {
	case !found:
		return fail("Mailroom subscribes itself on start (X-7): check that "+endpoint+" is reachable from the internet, then restart Mailroom",
			"%s is not subscribed to %s, so bounces and complaints aren't processed%s", endpoint, topic, extra)
	case pending:
		return warn("Mailroom confirms the subscription itself when SNS calls "+endpoint+"; check that the URL is reachable from the internet and restart Mailroom",
			"the subscription of %s is pending confirmation%s", endpoint, extra)
	case len(others) > 0:
		return warn(hintOthers, "%s is subscribed to the SNS topic%s", endpoint, extra)
	}
	return ok("%s is subscribed to the SNS topic, and nothing else is", endpoint)
}

// snsTopicName is the name of the event topic provision ses creates
// (provision.TopicName).
const snsTopicName = "mailroom-ses-events"
