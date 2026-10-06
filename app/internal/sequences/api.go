package sequences

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"mailroom/internal/api"
	"mailroom/internal/content"
	"mailroom/internal/settings"
)

// apiTags is the OpenAPI tag of every sequences operation.
var apiTags = []string{"Sequences"}

// Request body limits. Huma's default of 1 MiB is too small for requests
// carrying email bodies or long address lists. JSON escaping can make a
// body several times larger than its decoded text (content uses the same
// factor of four for its 1 MiB bodies).
const (
	// importMaxBodyBytes fits a YAML document and body files at their
	// limits (MaxYAMLBytes, maxTotalFiles).
	importMaxBodyBytes = 4 * (maxTotalFiles + MaxYAMLBytes)
	// stepMaxBodyBytes fits one email body of content.MaxBodyBytes.
	stepMaxBodyBytes = 4 * content.MaxBodyBytes
	// enrollMaxBodyBytes fits MaxEnrollBatch addresses of up to 254
	// characters each, quoted and comma separated.
	enrollMaxBodyBytes = MaxEnrollBatch*(254+3) + 64<<10
)

// RegisterAPI registers the sequence, step and enrollment operations.
func (s *Service) RegisterAPI(a *api.API) {
	s.registerSequences(a)
	s.registerYAML(a)
	s.registerSteps(a)
	s.registerEnrollments(a)
}

// SequenceOutput is a sequence with its ETag.
type SequenceOutput struct {
	ETag string `header:"ETag" doc:"Version of the sequence (including its steps); send it back in If-Match"`
	Body Sequence
}

func sequenceOutput(v Sequence) *SequenceOutput {
	return &SequenceOutput{ETag: api.ETag(v.Version), Body: v}
}

type sequenceIDInput struct {
	ID int64 `path:"id" minimum:"1" doc:"Sequence ID" example:"4"`
}

type listSequencesInput struct {
	api.PageParams
}

// sequenceBody is the body of create-sequence.
type sequenceBody struct {
	Name       string               `json:"name" minLength:"1" maxLength:"100" doc:"Unique name" example:"Community welcome"`
	Status     string               `json:"status,omitempty" enum:"active,paused" doc:"Default active" example:"active"`
	TriggerTag *string              `json:"trigger_tag,omitempty" maxLength:"64" doc:"Adding this tag to an active contact enrols it (created if needed); null or omitted for none" example:"joined-community"`
	ExitTags   []string             `json:"exit_tags,omitempty" maxItems:"20" doc:"A contact with any of these tags leaves before its next step (created if needed)" example:"[\"customer\"]"`
	ExitAddTag *string              `json:"exit_add_tag,omitempty" maxLength:"64" doc:"Tag added when a contact leaves because of an exit tag or because it is no longer active" example:"community-drip-exited"`
	Reentry    string               `json:"reentry,omitempty" enum:"never,after_completion,always" doc:"Default never" example:"never"`
	SendWindow *settings.SendWindow `json:"send_window,omitempty" doc:"Send only inside this window (install time zone); omit or null to use the default send window from the settings"`
}

type createSequenceInput struct {
	Body sequenceBody
}

// sequencePatchBody is the body of update-sequence.
type sequencePatchBody struct {
	Name       *string                     `json:"name,omitempty" nullable:"false" minLength:"1" maxLength:"100" example:"Community welcome"`
	Status     *string                     `json:"status,omitempty" nullable:"false" enum:"active,paused" example:"paused"`
	TriggerTag OptionalString              `json:"trigger_tag,omitempty" doc:"New trigger tag; null removes the tag trigger" example:"joined-community"`
	ExitTags   []string                    `json:"exit_tags,omitempty" maxItems:"20" doc:"Replaces the exit tags; [] removes them" example:"[\"customer\",\"refunded\"]"`
	ExitAddTag OptionalString              `json:"exit_add_tag,omitempty" doc:"New exit tag to add; null removes it" example:"community-drip-exited"`
	Reentry    *string                     `json:"reentry,omitempty" nullable:"false" enum:"never,after_completion,always" example:"after_completion"`
	SendWindow settings.OptionalSendWindow `json:"send_window,omitempty" doc:"New send window; null means the default from the settings"`
}

type updateSequenceInput struct {
	ID int64 `path:"id" minimum:"1" doc:"Sequence ID" example:"4"`
	api.IfMatchParam
	Body sequencePatchBody
}

type statusSequenceInput struct {
	ID int64 `path:"id" minimum:"1" doc:"Sequence ID" example:"4"`
	api.IfMatchParam
}

