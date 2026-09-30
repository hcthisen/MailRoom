package sequences

import (
	"context"
	"database/sql"

	"mailroom/internal/api"
)

// ImportYAML creates a sequence with its emails and steps from the YAML
// format (I-3) in one transaction. Everything is validated first; a name
// that already exists is core.ErrConflict. With dryRun the transaction is
// rolled back and the returned sequence shows what would be created (its
// IDs are not kept).
func (s *Service) ImportYAML(ctx context.Context, doc string, files map[string]string, dryRun bool) (Sequence, error) {
	def, err := ParseYAML(doc, files)
	if err != nil {
		return Sequence{}, err
	}
	var out Sequence
	err = api.Tx(ctx, s.db, dryRun, func(tx *sql.Tx) error {
		created, err := s.create(ctx, tx, def.Sequence)
		if err != nil {
			return err
		}
		r, err := loadSeq(ctx, tx, created.ID)
		if err != nil {
			return err
		}
		for i, st := range def.Steps {
			email := st.Email
			if _, err := s.addStep(ctx, tx, r, StepInput{Email: &email, DelaySeconds: st.DelaySeconds, Position: i + 1}); err != nil {
				return prefixField(err, "steps.")
			}
		}
		if out, err = s.get(ctx, tx, created.ID); err != nil {
			return err
		}
		return s.logAudit(ctx, tx, "sequence.import", "sequence", created.ID, map[string]any{
			"name": out.Name, "steps": len(out.Steps), "files": len(files),
		})
	})
	if err != nil {
		return Sequence{}, err
	}
	return out, nil
}

// ExportYAML returns a sequence in the YAML format, with each step's body
// in its own file. Importing the result (under another name, or after
// deleting the original) recreates the same sequence.
func (s *Service) ExportYAML(ctx context.Context, id int64) (SequenceYAML, error) {
	def, err := s.definition(ctx, id)
	if err != nil {
		return SequenceYAML{}, err
	}
	return marshalDefinition(def)
}

// definition reads a stored sequence as a Definition.
func (s *Service) definition(ctx context.Context, id int64) (Definition, error) {
	seq, err := s.Get(ctx, id)
	if err != nil {
		return Definition{}, err
	}
	def := Definition{Sequence: seq.input()}
	for _, st := range seq.Steps {
		e, err := s.content.GetEmail(ctx, nil, st.EmailID)
		if err != nil {
			return Definition{}, err
		}
		def.Steps = append(def.Steps, DefinitionStep{DelaySeconds: st.DelaySeconds, Email: StepEmail{
			Subject: e.Subject, Preheader: e.Preheader, Format: e.Format, Body: e.Body,
		}})
	}
	return def, nil
}
