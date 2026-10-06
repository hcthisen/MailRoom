package events

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sns"

	"mailroom/internal/server"
)

// SubscriberWorkerName is the name of the X-7 worker.
const SubscriberWorkerName = "sns-subscriber"

// maxSubscriptionPages bounds the topic's subscription listing (100 per page).
const maxSubscriptionPages = 50

// pendingConfirmation is the SubscriptionArn SNS reports before a
// subscription is confirmed.
const pendingConfirmation = "PendingConfirmation"

// SubscriberWorker returns the sns-subscriber worker (X-7). It runs once at
// start: unless <BaseURL>/hooks/ses is already a confirmed subscription of
// SNS_TOPIC_ARN, it subscribes it (the confirmation then arrives on the
// webhook). Failures are retried with a doubling backoff until it succeeds
// or the context ends. Without SNS_TOPIC_ARN or an SNS client it logs a
// warning and stops.
func (s *Service) SubscriberWorker() server.Worker { return subscriberWorker{s: s} }

type subscriberWorker struct{ s *Service }

// Name implements server.Worker.
func (w subscriberWorker) Name() string { return SubscriberWorkerName }

// Run implements server.Worker.
func (w subscriberWorker) Run(ctx context.Context) error {
	s := w.s
	if s.cfg.SNSTopicARN == "" {
		s.log.Warn("events: SNS_TOPIC_ARN is not set; bounces and complaints are not received (run `mailroom provision ses`)")
		return nil
	}
	if s.sns == nil {
		s.log.Warn("events: no SNS client (AWS not configured); /hooks/ses is not subscribed automatically")
		return nil
	}
	wait := s.minWait
	for {
		subscribed, err := s.EnsureSubscribed(ctx)
		if err == nil {
			if subscribed {
				s.log.Info("events: subscribed to the SNS topic; waiting for its confirmation on "+PathSESHook, "topic", s.cfg.SNSTopicARN)
			}
			return nil
		}
		if ctx.Err() != nil {
			return nil
		}
		s.log.Warn("events: SNS subscription check failed; retrying", "err", err, "retry_in", wait.String())
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
		wait = min(wait*2, s.maxWait)
	}
}

// EnsureSubscribed subscribes the webhook endpoint to the pinned topic
// unless it already is a confirmed subscription. subscribed reports
// whether Subscribe was called. A subscription still pending confirmation
// is subscribed again, which makes SNS resend the confirmation.
func (s *Service) EnsureSubscribed(ctx context.Context) (subscribed bool, err error) {
	if s.sns == nil || s.cfg.SNSTopicARN == "" {
		return false, errors.New("events: SNS is not configured")
	}
	endpoint := s.cfg.BaseURL + PathSESHook
	confirmed, err := s.findSubscription(ctx, endpoint)
	if err != nil {
		return false, err
	}
	if confirmed {
		return false, nil
	}
	protocol := "https"
	if u, err := url.Parse(endpoint); err == nil && u.Scheme == "http" {
		protocol = "http"
	}
	if _, err := s.sns.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn:              aws.String(s.cfg.SNSTopicARN),
		Protocol:              aws.String(protocol),
		Endpoint:              aws.String(endpoint),
		ReturnSubscriptionArn: true,
	}); err != nil {
		return false, fmt.Errorf("events: SNS Subscribe: %w", err)
	}
	return true, nil
}

// findSubscription reports whether endpoint has a confirmed subscription
// on the pinned topic.
func (s *Service) findSubscription(ctx context.Context, endpoint string) (bool, error) {
	var next *string
	for range maxSubscriptionPages {
		out, err := s.sns.ListSubscriptionsByTopic(ctx, &sns.ListSubscriptionsByTopicInput{
			TopicArn: aws.String(s.cfg.SNSTopicARN), NextToken: next,
		})
		if err != nil {
			return false, fmt.Errorf("events: SNS ListSubscriptionsByTopic: %w", err)
		}
		for _, sub := range out.Subscriptions {
			if aws.ToString(sub.Endpoint) != endpoint {
				continue
			}
			arn := aws.ToString(sub.SubscriptionArn)
			if arn != "" && !strings.EqualFold(arn, pendingConfirmation) {
				return true, nil
			}
		}
		if aws.ToString(out.NextToken) == "" {
			return false, nil
		}
		next = out.NextToken
	}
	return false, nil
}
