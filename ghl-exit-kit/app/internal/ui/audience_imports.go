package ui

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"sort"

	"mailroom/internal/imports"
	"mailroom/internal/jobs"
)

// audEmbedLimit is the largest CSV the dry-run report page carries along
// (base64 in a hidden field), so "Import" doesn't need the file again.
// Larger files are chosen again for the real import.
const audEmbedLimit = 8 << 20

type audImportsData struct {
	Tabs    template.HTML
	Form    *Form
	Imports []imports.ImportJob
	Pager   Pager
	Options []Option
}

func audOnExistingOptions() []Option {
	return []Option{
		{imports.OnExistingUpdate, "Update them: fill in names and attributes, add tags"},
		{imports.OnExistingSkip, "Leave them as they are"},
	}
}

// audImportsList lists imports (list-imports) with the upload form.
func (u *UI) audImportsList(w http.ResponseWriter, r *http.Request) error {
	return u.audRenderImports(w, r, http.StatusOK, EmptyForm().Set("on_existing", imports.OnExistingUpdate))
}

func (u *UI) audRenderImports(w http.ResponseWriter, r *http.Request, status int, f *Form) error {
	p, err := ParseList(r)
	if err != nil {
		return err
	}
	js, err := u.d.Imports.List(r.Context(), p.After, p.Fetch())
	if err != nil {
		return err
	}
	d := audImportsData{Tabs: audTabs("imports"), Form: f, Options: audOnExistingOptions()}
	d.Imports, d.Pager = Paginate(r, p, js, func(j imports.ImportJob) int64 { return j.JobID })
	return u.PageStatus(w, r, status, "audience/imports", View{Title: "Import contacts", Nav: "contacts",
		Subtitle: "Upload a CSV file. It is checked first; nothing is written until you confirm.", Data: d})
}

type audImportReportData struct {
	Report     imports.ImportReport
	Filename   string
	OnExisting string
	Embedded   string // base64 of the file, when small enough to carry along
	Form       *Form
	Statuses   []audCount
	Tags       []audCount
	Options    []Option
}

// audCount is one row of a count table.
type audCount struct {
	Name  string
	Count int
}

func audCounts(m map[string]int) []audCount {
	out := make([]audCount, 0, len(m))
	for k, v := range m {
		out = append(out, audCount{k, v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// audImportCreate is create-import. mode=check (the upload form) runs the
// dry run and shows its report; mode=import (the report's button) starts
// the import job and goes to its progress page.
func (u *UI) audImportCreate(w http.ResponseWriter, r *http.Request) error {
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct == "multipart/form-data" {
		if err := r.ParseMultipartForm(32 << 20); err != nil && !errors.Is(err, http.ErrNotMultipart) {
			return err
		}
	}
	f := NewForm(r)
	commit := f.Get("mode") == "import"
	opts := imports.Options{OnExisting: f.Get("on_existing"), Filename: f.Get("filename")}

	var (
		src      io.Reader
		embedded []byte
	)
	if b64 := f.Get("csv_b64"); b64 != "" {
		b, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return fieldError("file", "the file could not be read back; choose it again")
		}
		src = bytes.NewReader(b)
	} else {
		file, hdr, err := r.FormFile("file")
		if err != nil {
			f.AddError("file", "choose a CSV file")
			return u.audImportFormError(w, r, f, commit)
		}
		defer file.Close()
		if opts.Filename == "" {
			opts.Filename = hdr.Filename
		}
		src = file
		if !commit && hdr.Size <= audEmbedLimit {
			if embedded, err = audReadAll(file); err != nil {
				return err
			}
			src = bytes.NewReader(embedded)
		}
	}

	res, err := u.d.Imports.Import(r.Context(), src, opts, !commit)
	if err != nil {
		if f.Fail(err) == 0 {
			return err
		}
		return u.audImportFormError(w, r, f, commit)
	}
	if commit {
		u.Flash(r, FlashSuccess, "The import has started.")
		u.Redirect(w, r, fmt.Sprintf("%s/imports/%d", Prefix, res.Job.ID))
		return nil
	}
	d := audImportReportData{Report: *res.Report, Filename: opts.Filename, OnExisting: res.Report.OnExisting,
		Form: f, Statuses: audCounts(res.Report.StatusCounts), Tags: audCounts(res.Report.TagCounts),
		Options: audOnExistingOptions()}
	if embedded != nil {
		d.Embedded = base64.StdEncoding.EncodeToString(embedded)
	} else if b64 := f.Get("csv_b64"); b64 != "" {
		d.Embedded = b64
	}
	return u.Page(w, r, "audience/import", View{Title: "Check the import", Nav: "contacts",
		Subtitle: "This is what the import would do. Nothing has been written yet.",
		Crumbs:   []Crumb{{"Imports", Prefix + "/imports"}}, Data: audImportPage{Check: &d}})
}

// audImportPage is the data of page audience/import: the dry-run check
// of an upload, or an import job.
type audImportPage struct {
	Check  *audImportReportData
	Status *audImportData
}

// audImportFormError shows an upload problem on the imports page.
func (u *UI) audImportFormError(w http.ResponseWriter, r *http.Request, f *Form, commit bool) error {
	if f.Message == "" || f.Message == "Please correct the highlighted field." {
		f.Message = "The file can't be imported: " + audFormMessage(f)
	}
	if f.Get("on_existing") == "" {
		f.Set("on_existing", imports.OnExistingUpdate)
	}
	return u.audRenderImports(w, r, http.StatusUnprocessableEntity, f)
}

func audReadAll(f multipart.File) ([]byte, error) {
	return io.ReadAll(io.LimitReader(f, audEmbedLimit+1))
}

type audImportData struct {
	Job      imports.ImportJob
	Running  bool
	Percent  int
	Statuses []audCount
	Tags     []audCount
}

// audImportShow is an import's progress and final report (get-import).
// While it runs, htmx polls this address and swaps the status block.
func (u *UI) audImportShow(w http.ResponseWriter, r *http.Request) error {
	id, err := PathID(r, "job_id")
	if err != nil {
		return err
	}
	j, err := u.d.Imports.Get(r.Context(), id)
	if err != nil {
		return err
	}
	d := audImportData{Job: j, Running: j.Status == jobs.StatusQueued || j.Status == jobs.StatusRunning,
		Percent: int(j.Progress*100 + 0.5)}
	if j.Report != nil {
		d.Statuses, d.Tags = audCounts(j.Report.StatusCounts), audCounts(j.Report.TagCounts)
	}
	v := View{Title: fmt.Sprintf("Import %d", j.JobID), Subtitle: j.Filename, Nav: "contacts",
		Crumbs: []Crumb{{"Imports", Prefix + "/imports"}}, Data: audImportPage{Status: &d}}
	if isHTMX(r) {
		return u.Partial(w, r, "audience/import", "import-status", v)
	}
	return u.Page(w, r, "audience/import", v)
}
