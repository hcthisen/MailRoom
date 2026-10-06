package ui

import (
	"context"
	"sync"
	"time"

	"mailroom/internal/server"
	"mailroom/internal/system"
)

// DefaultDoctorInterval is how often DoctorWorker runs the doctor checks
// for the dashboard. Page loads never run them: several checks call DNS
// and AWS and may take seconds.
const DefaultDoctorInterval = 10 * time.Minute

// doctorFirstRun delays the first run after start, so the SNS subscriber
// and the SES poller have had a chance to run.
const doctorFirstRun = 30 * time.Second

// doctorCache keeps the last doctor report.
type doctorCache struct {
	mu   sync.Mutex
	last *system.Report
	busy sync.Mutex // one run at a time
}

// LastDoctor returns the last doctor report, if the checks have run.
func (u *UI) LastDoctor() (system.Report, bool) {
	u.doctor.mu.Lock()
	defer u.doctor.mu.Unlock()
	if u.doctor.last == nil {
		return system.Report{}, false
	}
	return *u.doctor.last, true
}

// RefreshDoctor runs the doctor checks now (system.Service.Doctor, the
// method behind GET /system/doctor), keeps the report for the dashboard
// and returns it. Concurrent calls wait for the run in progress.
func (u *UI) RefreshDoctor(ctx context.Context) system.Report {
	u.doctor.busy.Lock()
	defer u.doctor.busy.Unlock()
	rep := u.d.System.Doctor(ctx)
	u.doctor.mu.Lock()
	u.doctor.last = &rep
	u.doctor.mu.Unlock()
	return rep
}

// DoctorWorker runs the doctor checks 30 s after start and then every
// Deps.DoctorInterval, so the dashboard can show warnings without running
// network checks on every page load.
func (u *UI) DoctorWorker() server.Worker { return doctorWorker{u} }

type doctorWorker struct{ u *UI }

func (doctorWorker) Name() string { return "ui-doctor" }

func (w doctorWorker) Run(ctx context.Context) error {
	wait := doctorFirstRun
	for {
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
		w.u.RefreshDoctor(ctx)
		wait = w.u.d.DoctorInterval
	}
}
