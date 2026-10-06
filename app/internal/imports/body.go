package imports

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"mailroom/internal/api"
	"mailroom/internal/core"
)

// upload is a create-import request body after reading: the parsed file
// for a dry run, the stored file otherwise.
type upload struct {
	parsed     *parsedFile // dry run
	stored     string      // stored file name (not a dry run)
	size       int64
	filename   string
	onExisting string
}

// maxFieldBytes bounds a multipart form value other than the file.
const maxFieldBytes = 1 << 10

// errNoFile is the error when a request carries no CSV.
var errNoFile = core.Invalid("file", "a CSV file is required: multipart field file, JSON field csv, or a text/csv body")

// resolveOnExisting combines the query parameter and a body field; both
// may be empty, but they must not disagree.
func resolveOnExisting(query, body string) (string, error) {
	if query != "" && body != "" && query != body {
		return "", core.Invalid("on_existing", "the query parameter and the body disagree")
	}
	if body != "" {
		query = body
	}
	return validOnExisting(query)
}

// readUpload reads the request body: multipart/form-data (field file),
// application/json ({"csv": "...", "on_existing": "..."}) or text/csv.
// For a dry run the CSV is parsed while it is read; otherwise it is
// stored. When an error is returned, nothing remains on disk.
func (s *Service) readUpload(ctx context.Context, r *http.Request, dryRun bool, queryOnExisting string) (up upload, err error) {
	up.onExisting = queryOnExisting
	defer func() {
		if err != nil && up.stored != "" {
			os.Remove(filepath.Join(s.dir, up.stored))
			up.stored = ""
		}
	}()
	take := func(rd io.Reader) error {
		if dryRun {
			pf, err := parseFile(limitFile(rd), s.clock.Now())
			up.parsed = pf
			return err
		}
		name, size, err := s.saveUpload(rd)
		up.stored, up.size = name, size
		return err
	}

	ct := r.Header.Get("Content-Type")
	mt, params, err := mime.ParseMediaType(ct)
	if ct == "" || err != nil {
		return up, unsupportedType()
	}
	switch mt {
	case "multipart/form-data":
		err = s.readMultipart(r, params["boundary"], &up, take)
	case "application/json":
		err = readJSON(r.Body, &up, take)
	case "text/csv", "text/plain", "application/csv":
		err = take(r.Body)
	default:
		return up, unsupportedType()
	}
	if err != nil {
		return up, err
	}
	if up.parsed == nil && up.stored == "" {
		return up, errNoFile
	}
	up.onExisting, err = validOnExisting(up.onExisting)
	return up, err
}

func unsupportedType() error {
	return api.NewError(http.StatusUnsupportedMediaType, api.CodeValidation,
		"send the CSV as multipart/form-data (field file), application/json ({\"csv\": \"...\"}) or text/csv", "Content-Type")
}

// readMultipart streams the form's parts: file (once) and on_existing.
// Other fields are ignored.
func (s *Service) readMultipart(r *http.Request, boundary string, up *upload, take func(io.Reader) error) error {
	if boundary == "" {
		return core.Invalid("Content-Type", "multipart/form-data needs a boundary")
	}
	mr, err := r.MultipartReader()
	if err != nil {
		return core.Invalid("file", "the multipart form can't be read: %v", err)
	}
	formOnExisting := ""
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return formError(err)
		}
		switch part.FormName() {
		case "file":
			if up.parsed != nil || up.stored != "" {
				part.Close()
				return core.Invalid("file", "send one file per import")
			}
			up.filename = part.FileName()
			if err := take(part); err != nil {
				part.Close()
				return formError(err)
			}
		case "on_existing":
			b, err := io.ReadAll(io.LimitReader(part, maxFieldBytes))
			if err != nil {
				part.Close()
				return formError(err)
			}
			formOnExisting = strings.TrimSpace(string(b))
		}
		part.Close()
	}
	v, err := resolveOnExisting(up.onExisting, formOnExisting)
	if err != nil {
		return err
	}
	up.onExisting = v
	return nil
}

// formError keeps validation, size and timeout errors and describes other
// read errors as a malformed form.
func formError(err error) error {
	var mbe *http.MaxBytesError
	if errors.Is(err, core.ErrValidation) || errors.As(err, &mbe) || errors.Is(err, os.ErrDeadlineExceeded) {
		return err
	}
	return core.Invalid("file", "the multipart form can't be read: %v", err)
}

// jsonUpload is the JSON form of create-import.
type jsonUpload struct {
	CSV        *string `json:"csv"`
	OnExisting string  `json:"on_existing"`
}

// readJSON decodes {"csv": "...", "on_existing": "..."}.
func readJSON(body io.Reader, up *upload, take func(io.Reader) error) error {
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	var in jsonUpload
	if err := dec.Decode(&in); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) || errors.Is(err, os.ErrDeadlineExceeded) {
			return err
		}
		return core.Invalid("body", "invalid JSON: %v", err)
	}
	if dec.More() {
		return core.Invalid("body", "unexpected data after the JSON object")
	}
	if in.CSV == nil {
		return core.Invalid("csv", "is required")
	}
	v, err := resolveOnExisting(up.onExisting, in.OnExisting)
	if err != nil {
		return err
	}
	up.onExisting = v
	if len(*in.CSV) > MaxFileBytes {
		return errFileTooLarge
	}
	return take(strings.NewReader(*in.CSV))
}
