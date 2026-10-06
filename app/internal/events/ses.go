package events

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// SES event types handled here. Configuration-set event publishing uses
// "eventType"; identity notifications use "notificationType".
const (
	SESBounce    = "Bounce"
	SESComplaint = "Complaint"
	SESDelivery  = "Delivery"
	SESReject    = "Reject"
)

// ComplaintNotSpam is the complaintFeedbackType of a feedback report that
// says the message is not spam (often a correction of an earlier report).
// It is recorded but is not a complaint against the contact.
const ComplaintNotSpam = "not-spam"

// Bounce types (bounce.bounceType).
const (
	BouncePermanent    = "Permanent"
	BounceTransient    = "Transient"
	BounceUndetermined = "Undetermined"
)

// sesEvent is the part of an SES event notification Mailroom reads.
type sesEvent struct {
	EventType        string `json:"eventType"`
	NotificationType string `json:"notificationType"`
	Mail             struct {
		MessageID   string   `json:"messageId"`
		Timestamp   string   `json:"timestamp"`
		Destination []string `json:"destination"`
	} `json:"mail"`
	Bounce *struct {
		BounceType        string      `json:"bounceType"`
		BounceSubType     string      `json:"bounceSubType"`
		BouncedRecipients []recipient `json:"bouncedRecipients"`
	} `json:"bounce"`
	Complaint *struct {
		ComplaintFeedbackType string      `json:"complaintFeedbackType"`
		ComplainedRecipients  []recipient `json:"complainedRecipients"`
	} `json:"complaint"`
	Delivery *struct {
		Timestamp  string   `json:"timestamp"`
		Recipients []string `json:"recipients"`
	} `json:"delivery"`
	Reject *struct {
		Reason string `json:"reason"`
	} `json:"reject"`
}

type recipient struct {
	EmailAddress string `json:"emailAddress"`
}

// kind returns the event type, whichever field carries it.
func (e sesEvent) kind() string {
	if e.EventType != "" {
		return e.EventType
	}
	return e.NotificationType
}

// parsedEvent is an SES event reduced to what processing needs.
type parsedEvent struct {
	Kind          string    // SESBounce | SESComplaint | SESDelivery | SESReject
	SubType       string    // events.subtype: permanent|transient|undetermined for bounces, the feedback type for complaints
	BounceType    string    // BouncePermanent | BounceTransient | BounceUndetermined
	BounceSubType string    // bounce.bounceSubType, e.g. General
	MessageID     string    // SES mail.messageId
	Recipients    []string  // raw recipient addresses, as SES reports them
	SentAt        time.Time // mail.timestamp; zero when absent
	DeliveredAt   time.Time // delivery.timestamp; zero when absent
}

// notSpam reports whether the event is a "not-spam" feedback report, which
// must neither suppress the contact nor mark the message complained.
func (e parsedEvent) notSpam() bool {
	return e.Kind == SESComplaint && e.SubType == ComplaintNotSpam
}

// hardBounce reports whether the event is a permanent bounce.
func (e parsedEvent) hardBounce() bool {
	return e.Kind == SESBounce && e.BounceType == BouncePermanent
}

// eventSummary is the address-free detail stored for an event row that a
// GDPR erase could not find later (see storedDetail).
type eventSummary struct {
	Summary               bool   `json:"summary"` // always true: the raw event was withheld
	EventType             string `json:"eventType"`
	BounceType            string `json:"bounceType,omitempty"`
	BounceSubType         string `json:"bounceSubType,omitempty"`
	ComplaintFeedbackType string `json:"complaintFeedbackType,omitempty"`
	SESMessageID          string `json:"sesMessageId"`
}

// summary returns the address-free JSON summary of e.
func (e parsedEvent) summary() string {
	s := eventSummary{Summary: true, EventType: e.Kind, SESMessageID: e.MessageID}
	switch e.Kind {
	case SESBounce:
		s.BounceType, s.BounceSubType = e.BounceType, e.BounceSubType
	case SESComplaint:
		s.ComplaintFeedbackType = e.SubType
	}
	b, _ := json.Marshal(s) // plain strings: can't fail
	return string(b)
}

// parseSESEvent decodes an SES event. ok is false for event types that
// Mailroom ignores (Send, Open, Click, DeliveryDelay, test notifications,
// ...). Invalid JSON and handled events without mail.messageId wrap
// errMalformed.
func parseSESEvent(raw string) (ev parsedEvent, ok bool, err error) {
	var e sesEvent
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		return parsedEvent{}, false, fmt.Errorf("%w: SES event is not JSON: %v", errMalformed, err)
	}
	ev = parsedEvent{Kind: e.kind(), MessageID: strings.TrimSpace(e.Mail.MessageID)}
	if t, err := time.Parse(time.RFC3339Nano, e.Mail.Timestamp); err == nil {
		ev.SentAt = t.UTC()
	}
	switch ev.Kind {
	case SESBounce:
		if e.Bounce == nil {
			return parsedEvent{}, false, fmt.Errorf("%w: Bounce event without bounce", errMalformed)
		}
		ev.BounceType = normalizeBounceType(e.Bounce.BounceType)
		ev.SubType = strings.ToLower(ev.BounceType)
		ev.BounceSubType = strings.TrimSpace(e.Bounce.BounceSubType)
		for _, r := range e.Bounce.BouncedRecipients {
			ev.Recipients = append(ev.Recipients, r.EmailAddress)
		}
	case SESComplaint:
		if e.Complaint == nil {
			return parsedEvent{}, false, fmt.Errorf("%w: Complaint event without complaint", errMalformed)
		}
		ev.SubType = strings.ToLower(strings.TrimSpace(e.Complaint.ComplaintFeedbackType))
		for _, r := range e.Complaint.ComplainedRecipients {
			ev.Recipients = append(ev.Recipients, r.EmailAddress)
		}
	case SESDelivery:
		if e.Delivery != nil {
			ev.Recipients = append(ev.Recipients, e.Delivery.Recipients...)
			if t, err := time.Parse(time.RFC3339Nano, e.Delivery.Timestamp); err == nil {
				ev.DeliveredAt = t.UTC()
			}
		}
	case SESReject:
	default:
		return parsedEvent{}, false, nil
	}
	if ev.MessageID == "" {
		return parsedEvent{}, false, fmt.Errorf("%w: %s event without mail.messageId", errMalformed, ev.Kind)
	}
	if len(ev.Recipients) == 0 {
		// Reject events have no recipient list, and a delivery without one
		// still concerns the message's destination.
		ev.Recipients = append(ev.Recipients, e.Mail.Destination...)
	}
	ev.Recipients = dedupeRecipients(ev.Recipients)
	return ev, true, nil
}

// normalizeBounceType maps bounceType to one of the Bounce* constants;
// anything unknown counts as undetermined.
func normalizeBounceType(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "permanent":
		return BouncePermanent
	case "transient":
		return BounceTransient
	default:
		return BounceUndetermined
	}
}

// dedupeRecipients drops empty and repeated addresses (compared
// normalised), keeping the first spelling.
func dedupeRecipients(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0:0]
	for _, r := range in {
		r = strings.TrimSpace(r)
		k := normalizeRecipient(r)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, r)
	}
	return out
}
