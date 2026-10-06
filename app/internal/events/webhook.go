package events

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// maxSNSBodyBytes bounds a webhook body. SNS messages are at most 256 KiB;
// the JSON envelope adds escaping.
const maxSNSBodyBytes = 1 << 20

// AlertSNSUnsubscribed is the alert key raised when SNS reports that the
// /hooks/ses subscription was removed.
const AlertSNSUnsubscribed = "sns_unsubscribed"

// handleSNS serves POST /hooks/ses (X-1, X-2).
//
//   - malformed body or a message-type header that disagrees with the body: 400
//   - TopicArn other than SNS_TOPIC_ARN, bad signature or certificate URL: 403
//   - certificate fetch or database failure, or a fresh SES event for a
//     message whose SES MessageId isn't recorded yet (once): 5xx, so SNS
//     retries
//   - everything else: 200
func (s *Service) handleSNS(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSNSBodyBytes))
	if err != nil {
		plain(w, http.StatusBadRequest, "request body too large or unreadable")
		return
	}
	env, err := parseEnvelope(body)
	if err != nil {
		s.log.Warn("events: rejected SNS message", "err", err)
		plain(w, http.StatusBadRequest, "malformed SNS message")
		return
	}
	if h := r.Header.Get(SNSMessageTypeHeader); h != "" && h != env.Type {
		s.log.Warn("events: SNS message type header disagrees with body", "header", h, "type", env.Type)
		plain(w, http.StatusBadRequest, "message type mismatch")
		return
	}
	if s.cfg.SNSTopicARN == "" || env.TopicArn != s.cfg.SNSTopicARN {
		s.log.Warn("events: SNS message for a foreign topic", "topic", env.TopicArn, "configured", s.cfg.SNSTopicARN != "")
		plain(w, http.StatusForbidden, "topic not accepted")
		return
	}
	if err := s.verify(r.Context(), env); err != nil {
		if errors.Is(err, errCertFetch) {
			s.log.Error("events: SNS signing certificate unavailable", "err", err)
			plain(w, http.StatusServiceUnavailable, "signing certificate unavailable")
			return
		}
		s.log.Warn("events: SNS signature rejected", "err", err, "message_id", env.MessageID)
		plain(w, http.StatusForbidden, "invalid signature")
		return
	}

	switch env.Type {
	case TypeSubscriptionConfirmation:
		if err := s.confirmSubscription(r.Context(), env); err != nil {
			s.log.Error("events: confirm SNS subscription", "err", err)
			if errors.Is(err, errSignature) {
				plain(w, http.StatusForbidden, "subscribe URL not accepted")
				return
			}
			plain(w, http.StatusBadGateway, "confirmation failed")
			return
		}
		s.log.Info("events: SNS subscription confirmed", "topic", env.TopicArn)
	case TypeUnsubscribeConfirmation:
		s.unsubscribed(r.Context(), env)
	case TypeNotification:
		if err := s.processNotification(r.Context(), env); err != nil {
			if errors.Is(err, errRetryLater) {
				s.log.Info("events: SES event for a message not recorded yet; SNS will redeliver", "message_id", env.MessageID)
				plain(w, http.StatusServiceUnavailable, "message not recorded yet")
				return
			}
			if errors.Is(err, errMalformed) {
				s.log.Warn("events: malformed SES event", "err", err, "message_id", env.MessageID)
				plain(w, http.StatusBadRequest, "malformed SES event")
				return
			}
			s.log.Error("events: process SES event", "err", err, "message_id", env.MessageID)
			plain(w, http.StatusInternalServerError, "processing failed")
			return
		}
	}
	plain(w, http.StatusOK, "ok")
}

// confirmSubscription fetches the SubscribeURL of a verified confirmation
// for the pinned topic. Only https URLs on sns.<region>.amazonaws.com are
// fetched (SSRF guard); anything else wraps errSignature.
func (s *Service) confirmSubscription(ctx context.Context, env Envelope) error {
	if env.TopicArn != s.cfg.SNSTopicARN {
		return fmt.Errorf("%w: confirmation for a foreign topic", errSignature)
	}
	if !s.validSubscribeURL(env.SubscribeURL) {
		return fmt.Errorf("%w: SubscribeURL host is not sns.%s.amazonaws.com", errSignature, s.region())
	}
	ctx, cancel := context.WithTimeout(ctx, DefaultHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, env.SubscribeURL, nil)
	if err != nil {
		return fmt.Errorf("%w: %v", errSignature, err)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("fetch SubscribeURL: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch SubscribeURL: HTTP %d", resp.StatusCode)
	}
	return nil
}

// unsubscribed logs and alerts on an UnsubscribeConfirmation: bounces and
// complaints no longer reach Mailroom until it is subscribed again.
func (s *Service) unsubscribed(ctx context.Context, env Envelope) {
	s.log.Warn("events: /hooks/ses was unsubscribed from the SNS topic", "topic", env.TopicArn)
	if s.alerter == nil {
		return
	}
	body := "Amazon SNS reports that " + s.cfg.BaseURL + PathSESHook + " was unsubscribed from the topic " + env.TopicArn +
		". Bounces and complaints are not processed until it is subscribed again. Restart Mailroom to subscribe it again, then run `mailroom doctor`."
	if err := s.alerter.Alert(ctx, nil, AlertSNSUnsubscribed, "SNS subscription removed", body); err != nil {
		s.log.Error("events: raise SNS unsubscribe alert", "err", err)
	}
}

// plain writes a short text/plain response.
func plain(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, msg+"\n")
}
