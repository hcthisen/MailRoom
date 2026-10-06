package provision

import (
	"context"
	"fmt"
	"slices"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	sestypes "github.com/aws/aws-sdk-go-v2/service/sesv2/types"
)

// eventTypes are the SES events published to SNS.
var eventTypes = []sestypes.EventType{
	sestypes.EventTypeBounce,
	sestypes.EventTypeComplaint,
	sestypes.EventTypeDelivery,
	sestypes.EventTypeReject,
}

// suppressedReasons are the account-level suppression list reasons.
var suppressedReasons = []sestypes.SuppressionListReason{
	sestypes.SuppressionListReasonBounce,
	sestypes.SuppressionListReasonComplaint,
}

// identity ensures the domain identity with Easy DKIM (2048-bit keys) and
// the custom MAIL FROM domain.
func (r *run) identity(ctx context.Context) error {
	domain := aws.String(r.in.Domain)
	ident, err := r.c.SES.GetEmailIdentity(ctx, &sesv2.GetEmailIdentityInput{EmailIdentity: domain})
	switch {
	case isAPIError(err, sesNotFound...):
		_, err := r.c.SES.CreateEmailIdentity(ctx, &sesv2.CreateEmailIdentityInput{
			EmailIdentity: domain,
			DkimSigningAttributes: &sestypes.DkimSigningAttributes{
				DomainSigningAttributesOrigin: sestypes.DkimSigningAttributesOriginAwsSes,
				NextSigningKeyLength:          sestypes.DkimSigningKeyLengthRsa2048Bit,
			},
		})
		if err != nil {
			return fmt.Errorf("ses create-email-identity %s: %w", r.in.Domain, err)
		}
		ident = &sesv2.GetEmailIdentityOutput{}
		r.step("ses identity", ActionCreated, r.in.Domain+" (Easy DKIM, RSA 2048)")
	case err != nil:
		return fmt.Errorf("ses get-email-identity %s: %w", r.in.Domain, err)
	default:
		action := ActionUnchanged
		detail := r.in.Domain
		d := ident.DkimAttributes
		easy := d == nil || d.SigningAttributesOrigin == "" || d.SigningAttributesOrigin == sestypes.DkimSigningAttributesOriginAwsSes
		if easy && (d == nil || d.NextSigningKeyLength != sestypes.DkimSigningKeyLengthRsa2048Bit) {
			_, err := r.c.SES.PutEmailIdentityDkimSigningAttributes(ctx, &sesv2.PutEmailIdentityDkimSigningAttributesInput{
				EmailIdentity:           domain,
				SigningAttributesOrigin: sestypes.DkimSigningAttributesOriginAwsSes,
				SigningAttributes:       &sestypes.DkimSigningAttributes{NextSigningKeyLength: sestypes.DkimSigningKeyLengthRsa2048Bit},
			})
			if err != nil {
				return fmt.Errorf("ses put-email-identity-dkim-signing-attributes: %w", err)
			}
			action, detail = ActionUpdated, r.in.Domain+" (next DKIM key RSA 2048)"
		}
		r.step("ses identity", action, detail)
	}
	r.ident = ident
	return r.mailFrom(ctx)
}

func (r *run) mailFrom(ctx context.Context) error {
	want := r.in.MailFromSubdomain + "." + r.in.Domain
	cur := r.ident.MailFromAttributes
	if cur != nil && aws.ToString(cur.MailFromDomain) == want && cur.BehaviorOnMxFailure == sestypes.BehaviorOnMxFailureUseDefaultValue {
		r.step("ses mail from", ActionUnchanged, want)
		return nil
	}
	_, err := r.c.SES.PutEmailIdentityMailFromAttributes(ctx, &sesv2.PutEmailIdentityMailFromAttributesInput{
		EmailIdentity:       aws.String(r.in.Domain),
		MailFromDomain:      aws.String(want),
		BehaviorOnMxFailure: sestypes.BehaviorOnMxFailureUseDefaultValue,
	})
	if err != nil {
		return fmt.Errorf("ses put-email-identity-mail-from-attributes: %w", err)
	}
	r.step("ses mail from", createdOrUpdated(cur == nil || aws.ToString(cur.MailFromDomain) == ""), want)
	return nil
}

// suppression turns on the account-level suppression list for bounces and
// complaints.
func (r *run) suppression(ctx context.Context) error {
	acct, err := r.c.SES.GetAccount(ctx, &sesv2.GetAccountInput{})
	if err != nil {
		return fmt.Errorf("ses get-account: %w", err)
	}
	var cur []sestypes.SuppressionListReason
	if acct.SuppressionAttributes != nil {
		cur = acct.SuppressionAttributes.SuppressedReasons
	}
	if sameSet(cur, suppressedReasons) {
		r.step("ses account suppression", ActionUnchanged, "BOUNCE, COMPLAINT")
		return nil
	}
	_, err = r.c.SES.PutAccountSuppressionAttributes(ctx, &sesv2.PutAccountSuppressionAttributesInput{
		SuppressedReasons: slices.Clone(suppressedReasons),
	})
	if err != nil {
		return fmt.Errorf("ses put-account-suppression-attributes: %w", err)
	}
	r.step("ses account suppression", createdOrUpdated(len(cur) == 0), "BOUNCE, COMPLAINT")
	return nil
}

