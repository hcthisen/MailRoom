package provision

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sns"
)

// topic ensures the SNS topic and its policy: only SES, on behalf of this
// account's configuration set, may publish.
func (r *run) topic(ctx context.Context) error {
	arn := r.res.TopicARN
	attrs, err := r.c.SNS.GetTopicAttributes(ctx, &sns.GetTopicAttributesInput{TopicArn: aws.String(arn)})
	created := false
	var curPolicy string
	switch {
	case isAPIError(err, snsNotFound...):
		out, err := r.c.SNS.CreateTopic(ctx, &sns.CreateTopicInput{Name: aws.String(TopicName)})
		if err != nil {
			return fmt.Errorf("sns create-topic %s: %w", TopicName, err)
		}
		if got := aws.ToString(out.TopicArn); got != "" && got != arn {
			return fmt.Errorf("sns create-topic returned %s, expected %s", got, arn)
		}
		created = true
		r.step("sns topic", ActionCreated, arn)
	case err != nil:
		return fmt.Errorf("sns get-topic-attributes %s: %w", arn, err)
	default:
		curPolicy = attrs.Attributes["Policy"]
		r.step("sns topic", ActionUnchanged, arn)
	}

	sources := []string{r.res.ConfigurationSetARN}
	if r.receive {
		sources = append(sources, r.arns.receiptRule())
	}
	want := TopicPolicy(arn, r.res.AccountID, sources...)
	if !created && policyMatches(curPolicy, want) {
		r.step("sns topic policy", ActionUnchanged, "ses.amazonaws.com may publish for "+r.in.ConfigurationSet)
		return nil
	}
	_, err = r.c.SNS.SetTopicAttributes(ctx, &sns.SetTopicAttributesInput{
		TopicArn:       aws.String(arn),
		AttributeName:  aws.String("Policy"),
		AttributeValue: aws.String(want.JSON()),
	})
	if err != nil {
		return fmt.Errorf("sns set-topic-attributes Policy: %w", err)
	}
	r.step("sns topic policy", createdOrUpdated(created), "ses.amazonaws.com may publish for "+r.in.ConfigurationSet)
	return nil
}
