package sequences

import (
	"context"
	"fmt"
)

// Stats returns the enrolment counts of a sequence and the numbers of each
// step (S-9). Messages of deleted steps and test sends are not counted.
func (s *Service) Stats(ctx context.Context, seqID int64) (SequenceStats, error) {
	q := s.db.Read
	if _, err := loadSeq(ctx, q, seqID); err != nil {
		return SequenceStats{}, err
	}
	out := SequenceStats{SequenceID: seqID, Steps: []SequenceStepStats{}}
	if err := q.QueryRowContext(ctx, `SELECT
			COALESCE(sum(status = 'active'), 0), COALESCE(sum(status = 'completed'), 0), COALESCE(sum(status = 'exited'), 0)
		FROM enrollments WHERE sequence_id = ?`, seqID).Scan(&out.Active, &out.Completed, &out.Exited); err != nil {
		return SequenceStats{}, fmt.Errorf("sequences: enrollment counts of %d: %w", seqID, err)
	}
	rows, err := q.QueryContext(ctx, `
		SELECT st.id, st.position,
			(SELECT count(*) FROM enrollments e WHERE e.next_step_id = st.id AND e.status = 'active'),
			COALESCE(m.queued, 0), COALESCE(m.sent, 0), COALESCE(m.delivered, 0), COALESCE(m.bounced, 0), COALESCE(m.complained, 0),
			(SELECT count(DISTINCT COALESCE(cl.contact_id, -cl.message_id)) FROM clicks cl JOIN messages mm ON mm.id = cl.message_id
				WHERE mm.step_id = st.id AND mm.kind = 'sequence'),
			(SELECT count(DISTINCT ev.message_id) FROM events ev JOIN messages mm ON mm.id = ev.message_id
				WHERE mm.step_id = st.id AND mm.kind = 'sequence' AND ev.type = 'unsubscribe')
		FROM sequence_steps st
		LEFT JOIN (
			SELECT step_id,
				sum(status IN ('queued','sending')) AS queued,
				sum(sent_at IS NOT NULL) AS sent,
				sum(delivered_at IS NOT NULL OR status = 'delivered') AS delivered,
				sum(status = 'bounced') AS bounced,
				sum(status = 'complained') AS complained
			FROM messages WHERE kind = 'sequence' AND step_id IN (SELECT id FROM sequence_steps WHERE sequence_id = ?1)
			GROUP BY step_id
		) m ON m.step_id = st.id
		WHERE st.sequence_id = ?1 ORDER BY st.position`, seqID)
	if err != nil {
		return SequenceStats{}, fmt.Errorf("sequences: step stats of %d: %w", seqID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var st SequenceStepStats
		if err := rows.Scan(&st.StepID, &st.Position, &st.Waiting, &st.Queued, &st.Sent, &st.Delivered, &st.Bounced,
			&st.Complained, &st.Clicked, &st.Unsubscribed); err != nil {
			return SequenceStats{}, fmt.Errorf("sequences: step stats of %d: %w", seqID, err)
		}
		out.Steps = append(out.Steps, st)
	}
	return out, rows.Err()
}
