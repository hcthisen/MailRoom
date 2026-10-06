// Package events handles everything that arrives from outside after an
// email has been sent (PRD §6.8–§6.10):
//
//   - POST /hooks/ses receives Amazon SNS messages. Every message is checked
//     against the pinned topic (SNS_TOPIC_ARN) and its signature
//     (SignatureVersion 1 and 2) before anything else happens. The
//     signing certificate must come from sns.<topic region>.amazonaws.com;
//     parsed certificates are cached, and URLs that yield none are refused
//     for a while without being fetched again.
//     SubscriptionConfirmation is confirmed by fetching its SubscribeURL,
//     but only for the pinned topic and only on sns.<region>.amazonaws.com.
//     Notifications carry SES events (Bounce, Complaint, Delivery, Reject),
//     which update the message and apply the contact rules of X-4.
//   - GET/POST /u/{token} is the unsubscribe page and the RFC 8058
//     one-click endpoint (U-1..U-4).
//   - GET /c/{token} records a click and redirects (K-1); GET /o/{token}
//     records an open and returns a 1x1 GIF (K-2).
//   - The sns-subscriber worker subscribes /hooks/ses to the topic on start
//     when it isn't subscribed yet (X-7).
//
// # Idempotency
//
// Every SES event row is keyed by events.provider_event_id =
// "<SNS MessageId>:<sha256 hex of the normalised recipient>", one row per
// recipient. SNS redelivers messages, so a repeated delivery finds the key
// and changes nothing. The plain address is never used, so the key stays
// the same after a GDPR erase (which rewrites the suffix to that hash).
//
// # SES event rules
//
// A permanent bounce makes the message bounced and the contact bounced; a
// transient or undetermined bounce is kept as its events row only (the
// message status stays, so "bounced" in the stats always means a hard
// bounce) and the contact becomes bounced after TransientBounceLimit of
// them within TransientBounceWindow. A complaint makes message and contact
// complained, except complaintFeedbackType "not-spam", which is recorded
// and changes nothing. An event for a message whose SES MessageId is not
// recorded yet (the sender stores it right after SES accepts the email) is
// answered 503 once when the email is younger than UnknownMessageGrace, so
// SNS redelivers it after the sender has committed; a redelivery, or an
// older event, is applied to the contact with the recipient's address.
//
// # Personal data in events.detail
//
// detail holds the raw SES JSON, which contains recipient addresses, only
// where a GDPR erase finds it again: rows of a contact's message for that
// contact's own address. Rows of erased contacts store '{}'; every other
// row (an unknown message, a test or alert message without a contact,
// another contact's address) stores an address-free summary
// ({"summary":true,"eventType":...,"sesMessageId":...}).
//
// # Stale tokens
//
// contacts.id is a plain rowid, so after a GDPR erase a new contact can get
// the erased contact's id. A token therefore acts on its contact only while
// its message still belongs to that contact: once erase has cleared the
// message's contact, the unsubscribe link shows the result page and
// changes nothing, and clicks and opens are recorded without a contact.
// (A token whose message row is gone still acts on its contact.)
//
// # Sequence-only unsubscribe
//
// "Only stop these emails" on the unsubscribe page of a sequence message
// exits just that enrollment. events does this with a direct
// UPDATE enrollments SET status = 'exited', exit_reason =
// 'unsubscribed_sequence' (no import of the sequences package, which is a
// sibling). The sender skips queued messages of exited enrollments, so
// nothing more of that sequence is sent; the contact stays active.
package events