type deleteSequenceInput struct {
	ID int64 `path:"id" minimum:"1" doc:"Sequence ID" example:"4"`
	api.IfMatchParam
	Force bool `query:"force" doc:"Required while enrollments are active: they end with reason sequence_deleted"`
}

type deleteSequenceOutput struct {
	Body DeleteResult
}

type statsOutput struct {
	Body SequenceStats
}

func (s *Service) registerSequences(a *api.API) {
	api.Register(a, huma.Operation{
		OperationID: "list-sequences", Method: http.MethodGet, Path: "/sequences", Tags: apiTags,
		Summary:     "List sequences",
		Description: "Returns the sequences in ID order, each with its configuration and steps. Paginate with `limit` and `cursor`.",
	}, func(ctx context.Context, in *listSequencesInput) (*api.ListOutput[Sequence], error) {
		after, err := in.After()
		if err != nil {
			return nil, err
		}
		limit := in.PageLimit()
		list, err := s.List(ctx, after, limit+1)
		if err != nil {
			return nil, err
		}
		return &api.ListOutput[Sequence]{Body: api.NewList(list, limit, func(v Sequence) int64 { return v.ID })}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "get-sequence", Method: http.MethodGet, Path: "/sequences/{id}", Tags: apiTags,
		Summary:     "Get a sequence",
		Description: "Returns one sequence with its steps (each with its email's subject; the body is at get-email). The ETag header carries the version, which also changes when steps change.",
	}, func(ctx context.Context, in *sequenceIDInput) (*SequenceOutput, error) {
		v, err := s.Get(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		return sequenceOutput(v), nil
	})

	api.Register(a, huma.Operation{
		OperationID: "create-sequence", Method: http.MethodPost, Path: "/sequences", Tags: apiTags, DefaultStatus: http.StatusCreated,
		Summary: "Create a sequence",
		Description: "Creates a sequence without steps (add them with add-sequence-step, or create a whole sequence with import-sequence). " +
			"`trigger_tag` enrols active contacts when the tag is added to them (not by CSV imports); `exit_tags` end an enrolment before its next step; " +
			"`exit_add_tag` is added when a contact leaves because of an exit tag or because it is no longer active. Tags are created as needed; " +
			"the trigger tag can't also be an exit tag. `reentry`: never (default: once per contact, ever), after_completion (again only after completing), " +
			"always (again whenever not enrolled). `send_window` days are ISO weekdays (1 = Monday). A duplicate name is 409.",
	}, func(ctx context.Context, in *createSequenceInput) (*SequenceOutput, error) {
		b := in.Body
		v, err := s.Create(ctx, SequenceInput{
			Name: b.Name, Status: b.Status, TriggerTag: b.TriggerTag, ExitTags: b.ExitTags,
			ExitAddTag: b.ExitAddTag, Reentry: b.Reentry, SendWindow: b.SendWindow,
		})
		if err != nil {
			return nil, err
		}
		return sequenceOutput(v), nil
	})

	api.Register(a, huma.Operation{
		OperationID: "update-sequence", Method: http.MethodPatch, Path: "/sequences/{id}", Tags: apiTags,
		Summary: "Update a sequence",
		Description: "Changes the fields you send. `trigger_tag`, `exit_add_tag` and `send_window` may be null to remove them; `exit_tags` replaces the list. " +
			"`status: paused` stops sending (enrollments keep their place and new ones are still accepted); `active` resumes, and steps that fell due meanwhile go out at the next scheduler run. " +
			"New exit tags apply to active enrollments at their next step. Send the ETag in If-Match to get 412 instead of overwriting a newer change.",
	}, func(ctx context.Context, in *updateSequenceInput) (*SequenceOutput, error) {
		v, err := in.Version()
		if err != nil {
			return nil, err
		}
		b := in.Body
		p := SequencePatch{
			Name: b.Name, Status: b.Status, TriggerTag: b.TriggerTag, ExitAddTag: b.ExitAddTag,
			Reentry: b.Reentry, SendWindow: b.SendWindow,
		}
		if b.ExitTags != nil {
			p.ExitTags = &b.ExitTags
		}
		seq, err := s.Update(ctx, in.ID, p, v)
		if err != nil {
			return nil, err
		}
		return sequenceOutput(seq), nil
	})

	for _, op := range []struct{ id, path, status, summary, desc string }{
		{"pause-sequence", "/sequences/{id}/pause", StatusPaused, "Pause a sequence",
			"Stops the scheduler from sending steps of this sequence. Enrollments keep their place and new enrolments are still accepted. Same as update-sequence with `status: paused`."},
		{"resume-sequence", "/sequences/{id}/resume", StatusActive, "Resume a sequence",
			"Lets the scheduler send steps of this sequence again; steps that fell due while it was paused go out at the next run (inside the send window). Same as update-sequence with `status: active`."},
	} {
		status := op.status
		api.Register(a, huma.Operation{
			OperationID: op.id, Method: http.MethodPost, Path: op.path, Tags: apiTags, Summary: op.summary, Description: op.desc,
		}, func(ctx context.Context, in *statusSequenceInput) (*SequenceOutput, error) {
			v, err := in.Version()
			if err != nil {
				return nil, err
			}
			seq, err := s.SetStatus(ctx, in.ID, status, v)
			if err != nil {
				return nil, err
			}
			return sequenceOutput(seq), nil
		})
	}

	api.Register(a, huma.Operation{
		OperationID: "delete-sequence", Method: http.MethodDelete, Path: "/sequences/{id}", Tags: apiTags,
		Summary: "Delete a sequence",
		Description: "Deletes the sequence with its steps and enrollment history. While enrollments are active this is 409 unless `force=true`, " +
			"which ends them first (reason sequence_deleted; their queued messages are skipped). Forms that started it stop enrolling. The steps' emails are kept.",
	}, func(ctx context.Context, in *deleteSequenceInput) (*deleteSequenceOutput, error) {
		v, err := in.Version()
		if err != nil {
			return nil, err
		}
		res, err := s.Delete(ctx, in.ID, v, in.Force)
		if err != nil {
			return nil, err
		}
		return &deleteSequenceOutput{Body: res}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "get-sequence-stats", Method: http.MethodGet, Path: "/sequences/{id}/stats", Tags: apiTags,
		Summary: "Get sequence stats",
		Description: "Returns the number of active, completed and exited enrollments, and per step: enrollments waiting for it, messages queued, sent, delivered, " +
			"bounced and complained, unique contacts who clicked, and unsubscribes from its messages. Test sends are not counted.",
	}, func(ctx context.Context, in *sequenceIDInput) (*statsOutput, error) {
		st, err := s.Stats(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		return &statsOutput{Body: st}, nil
	})
}

type importSequenceInput struct {
	api.DryRunParam
	Body struct {
		YAML  string            `json:"yaml" minLength:"1" maxLength:"262144" doc:"The sequence definition (docs/sequence-format.md)" example:"name: Community welcome\nsteps:\n  - delay: \"0\"\n    subject: Welcome\n    body_file: 01-welcome.md\n"`
		Files map[string]string `json:"files,omitempty" doc:"Step bodies by file name, referenced by body_file" example:"{\"01-welcome.md\":\"Hi there!\"}"`
	}
}

type importSequenceOutput struct {
	Status int
	ETag   string `header:"ETag" doc:"Version of the new sequence"`
	Body   Sequence
}

type exportSequenceOutput struct {
	Body SequenceYAML
}

func (s *Service) registerYAML(a *api.API) {
	api.Register(a, huma.Operation{
		OperationID: "import-sequence", Method: http.MethodPost, Path: "/sequences/import", Tags: apiTags, DefaultStatus: http.StatusCreated,
		Metadata: map[string]any{api.MetaAlsoResponds: map[int]string{
			http.StatusOK: "Dry run (?dry_run=true): the sequence that would be created; nothing was stored"}},
		MaxBodyBytes: importMaxBodyBytes,
		Summary:      "Import a sequence from YAML",
		Description: "Creates a sequence, its emails and its steps from the YAML format in docs/sequence-format.md (I-3), in one transaction. " +
			"`files` holds the step bodies referenced by `body_file`, by file name. Everything is validated first; errors name the place, e.g. field `steps[2].delay`. " +
			"A name that already exists is 409. With `dry_run=true` nothing is stored and the response (200) shows the sequence that would be created.",
	}, func(ctx context.Context, in *importSequenceInput) (*importSequenceOutput, error) {
		v, err := s.ImportYAML(ctx, in.Body.YAML, in.Body.Files, in.DryRun)
		if err != nil {
			return nil, err
		}
		out := &importSequenceOutput{Status: http.StatusCreated, ETag: api.ETag(v.Version), Body: v}
		if in.DryRun {
			out.Status, out.ETag = http.StatusOK, ""
		}
		return out, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "export-sequence", Method: http.MethodGet, Path: "/sequences/{id}/export", Tags: apiTags,
		Summary:     "Export a sequence as YAML",
		Description: "Returns the sequence in the YAML format of docs/sequence-format.md, with each step's body in `files`. Importing it again (under another name) recreates the same sequence.",
	}, func(ctx context.Context, in *sequenceIDInput) (*exportSequenceOutput, error) {
		y, err := s.ExportYAML(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		return &exportSequenceOutput{Body: y}, nil
	})
}
