package settings

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"mailroom/internal/api"
)

// Output is a settings response with its ETag.
type Output struct {
	ETag string `header:"ETag" doc:"Version of the settings; send it back in If-Match"`
	Body Settings
}

type getInput struct{}

type patchInput struct {
	api.IfMatchParam
	Body Patch
}

// RegisterAPI registers GET and PATCH /settings.
func (s *Service) RegisterAPI(a *api.API) {
	api.Register(a, huma.Operation{
		OperationID: "get-settings",
		Method:      http.MethodGet,
		Path:        "/settings",
		Summary:     "Get settings",
		Description: "Returns the business settings: sender name and addresses, footer, owner and alert addresses, time zone, tracking, send rate, circuit-breaker thresholds and default send window. `domain` is the sending domain; it is read-only (changing it means re-provisioning). The ETag header carries the version for If-Match.",
		Tags:        []string{"Settings"},
	}, func(ctx context.Context, _ *getInput) (*Output, error) {
		st, err := s.Get(ctx)
		if err != nil {
			return nil, err
		}
		return &Output{ETag: api.ETag(st.Version), Body: st}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "update-settings",
		Method:      http.MethodPatch,
		Path:        "/settings",
		Summary:     "Update settings",
		Description: "Partially updates the settings: only the fields you send change (nested objects are merged). `default_send_window: null` removes the default send window. `sender.from_email` and `sender.reply_to` must be on the sending domain (D-7), and `domain` can't be changed. Send the ETag from GET in If-Match to get 412 instead of overwriting someone else's change. Every change is written to the audit log.",
		Tags:        []string{"Settings"},
	}, func(ctx context.Context, in *patchInput) (*Output, error) {
		v, err := in.Version()
		if err != nil {
			return nil, err
		}
		st, err := s.Update(ctx, in.Body, v)
		if err != nil {
			return nil, err
		}
		return &Output{ETag: api.ETag(st.Version), Body: st}, nil
	})
}
