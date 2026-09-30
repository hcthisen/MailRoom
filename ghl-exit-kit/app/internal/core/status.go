package core

// ContactStatus is the single global subscription status of a contact (C-2).
type ContactStatus string

// Contact statuses. Only StatusActive receives marketing email.
const (
	StatusPending          ContactStatus = "pending"
	StatusActive           ContactStatus = "active"
	StatusUnsubscribed     ContactStatus = "unsubscribed"
	StatusBounced          ContactStatus = "bounced"
	StatusComplained       ContactStatus = "complained"
	StatusSuppressedManual ContactStatus = "suppressed_manual"
)

// ContactStatuses lists every valid contact status in a stable order.
var ContactStatuses = []ContactStatus{
	StatusPending, StatusActive, StatusUnsubscribed,
	StatusBounced, StatusComplained, StatusSuppressedManual,
}

// Valid reports whether s is one of the defined contact statuses.
func (s ContactStatus) Valid() bool {
	switch s {
	case StatusPending, StatusActive, StatusUnsubscribed,
		StatusBounced, StatusComplained, StatusSuppressedManual:
		return true
	}
	return false
}

// MessageKind is the kind of a messages row.
type MessageKind string

// Message kinds.
const (
	KindBroadcast MessageKind = "broadcast"
	KindSequence  MessageKind = "sequence"
	KindOptin     MessageKind = "optin"
	KindTest      MessageKind = "test"
	KindAlert     MessageKind = "alert"
)

// Valid reports whether k is one of the defined message kinds.
func (k MessageKind) Valid() bool {
	switch k {
	case KindBroadcast, KindSequence, KindOptin, KindTest, KindAlert:
		return true
	}
	return false
}

// Keys of messages.data that more than one package reads.
const (
	// DataFromName and DataReplyTo override the settings' From name and
	// Reply-To of a test message (kind test), e.g. with a broadcast's sender
	// overrides (B-7). Written by content, applied by sending. A missing key
	// keeps the settings' value; DataReplyTo "" means no Reply-To.
	DataFromName = "from_name"
	DataReplyTo  = "reply_to"
)

// MessageStatus is the delivery status of a messages row.
type MessageStatus string

// Message statuses.
const (
	MessageQueued     MessageStatus = "queued"
	MessageSending    MessageStatus = "sending"
	MessageSent       MessageStatus = "sent"
	MessageDelivered  MessageStatus = "delivered"
	MessageBounced    MessageStatus = "bounced"
	MessageComplained MessageStatus = "complained"
	MessageFailed     MessageStatus = "failed"
	MessageSkipped    MessageStatus = "skipped"
)

// Valid reports whether s is one of the defined message statuses.
func (s MessageStatus) Valid() bool {
	switch s {
	case MessageQueued, MessageSending, MessageSent, MessageDelivered,
		MessageBounced, MessageComplained, MessageFailed, MessageSkipped:
		return true
	}
	return false
}

// IsMarketingKind reports whether messages of this kind are marketing email:
// wrapped in the base layout with an unsubscribe footer, List-Unsubscribe
// headers and click tracking. Broadcasts and sequence steps are marketing;
// opt-in confirmations, tests and alerts are not.
func IsMarketingKind(kind MessageKind) bool {
	return kind == KindBroadcast || kind == KindSequence
}

// CanReceive reports whether a contact in the given status may be sent a
// message of the given kind (ARCHITECTURE.md §6, last paragraph):
//
//   - broadcast and sequence messages go only to active contacts;
//   - optin messages go to pending and unsubscribed contacts (resubscribe)
//     and to active ones (a double opt-in form submitted for a subscriber),
//     never to bounced, complained or manually suppressed ones;
//   - test messages go to any address except one whose contact opted out
//     or can't receive mail: status here is the status of the contact with
//     the test's address (no contact: the sender only checks the
//     suppressions table), and only active and pending contacts pass, so a
//     test never reaches an unsubscribed, bounced, complained or manually
//     suppressed contact (PRD goal 3);
//   - alert messages are never suppressed.
//
// CanReceive does not consult the suppressions table; the sender does that
// separately for every kind except alert.
func CanReceive(status ContactStatus, kind MessageKind) bool {
	switch kind {
	case KindBroadcast, KindSequence:
		return status == StatusActive
	case KindOptin:
		return status == StatusPending || status == StatusUnsubscribed || status == StatusActive
	case KindTest:
		return status == StatusActive || status == StatusPending
	case KindAlert:
		return true
	}
	return false
}
