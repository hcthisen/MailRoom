package imports

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"mailroom/internal/api"
	"mailroom/internal/core"
	"mailroom/internal/db"
	"mailroom/internal/jobs"
)

// errFileTooLarge reports a CSV over MaxFileBytes.
var errFileTooLarge = core.Invalid("file", "the file is larger than %d MiB", MaxFileBytes>>20)

// Options are an import's options.
type Options struct {
	OnExisting string // update (default) or skip
	Filename   string // the uploaded file's name, for display; optional
}

// ImportResult is the answer to an import request: the submitted job, or
// for a dry run the report of what would happen.
type ImportResult struct {
	Job    *jobs.Job     `json:"job,omitempty" doc:"The import job (type import.contacts); absent in a dry run. Poll get-import or get-job."`
	Report *ImportReport `json:"report,omitempty" doc:"What the import would do; only in a dry run"`
}

// limitedReader fails with errFileTooLarge after MaxFileBytes.
type limitedReader struct {
	r io.Reader
	n int64 // bytes left
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.n < 0 {
		return 0, errFileTooLarge
	}
	if int64(len(p)) > l.n+1 {
		p = p[:l.n+1]
	}
	n, err := l.r.Read(p)
	l.n -= int64(n)
	if l.n < 0 {
		return n, errFileTooLarge
	}
	return n, err
}

func limitFile(r io.Reader) io.Reader { return &limitedReader{r: r, n: MaxFileBytes} }

// Import imports the CSV in r. Without dryRun it stores the file and
// submits an import.contacts job (with its audit record, in one
// transaction); with dryRun it evaluates the whole file against the
// current contacts and returns the report, writing nothing.
func (s *Service) Import(ctx context.Context, r io.Reader, opts Options, dryRun bool) (ImportResult, error) {
	onExisting, err := validOnExisting(opts.OnExisting)
	if err != nil {
		return ImportResult{}, err
	}
	if dryRun {
		pf, err := parseFile(limitFile(r), s.clock.Now())
		if err != nil {
			return ImportResult{}, err
		}
		rep, err := s.evaluate(ctx, pf, onExisting)
		if err != nil {
			return ImportResult{}, err
		}
		return ImportResult{Report: &rep}, nil
	}
	name, size, err := s.saveUpload(r)
	if err != nil {
		return ImportResult{}, err
	}
	j, err := s.start(ctx, name, size, Options{OnExisting: onExisting, Filename: opts.Filename})
	if err != nil {
		return ImportResult{}, err
	}
	return ImportResult{Job: &j}, nil
}

// evaluate plans every record of pf against the database (read pool only)
// and returns the report a real import would give now.
func (s *Service) evaluate(ctx context.Context, pf *parsedFile, onExisting string) (ImportReport, error) {
	var st applyStats
	for start := 0; start < len(pf.records); start += BatchSize {
		recs := pf.records[start:min(start+BatchSize, len(pf.records))]
		state, err := loadState(ctx, s.db.Read, recs)
		if err != nil {
			return ImportReport{}, err
		}
		for _, r := range recs {
			st.count(r, decide(r, state.existing[r.email], state.suppressed[r.email], onExisting))
		}
	}
	return buildReport(pf, st, onExisting, true), nil
}

// saveUpload writes r to a new random file (0600) in the imports
// directory and returns its name and size. The file is removed again when
// writing fails.
func (s *Service) saveUpload(r io.Reader) (name string, size int64, err error) {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return "", 0, fmt.Errorf("imports: create %s: %w", s.dir, err)
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", 0, fmt.Errorf("imports: random name: %w", err)
	}
	name = hex.EncodeToString(b[:]) + ".csv"
	path := filepath.Join(s.dir, name)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, db.FileMode)
	if err != nil {
		return "", 0, fmt.Errorf("imports: create upload: %w", err)
	}
	size, err = io.Copy(f, limitFile(r))
	if cerr := f.Close(); err == nil && cerr != nil {
		err = fmt.Errorf("imports: write upload: %w", cerr)
	}
	if err != nil {
		os.Remove(path)
		if errors.Is(err, errFileTooLarge) {
			return "", 0, err
		}
		return "", 0, fmt.Errorf("imports: write upload: %w", err)
	}
	return name, size, nil
}

// start checks the stored file's header and submits its job with an audit
// record in one transaction. The file is removed when that fails.
func (s *Service) start(ctx context.Context, name string, size int64, opts Options) (j jobs.Job, err error) {
	path := filepath.Join(s.dir, name)
	defer func() {
		if err != nil {
			os.Remove(path)
		}
	}()
	if size == 0 {
		return jobs.Job{}, fileError("the file is empty; it needs a header row with at least an email column")
	}
	if err := checkHeader(path); err != nil {
		return jobs.Job{}, err
	}
	if s.jobs == nil {
		return jobs.Job{}, errors.New("imports: no job runner configured")
	}
	params := jobParams{File: name, OnExisting: opts.OnExisting, Filename: cleanFilename(opts.Filename), SizeBytes: size, ParsedAt: s.clock.Now().UTC()}
	err = api.Tx(ctx, s.db, false, func(tx *sql.Tx) error {
		var err error
		if j, err = s.jobs.Submit(ctx, tx, JobType, params); err != nil {
			return err
		}
		return s.logAudit(ctx, tx, "import.create", "job", fmt.Sprint(j.ID), map[string]any{
			"on_existing": params.OnExisting, "filename": params.Filename, "size_bytes": size,
		})
	})
	return j, err
}

// checkHeader reads the file's header row, so a file without an email
// column fails the request instead of the job.
func checkHeader(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("imports: open upload: %w", err)
	}
	defer f.Close()
	_, _, err = openCSV(f)
	return err
}

// maxFilenameLength bounds the stored display name of an upload.
const maxFilenameLength = 255

// cleanFilename keeps the base name of an uploaded file for display,
// without control characters.
func cleanFilename(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, `\`, "/"))
	if name == "." || name == "/" {
		return ""
	}
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	if r := []rune(name); len(r) > maxFilenameLength {
		name = string(r[:maxFilenameLength])
	}
	return strings.ToValidUTF8(name, "")
}