func (r *run) configurationSet(ctx context.Context) error {
	name := aws.String(r.in.ConfigurationSet)
	_, err := r.c.SES.GetConfigurationSet(ctx, &sesv2.GetConfigurationSetInput{ConfigurationSetName: name})
	switch {
	case err == nil:
		r.step("ses configuration set", ActionUnchanged, r.in.ConfigurationSet)
		return nil
	case !isAPIError(err, sesNotFound...):
		return fmt.Errorf("ses get-configuration-set %s: %w", r.in.ConfigurationSet, err)
	}
	if _, err := r.c.SES.CreateConfigurationSet(ctx, &sesv2.CreateConfigurationSetInput{ConfigurationSetName: name}); err != nil {
		return fmt.Errorf("ses create-configuration-set %s: %w", r.in.ConfigurationSet, err)
	}
	r.step("ses configuration set", ActionCreated, r.in.ConfigurationSet)
	return nil
}

// eventDestination publishes the configuration set's events to the topic.
func (r *run) eventDestination(ctx context.Context) error {
	name := aws.String(r.in.ConfigurationSet)
	out, err := r.c.SES.GetConfigurationSetEventDestinations(ctx, &sesv2.GetConfigurationSetEventDestinationsInput{ConfigurationSetName: name})
	if err != nil {
		return fmt.Errorf("ses get-configuration-set-event-destinations: %w", err)
	}
	def := &sestypes.EventDestinationDefinition{
		Enabled:            true,
		MatchingEventTypes: slices.Clone(eventTypes),
		SnsDestination:     &sestypes.SnsDestination{TopicArn: aws.String(r.res.TopicARN)},
	}
	detail := EventDestinationName + " -> " + r.res.TopicARN
	for _, d := range out.EventDestinations {
		if aws.ToString(d.Name) != EventDestinationName {
			continue
		}
		if d.Enabled && d.SnsDestination != nil && aws.ToString(d.SnsDestination.TopicArn) == r.res.TopicARN &&
			sameSet(d.MatchingEventTypes, eventTypes) {
			r.step("ses event destination", ActionUnchanged, detail)
			return nil
		}
		_, err := r.c.SES.UpdateConfigurationSetEventDestination(ctx, &sesv2.UpdateConfigurationSetEventDestinationInput{
			ConfigurationSetName: name,
			EventDestinationName: aws.String(EventDestinationName),
			EventDestination:     def,
		})
		if err != nil {
			return fmt.Errorf("ses update-configuration-set-event-destination: %w", err)
		}
		r.step("ses event destination", ActionUpdated, detail)
		return nil
	}
	_, err = r.c.SES.CreateConfigurationSetEventDestination(ctx, &sesv2.CreateConfigurationSetEventDestinationInput{
		ConfigurationSetName: name,
		EventDestinationName: aws.String(EventDestinationName),
		EventDestination:     def,
	})
	if err != nil {
		return fmt.Errorf("ses create-configuration-set-event-destination: %w", err)
	}
	r.step("ses event destination", ActionCreated, detail)
	return nil
}

// identityConfigurationSet makes the configuration set the identity's
// default, so even a send without an explicit set is tracked.
func (r *run) identityConfigurationSet(ctx context.Context) error {
	cur := aws.ToString(r.ident.ConfigurationSetName)
	if cur == r.in.ConfigurationSet {
		r.step("ses identity default configuration set", ActionUnchanged, r.in.ConfigurationSet)
		return nil
	}
	_, err := r.c.SES.PutEmailIdentityConfigurationSetAttributes(ctx, &sesv2.PutEmailIdentityConfigurationSetAttributesInput{
		EmailIdentity:        aws.String(r.in.Domain),
		ConfigurationSetName: aws.String(r.in.ConfigurationSet),
	})
	if err != nil {
		return fmt.Errorf("ses put-email-identity-configuration-set-attributes: %w", err)
	}
	r.step("ses identity default configuration set", createdOrUpdated(cur == ""), r.in.ConfigurationSet)
	return nil
}

func createdOrUpdated(created bool) Action {
	if created {
		return ActionCreated
	}
	return ActionUpdated
}

// sameSet reports whether a and b hold the same distinct values.
func sameSet[T comparable](a, b []T) bool {
	in := func(xs []T, x T) bool { return slices.Contains(xs, x) }
	for _, x := range a {
		if !in(b, x) {
			return false
		}
	}
	for _, x := range b {
		if !in(a, x) {
			return false
		}
	}
	return true
}
