package sending

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sesv2"

	"mailroom/internal/core"
	"mailroom/internal/db"
)

// AccountAPI is the part of the SES v2 client the ses-account-poller uses.
// *sesv2.Client satisfies it (see SESProvider.AccountAPI).
type AccountAPI interface {
	GetAccount(ctx context.Context, in *sesv2.GetAccountInput, optFns ...func(*sesv2.Options)) (*sesv2.GetAccountOutput, error)
}

// Account is the last SES account snapshot, stored under settings key
// AccountKey by the ses-account-poller.
type Account struct {
	MaxSendRate       float64   `json:"max_send_rate" doc:"SES maximum send rate (messages per second)" example:"14"`
	Max24hSend        float64   `json:"max_24_hour_send" doc:"SES 24-hour sending quota; -1 means unlimited" example:"50000"`
	SentLast24h       float64   `json:"sent_last_24_hours" doc:"Messages SES sent from the account in the last 24 hours" example:"1200"`
	SendingEnabled    bool      `json:"sending_enabled" doc:"Whether SES currently allows the account to send" example:"true"`
	ProductionAccess  bool      `json:"production_access_enabled" doc:"False while the account is in the SES sandbox" example:"true"`
	EnforcementStatus string    `json:"enforcement_status" doc:"SES enforcement status: HEALTHY, PROBATION or SHUTDOWN" example:"HEALTHY"`
	FetchedAt         time.Time `json:"fetched_at" doc:"When the snapshot was taken"`
}

// accountTimeout bounds one GetAccount call.
const accountTimeout = 30 * time.Second

// currentAccount returns the in-memory SES snapshot (nil until known).
func (s *Service) currentAccount() *Account {
	s.acctMu.RLock()
	defer s.acctMu.RUnlock()
	if s.acct == nil {
		return nil
	}
	a := *s.acct
	return &a
}

func (s *Service) setAccount(a Account) {
	s.acctMu.Lock()
	s.acct = &a
	s.acctMu.Unlock()
}

// StoredAccount returns the SES snapshot stored by the poller; it wraps
// core.ErrNotFound when the account was never polled. A nil q reads from
// the read pool.
func (s *Service) StoredAccount(ctx context.Context, q db.Querier) (Account, error) {
	raw, err := s.settings.GetRaw(ctx, q, AccountKey)
	if err != nil {
		return Account{}, err
	}
	var a Account
	if err := json.Unmarshal(raw, &a); err != nil {
		return Account{}, fmt.Errorf("sending: decode %s: %w", AccountKey, err)
	}
	return a, nil
}

// loadAccount primes the in-memory snapshot from the settings table, so
// the rate is right straight after a restart.
func (s *Service) loadAccount(ctx context.Context) {
	if s.currentAccount() != nil {
		return
	}
	a, err := s.StoredAccount(ctx, nil)
	if err != nil {
		if !errors.Is(err, core.ErrNotFound) {
			s.log.Warn("sending: read stored SES account", "err", err)
		}
		return
	}
	s.setAccount(a)
}

// PollAccount fetches the SES account once, stores the snapshot, feeds the
// token bucket and applies the pause rules: sending disabled pauses all
// sending (with an alert, once); sending enabled again lifts an SES pause
// (with an alert). A manual pause is never lifted here.
func (s *Service) PollAccount(ctx context.Context) (Account, error) {
	if s.account == nil {
		return Account{}, errors.New("sending: no SES account API configured")
	}
	cctx, cancel := context.WithTimeout(ctx, accountTimeout)
	out, err := s.account.GetAccount(cctx, &sesv2.GetAccountInput{})
	cancel()
	if err != nil {
		return Account{}, fmt.Errorf("sending: SES GetAccount: %w", classifySESError(err))
	}
	a := Account{
		SendingEnabled:   out.SendingEnabled,
		ProductionAccess: out.ProductionAccessEnabled,
		FetchedAt:        s.clock.Now(),
	}
	if out.SendQuota != nil {
		a.MaxSendRate = out.SendQuota.MaxSendRate
		a.Max24hSend = out.SendQuota.Max24HourSend
		a.SentLast24h = out.SendQuota.SentLast24Hours
	}
	if out.EnforcementStatus != nil {
		a.EnforcementStatus = *out.EnforcementStatus
	}

	err = s.db.WithTx(ctx, func(tx *sql.Tx) error {
		if err := s.settings.PutRaw(ctx, tx, AccountKey, a); err != nil {
			return err
		}
		if !a.SendingEnabled {
			reason := "SES reports sending disabled for the account"
			if a.EnforcementStatus != "" {
				reason += " (enforcement status " + a.EnforcementStatus + ")"
			}
			changed, err := s.setPaused(ctx, tx, PauseSES, reason)
			if err != nil || !changed {
				return err
			}
			s.log.Warn("sending: all sending paused", "source", PauseSES, "reason", reason)
			return s.Alert(ctx, tx, AlertSESPaused, "Sending is paused: Amazon SES disabled sending",
				"Mailroom has paused all sending because "+reason+".\n\nQueued messages are kept. Mailroom resumes "+
					"automatically when SES enables sending again (checked every 5 minutes). Check the SES console "+
					"for account status and any notice from AWS.")
		}
		resumed, err := s.setResumed(ctx, tx, PauseSES)
		if err != nil || !resumed {
			return err
		}
		s.log.Info("sending: SES sending enabled again, resumed")
		return s.Alert(ctx, tx, AlertSESResumed, "Sending resumed: Amazon SES enabled sending again",
			"Amazon SES reports that sending is enabled again, so Mailroom has resumed sending the queued messages.")
	})
	if err != nil {
		return Account{}, err
	}
	s.setAccount(a)
	return a, nil
}

// accountPoller is the ses-account-poller worker.
type accountPoller struct{ s *Service }

func (p *accountPoller) Name() string { return "ses-account-poller" }

// Run polls at start and then every AccountPollInterval. Failures are
// logged and retried at the next tick; the last snapshot stays in use.
func (p *accountPoller) Run(ctx context.Context) error {
	for {
		if _, err := p.s.PollAccount(ctx); err != nil && ctx.Err() == nil {
			p.s.log.Warn("sending: SES account poll failed", "err", err)
		}
		if !wait(ctx, p.s.acctInterval, nil) {
			return nil
		}
	}
}
