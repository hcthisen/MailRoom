package sequences

import (
	"context"
	"database/sql"
)

// StepTestResult reports a test send of a step.
type StepTestResult struct {
	MessageIDs []int64  `json:"message_ids" doc:"The queued test messages" example:"[901]"`
	To         []string `json:"to" doc:"The normalised recipient addresses" example:"[\"owner@example.org\"]"`
}

// TestStep queues a test send of a step's email (S-10) through the content
// service: to 1-5 addresses (empty: the owner's address from the
// settings), with contactID's data for the merge fields when it is set,
// else the fallbacks. Test messages carry {"test": true}, have no working
// unsubscribe or tracking links, don't count in stats, and the sender skips
// them for suppressed addresses and for addresses whose contact opted out
// or can't receive mail (core.CanReceive).
func (s *Service) TestStep(ctx context.Context, seqID, stepID int64, to []string, contactID int64) (StepTestResult, error) {
	var res StepTestResult
	err := s.db.WithTx(ctx, func(tx *sql.Tx) error {
		step, err := loadStep(ctx, tx, seqID, stepID)
		if err != nil {
			return err
		}
		res.MessageIDs, res.To, err = s.content.SendTest(ctx, tx, step.EmailID, to, contactID)
		return err
	})
	return res, err
}
