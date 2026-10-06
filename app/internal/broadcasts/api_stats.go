package broadcasts

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"mailroom/internal/api"
)

type statsOutput struct {
	Body BroadcastStats
}

type recipientsInput struct {
	ID int64 `path:"id" minimum:"1" doc:"Broadcast ID" example:"7"`
	api.PageParams
	Status string `query:"status" enum:"queued,sending,sent,delivered,bounced,complained,failed,skipped" doc:"Only messages with this status"`
}

func (s *Service) registerStats(a *api.API) {
	api.Register(a, huma.Operation{
		OperationID: "get-broadcast-stats", Method: http.MethodGet, Path: "/broadcasts/{id}/stats", Tags: apiTags,
		Summary: "Get broadcast stats",
		Description: "Returns the broadcast's numbers (B-8): recipients, queued, sent, delivered, bounced (hard), complained, failed, skipped, unsubscribed, unique clicks, clicks per link, and rates as fractions of sent. " +
			"`missing_tags` lists tag names of the audience segment that no longer exist. Test sends don't count.",
	}, func(ctx context.Context, in *idInput) (*statsOutput, error) {
		st, err := s.Stats(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		return &statsOutput{Body: st}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "list-broadcast-recipients", Method: http.MethodGet, Path: "/broadcasts/{id}/recipients", Tags: apiTags,
		Summary:     "List broadcast recipients",
		Description: "Returns the broadcast's messages in queue order with their recipient, status, error and times. Filter with `status` (e.g. bounced, failed, skipped); paginate with `limit` and `cursor`.",
	}, func(ctx context.Context, in *recipientsInput) (*api.ListOutput[BroadcastRecipient], error) {
		after, err := in.After()
		if err != nil {
			return nil, err
		}
		limit := in.PageLimit()
		rs, err := s.Recipients(ctx, in.ID, RecipientFilter{Status: in.Status, AfterID: after, Limit: limit + 1})
		if err != nil {
			return nil, err
		}
		return &api.ListOutput[BroadcastRecipient]{Body: api.NewList(rs, limit, func(r BroadcastRecipient) int64 { return r.MessageID })}, nil
	})
}
