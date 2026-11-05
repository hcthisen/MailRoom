package ui

import (
	"context"
	"errors"
	"net/http"
	"time"

	"mailroom/internal/broadcasts"
	"mailroom/internal/core"
	"mailroom/internal/sending"
	"mailroom/internal/stats"
	"mailroom/internal/system"
)

func init() {
	registerGroup("dashboard", func(u *UI) {
		u.handle(Action{Method: http.MethodGet, Path: Prefix + "/{$}", OperationID: "get-dashboard-stats",
			Uses: []string{"get-ses-status", "get-sending-state", "run-doctor", "list-broadcasts"}, Handler: u.dashboard})
	})
}

// sesTimeout bounds the SES status read of a dashboard load (it is
// normally served from the poller's cache).
const sesTimeout = 3 * time.Second

// Dashboard warning levels for the 30-day rates. SES reviews an account
// at about 5 % bounces and 0.1 % complaints. The dashboard highlights the
// hard-bounce rate from 2 % (O-5); no separate alert email is sent for it
// (ARCHITECTURE.md §10, "Not built").
const (
	bounceWarn     = 0.02
	bounceDanger   = 0.05
	complaintWarn  = 0.0005
	complaintAlarm = 0.001
)

type dashboardData struct {
	Stats    stats.Dashboard
	StatsErr string

	SES    *system.SESStatus
	SESErr string

	Sending    sending.State
	SendingErr string

	Doctor   *system.Report
	Warnings []system.Check // warn and fail checks of the last doctor run

	BounceLevel    string // "", "warn" or "danger"
	ComplaintLevel string
	QuotaPercent   int // 0-100, for the meter

	Recent    []broadcasts.Broadcast // the newest broadcasts
	RecentErr string
}

func level(v, warn, danger float64) string {
	switch {
	case v >= danger:
		return "danger"
	case v >= warn:
		return "warn"
	}
	return ""
}

// dashboard is the start page (A-3). Each part that fails is shown as
// unavailable instead of failing the whole page.
func (u *UI) dashboard(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	var d dashboardData
	if st, err := u.d.Stats.Dashboard(ctx); err != nil {
		u.log.ErrorContext(ctx, "ui: dashboard stats", "err", err)
		d.StatsErr = "The numbers could not be read. Check the logs."
	} else {
		d.Stats = st
		d.BounceLevel = level(st.BounceRate30d, bounceWarn, bounceDanger)
		d.ComplaintLevel = level(st.ComplaintRate30d, complaintWarn, complaintAlarm)
	}

	sctx, cancel := context.WithTimeout(ctx, sesTimeout)
	ses, err := u.d.System.SESStatus(sctx, false)
	cancel()
	switch {
	case errors.Is(err, core.ErrConflict):
		d.SESErr = "Email sending isn't connected yet: Amazon SES still has to be set up. Ask whoever installed Mailroom to finish it."
	case err != nil:
		u.log.WarnContext(ctx, "ui: SES status", "err", err)
		d.SESErr = "The SES account status could not be read right now."
	default:
		d.SES = &ses
		d.QuotaPercent = min(100, int(ses.QuotaUsed*100+0.5))
	}

	if st, err := u.d.Sending.GetState(ctx, nil); err != nil {
		u.log.ErrorContext(ctx, "ui: sending state", "err", err)
		d.SendingErr = "The sending state could not be read."
	} else {
		d.Sending = st
	}

	if rep, ok := u.LastDoctor(); ok {
		d.Doctor = &rep
		for _, c := range rep.Checks {
			if c.Status == system.StatusWarn || c.Status == system.StatusFail {
				d.Warnings = append(d.Warnings, c)
			}
		}
	}
	if bs, err := u.d.Broadcasts.List(ctx, broadcasts.ListFilter{Limit: 5}); err != nil {
		u.log.ErrorContext(ctx, "ui: recent broadcasts", "err", err)
		d.RecentErr = "The broadcasts could not be read."
	} else {
		d.Recent = bs
	}
	return u.Page(w, r, "dashboard/index", View{Title: "Dashboard", Nav: "dashboard", Data: d})
}
