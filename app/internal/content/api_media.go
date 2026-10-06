package content

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"mailroom/internal/api"
)

// maxUploadRequestBytes bounds a whole upload request: the file plus
// multipart overhead. Larger requests fail while the form is read.
const maxUploadRequestBytes = MaxMediaBytes + 64<<10

type mediaPath struct {
	ID int64 `path:"id" minimum:"1" doc:"Media ID" example:"7"`
}

type mediaOutput struct {
	Body Media
}

// UploadForm is the multipart form of upload-media.
type UploadForm struct {
	File huma.FormFile `form:"file" required:"true" doc:"A PNG, JPEG, GIF or WebP image of at most 5 MiB (the type is detected from the content)"`
}

type uploadMediaInput struct {
	RawBody huma.MultipartFormFiles[UploadForm]
}

// limitBody caps the request body before Huma parses the multipart form
// (Huma's MaxBodyBytes doesn't apply to multipart bodies).
func limitBody(n int64) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		r, w := humago.Unwrap(ctx)
		r.Body = http.MaxBytesReader(w, r.Body, n)
		next(ctx)
	}
}

func (s *Service) registerMedia(a *api.API) {
	tags := []string{"Media"}
	api.Register(a, huma.Operation{
		OperationID: "upload-media", Method: http.MethodPost, Path: "/media", Tags: tags, DefaultStatus: http.StatusCreated,
		Middlewares: huma.Middlewares{limitBody(maxUploadRequestBytes)},
		Summary:     "Upload an image",
		Description: "Uploads an image (multipart/form-data, field file) for use in emails and the layout (E-6). Only PNG, JPEG, GIF and WebP up to 5 MiB are accepted, detected from the content. " +
			"The image is stored under a random name and served publicly at the returned url (/media/<stored_name>). Remote image URLs can be used in emails too.",
	}, func(ctx context.Context, in *uploadMediaInput) (*mediaOutput, error) {
		f := in.RawBody.Data().File
		defer f.Close()
		m, err := s.UploadMedia(ctx, f.Filename, f)
		if err != nil {
			return nil, err
		}
		return &mediaOutput{Body: m}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "list-media", Method: http.MethodGet, Path: "/media", Tags: tags,
		Summary:     "List images",
		Description: "Returns the uploaded images, oldest first, with their public URLs.",
	}, func(ctx context.Context, in *api.PageParams) (*api.ListOutput[Media], error) {
		after, err := in.After()
		if err != nil {
			return nil, err
		}
		limit := in.PageLimit()
		ms, err := s.ListMedia(ctx, after, limit+1)
		if err != nil {
			return nil, err
		}
		return &api.ListOutput[Media]{Body: api.NewList(ms, limit, func(m Media) int64 { return m.ID })}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "get-media", Method: http.MethodGet, Path: "/media/{id}", Tags: tags,
		Summary:     "Get an image",
		Description: "Returns one uploaded image's details and public URL.",
	}, func(ctx context.Context, in *mediaPath) (*mediaOutput, error) {
		m, err := s.GetMedia(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		return &mediaOutput{Body: m}, nil
	})

	api.Register(a, huma.Operation{
		OperationID: "delete-media", Method: http.MethodDelete, Path: "/media/{id}", Tags: tags, DefaultStatus: http.StatusNoContent,
		Summary:     "Delete an image",
		Description: "Deletes an uploaded image and its file; its URL stops working, also in emails already sent. Refused with 409 while an email or the layout refers to it.",
	}, func(ctx context.Context, in *mediaPath) (*noContent, error) {
		return &noContent{}, s.DeleteMedia(ctx, in.ID)
	})
}
