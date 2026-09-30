package audit

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"mailroom/internal/api"
)

type listInput struct {
	api.PageParams
	TargetType string    `query:"target_type" maxLength:"64" doc:"Only entries for this target type, e.g. contact or settings" example:"contact"`
	TargetID   string    `query:"target_id" maxLength:"64" doc:"Only entries for this target ID (use with target_type)" example:"42"`
	Actor      string    `query:"actor" maxLength:"254" doc:"Only entries by this actor: \"token:3\", \"user:1\", \"cli\", or a token name or user email" example:"token:3"`
	Operation  string    `query:"operation" maxLength:"64" doc:"Only entries for this operation" example:"contact.update"`
	Since      time.Time `query:"since" doc:"Only entries at or after this time (RFC 3339)" example:"2026-01-01T00:00:00Z"`
	Until      time.Time `query:"until" doc:"Only entries before this time (RFC 3339)"`
}

// RegisterAPI registers GET /audit.
func (s *Service) RegisterAPI(a *api.API) {
	api.Register(a, huma.Operation{
		OperationID: "list-audit-log",
		Method:      http.MethodGet,
		Path:        "/audit",
		Summary:     "List audit log entries",
		Description: "Returns audit log entries, newest first. Every write made through the API or the UI is logged with its actor (user or token), time, operation, target and changed fields (G-8). Filter with the query parameters and page with `cursor`.",
		Tags:        []string{"Audit"},
	}, func(ctx context.Context, in *listInput) (*api.ListOutput[Record], error) {
		before, err := in.After()
		if err != nil {
			return nil, err
		}
		limit := in.PageLimit()
		recs, err := s.List(ctx, Filter{
			TargetType: in.TargetType, TargetID: in.TargetID, Actor: in.Actor, Operation: in.Operation,
			Since: in.Since, Until: in.Until, BeforeID: before, Limit: limit + 1,
		})
		if err != nil {
			return nil, err
		}
		return &api.ListOutput[Record]{Body: api.NewList(recs, limit, func(r Record) int64 { return r.ID })}, nil
	})
}
