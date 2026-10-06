package stats

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"mailroom/internal/api"
)

type dashboardInput struct{}

type dashboardOutput struct{ Body Dashboard }

// RegisterAPI registers GET /stats/dashboard.
func (s *Service) RegisterAPI(a *api.API) {
	api.Register(a, huma.Operation{
		OperationID: "get-dashboard-stats",
		Method:      http.MethodGet,
		Path:        "/stats/dashboard",
		Summary:     "Get dashboard numbers",
		Description: "Returns the numbers of the admin dashboard (A-3): contacts in total and active, new contacts in the last 7 and 30 days, unsubscribes in the last 30 days, broadcast, sequence and opt-in emails sent this month (in the settings time zone) and in the last 30 days, and the hard-bounce and complaint rates of the emails sent in the last 30 days. Test sends and alerts are not counted. The message numbers are cached for up to a minute; the contact numbers are recomputed as soon as contacts are added, erased or change status. The SES quota is at GET /system/ses and the doctor checks at GET /system/doctor.",
		Tags:        []string{"Stats"},
	}, func(ctx context.Context, _ *dashboardInput) (*dashboardOutput, error) {
		d, err := s.Dashboard(ctx)
		if err != nil {
			return nil, err
		}
		return &dashboardOutput{Body: d}, nil
	})
}
