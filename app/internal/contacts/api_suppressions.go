package contacts

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"mailroom/internal/api"
)

type suppressionInput struct {
	Body struct {
		Email string `json:"email" maxLength:"254" doc:"The address; only its SHA-256 is stored" example:"alice@example.com"`
		Note  string `json:"note,omitempty" maxLength:"500" doc:"Why" example:"asked by phone not to be emailed"`
	}
}

type listSuppressionsInput struct {
	api.PageParams
	Reason    string `query:"reason" enum:"erased,manual" doc:"Only suppressions with this reason"`
	EmailHash string `query:"email_hash" pattern:"^[0-9a-fA-F]{64}$" doc:"Only the suppression with this SHA-256 hex of the normalised (trimmed, lowercased) address"`
}

type suppressOutput struct {
	Status int
	Body   SuppressResult
}

type unsuppressOutput struct {
	Body UnsuppressResult
}

func (s *Service) registerSuppressions(a *api.API) {
	tags := []string{"Suppressions"}

	api.Register(a, huma.Operation{
		OperationID: "list-suppressions", Method: http.MethodGet, Path: "/suppressions", Tags: tags,
		Summary:     "List suppressions",
		Description: "Returns suppressed addresses newest first, as SHA-256 hashes of the normalised address: `erased` rows come from GDPR erase, `manual` ones from add-suppression. To check one address, pass the hash of its trimmed, lowercased form as `email_hash`. Bounced, complained and unsubscribed contacts are statuses, not suppressions (see list-contacts).",
	}, func(ctx context.Context, in *listSuppressionsInput) (*api.ListOutput[Suppression], error) {
		before, err := in.After()
		if err != nil {
			return nil, err
		}
		limit := in.PageLimit()
		ss, err := s.ListSuppressions(ctx, SuppressionFilter{Reason: in.Reason, Hash: in.EmailHash, BeforeID: before, Limit: limit + 1})
		if err != nil {
			return nil, err
		}
		return &api.ListOutput[Suppression]{Body: api.NewList(ss, limit, func(x Suppression) int64 { return x.ID })}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "add-suppression", Method: http.MethodPost, Path: "/suppressions", Tags: tags, DefaultStatus: http.StatusCreated,
		Summary:     "Suppress an address",
		Description: "Adds the address to the suppression list (reason manual): it can't be emailed, imported or signed up. An active contact with that address is set to suppressed_manual; other statuses are kept. Answers 201, or 200 when the address was already suppressed.",
	}, func(ctx context.Context, in *suppressionInput) (*suppressOutput, error) {
		res, err := s.AddSuppression(ctx, in.Body.Email, in.Body.Note)
		if err != nil {
			return nil, err
		}
		status := http.StatusCreated
		if !res.Created {
			status = http.StatusOK
		}
		return &suppressOutput{Status: status, Body: res}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "remove-suppression", Method: http.MethodPost, Path: "/suppressions/remove", Tags: tags,
		Summary:     "Remove a manual suppression",
		Description: "Removes a manual suppression (404 if the address isn't suppressed). Erased addresses stay suppressed (403), and bounces and complaints can't be removed. A contact with status suppressed_manual is set back to active with the note (recorded in its consent log), or to unsubscribed when it asked to unsubscribe while suppressed; `status` says which. The address goes in the body, not the URL, to keep it out of logs.",
	}, func(ctx context.Context, in *suppressionInput) (*unsuppressOutput, error) {
		res, err := s.RemoveSuppression(ctx, in.Body.Email, in.Body.Note)
		if err != nil {
			return nil, err
		}
		return &unsuppressOutput{Body: res}, nil
	})
}
