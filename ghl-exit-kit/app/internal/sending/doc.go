// Package sending delivers queued messages (PRD §6.7, ARCHITECTURE.md §8).
//
// It owns:
//
//   - the sender worker: a dispatcher that picks due queued messages and N
//     workers that share one global token bucket (B-4). Each worker claims a
//     message atomically (queued -> sending), runs the final eligibility
//     check (D-6), renders it through core.Renderer, builds the MIME message
//     and hands it to the core.EmailProvider, then records the outcome with
//     retries and backoff (D-4);
//   - the global pause state (settings key "sending_state"), set when SES
//     reports sending paused or disabled, or by an operator, and lifted
//     automatically when SES sending is enabled again;
//   - the ses-account-poller worker, which stores the SES quota and status
//     under settings key "ses_account" and feeds the token bucket;
//   - the SES v2 provider (NewSESProvider) and its error classification;
//   - the Alerter (core.Alerter) that queues de-duplicated alert emails;
//   - the read API for messages, events and clicks, and the sending state API.
//
// Delivery is at most once: a message is sent only after its row has been
// switched to "sending" in a committed transaction, and rows found in
// "sending" at startup are failed instead of being sent again (see
// Service.RecoverInterrupted). For the same reason a provider call that
// ends without a response after the request may have been sent (timeout,
// connection reset) fails the message with code CodeDeliveryUnknown; only
// errors that prove the request never left (DNS, dial) and error responses
// that say so (throttling, 5xx) are retried.
package sending
