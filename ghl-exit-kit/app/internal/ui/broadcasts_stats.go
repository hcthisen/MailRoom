package ui

// The broadcast stats page (B-8): the numbers, the clicks per link and
// the recipients with a status filter.

import (
	"net/http"
	"strings"

	"mailroom/internal/broadcasts"
)

func (u *UI) registerBroadcastStats() {
	u.handle(Action{Method: http.MethodGet, Path: broadcastsPath + "/{id}/stats", OperationID: "get-broadcast-stats",
		Uses: []string{"get-broadcast", "list-broadcast-recipients"}, Handler: u.broadcastStats})
}

// recipientStatuses is the recipients filter.
var recipientStatuses = []Option{
	{"", "Any status"},
	{"queued", "Queued"},
	{"sending", "Sending"},
	{"sent", "Sent"},
	{"delivered", "Delivered"},
	{"bounced", "Bounced"},
	{"complained", "Complained"},
	{"failed", "Failed"},
	{"skipped", "Skipped"},
}

type broadcastStatsData struct {
	B          broadcasts.Broadcast
	Stats      broadcasts.BroadcastStats
	Recipients []broadcasts.BroadcastRecipient
	Pager      Pager
	Filter     *Form
	Statuses   []Option
	// Live is set while the broadcast sends: the numbers refresh.
	Live bool
	// Levels of the bounce and complaint rates against the circuit
	// breaker's defaults ("", "warn" or "danger").
	BounceLevel, ComplaintLevel string
}

// broadcastStats is the stats page.
func (u *UI) broadcastStats(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	b, err := u.d.Broadcasts.Get(ctx, id)
	if err != nil {
		return err
	}
	st, err := u.d.Broadcasts.Stats(ctx, id)
	if err != nil {
		return err
	}
	p, err := ParseList(r)
	if err != nil {
		return err
	}
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	rows, err := u.d.Broadcasts.Recipients(ctx, id, broadcasts.RecipientFilter{Status: status, AfterID: p.After, Limit: p.Fetch()})
	if err != nil {
		return err
	}
	items, pager := Paginate(r, p, rows, func(x broadcasts.BroadcastRecipient) int64 { return x.MessageID })
	d := broadcastStatsData{
		B: b, Stats: st, Recipients: items, Pager: pager,
		Filter: EmptyForm().Set("status", status), Statuses: recipientStatuses,
		Live: b.Status == broadcasts.StatusSending,
	}
	if st.Sent > 0 {
		d.BounceLevel = level(st.Rates.Bounce, bounceWarn, bounceDanger)
		d.ComplaintLevel = level(st.Rates.Complaint, complaintWarn, complaintAlarm)
	}
	return u.Page(w, r, "broadcasts/stats", View{
		Title: "Stats: " + b.Name, Nav: "broadcasts",
		Crumbs: []Crumb{{"Broadcasts", broadcastsPath}, {b.Name, broadcastURL(id)}}, Data: d,
	})
}
