package ui

import (
	"archive/zip"
	"bytes"
	"fmt"
	"html/template"
	"io"
	"mime/multipart"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"

	"mailroom/internal/sequences"
)

// Sequence YAML import (dry run first) and export (page and zip
// download), the format of docs/sequence-format.md (I-3).

func (u *UI) registerSequenceYAML() {
	u.handle(Action{Method: http.MethodGet, Path: seqPath + "/import", UIOnly: "the empty YAML import form; submitting it is import-sequence",
		Handler: u.yamlNew})
	u.handle(Action{Method: http.MethodPost, Path: seqPath + "/import", OperationID: "import-sequence",
		MaxBodyBytes: yamlMaxBody, Handler: u.yamlImport})
	u.handle(Action{Method: http.MethodGet, Path: seqPath + "/{id}/export", OperationID: "export-sequence",
		Uses: []string{"get-sequence"}, Handler: u.yamlExport})
	u.handle(Action{Method: http.MethodGet, Path: seqPath + "/{id}/export.zip", OperationID: "export-sequence",
		Uses: []string{"get-sequence"}, Handler: u.yamlDownload})
}

// Import limits: the service's own (sequences.MaxYAMLBytes, 16 MiB of
// body files) plus room for the multipart framing. The confirmation page
// re-posts the files as multipart fields, so they are not re-encoded.
const (
	yamlMaxFiles    = 16 << 20
	yamlMaxBody     = 2*(yamlMaxFiles+sequences.MaxYAMLBytes) + 1<<20
	yamlFileName    = "file_name"
	yamlFileBody    = "file_body"
	yamlUploadField = "files"
)

// yamlFile is one body file of an import.
type yamlFile struct {
	Name string
	Body string
}

type yamlImportData struct {
	Summary template.HTML // of Preview
	Form    *Form
	Files   []yamlFile
	Preview *sequences.Sequence // the dry run's result
	Window  string
}

func (u *UI) yamlView(w http.ResponseWriter, r *http.Request, status int, d yamlImportData) error {
	v := View{Title: "Import a sequence", Nav: "sequences", Crumbs: []Crumb{{"Sequences", seqPath}}, Data: d,
		Subtitle: "From the YAML format of docs/sequence-format.md, with the email bodies inline or as files."}
	if d.Preview != nil {
		v.Title = "Check the import"
		v.Subtitle = d.Preview.Name
		d.Window = autoWindowText(d.Preview.SendWindow)
		sum, err := u.seqSummary(*d.Preview, d.Window, "")
		if err != nil {
			return err
		}
		d.Summary = sum
		v.Data = d
		return u.PageStatus(w, r, status, "automation/seq_import_preview", v)
	}
	return u.PageStatus(w, r, status, "automation/seq_import", v)
}

// yamlNew is the import form.
func (u *UI) yamlNew(w http.ResponseWriter, r *http.Request) error {
	return u.yamlView(w, r, http.StatusOK, yamlImportData{Form: EmptyForm()})
}

// yamlReadFiles collects the body files: those uploaded in this request
// and those carried over as fields from an earlier step (an upload with
// the same name replaces the carried one). A .yaml/.yml upload fills an
// empty YAML field.
func yamlReadFiles(r *http.Request, f *Form) ([]yamlFile, error) {
	byName := map[string]string{}
	names, bodies := f.All(yamlFileName), f.All(yamlFileBody)
	for i, n := range names {
		if i < len(bodies) && n != "" {
			byName[n] = bodies[i]
		}
	}
	if r.MultipartForm != nil {
		for _, fh := range r.MultipartForm.File[yamlUploadField] {
			body, err := yamlReadUpload(fh)
			if err != nil {
				return nil, err
			}
			name := path.Base(strings.ReplaceAll(fh.Filename, "\\", "/"))
			ext := strings.ToLower(path.Ext(name))
			if (ext == ".yaml" || ext == ".yml") && strings.TrimSpace(f.Raw("yaml")) == "" {
				f.Set("yaml", body)
				continue
			}
			byName[name] = body
		}
	}
	out := make([]yamlFile, 0, len(byName))
	for n, b := range byName {
		out = append(out, yamlFile{Name: n, Body: b})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func yamlReadUpload(fh *multipart.FileHeader) (string, error) {
	if fh.Size > yamlMaxFiles {
		return "", fieldError("files", fmt.Sprintf("%s is larger than 16 MB", fh.Filename))
	}
	file, err := fh.Open()
	if err != nil {
		return "", err
	}
	defer file.Close()
	b, err := io.ReadAll(io.LimitReader(file, yamlMaxFiles+1))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// yamlImport checks (dry run) and then imports a sequence
// (import-sequence). The first post shows what would be created; its
// confirmation re-posts the YAML and files with confirm=yes. "edit" goes
// back to the form with everything kept.
func (u *UI) yamlImport(w http.ResponseWriter, r *http.Request) error {
	if r.MultipartForm == nil && strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			return fieldError("files", "the upload could not be read: "+err.Error())
		}
	}
	f := NewForm(r)
	files, err := yamlReadFiles(r, f)
	d := yamlImportData{Form: f, Files: files}
	if err != nil {
		if code := f.Fail(err); code != 0 {
			return u.yamlView(w, r, code, d)
		}
		return err
	}
	if f.Get("edit") != "" {
		return u.yamlView(w, r, http.StatusOK, d)
	}
	fileMap := make(map[string]string, len(files))
	for _, fl := range files {
		fileMap[fl.Name] = fl.Body
	}
	confirmed := Confirmed(r)
	seq, err := u.d.Sequences.ImportYAML(r.Context(), f.Raw("yaml"), fileMap, !confirmed)
	if err != nil {
		if code := f.Fail(yamlErrorField(err)); code != 0 {
			return u.yamlView(w, r, code, d)
		}
		return err
	}
	if !confirmed {
		d.Preview = &seq
		return u.yamlView(w, r, http.StatusOK, d)
	}
	u.Flash(r, FlashSuccess, fmt.Sprintf("Sequence %q imported with %s.", seq.Name, plural(len(seq.Steps), "step", "steps")))
	u.Redirect(w, r, seqURL(seq.ID, ""))
	return nil
}

// yamlErrorField shows an import error about a place in the document
// ("steps[2].delay") above the form, naming the place, and one about the
// document itself next to the YAML field.
func yamlErrorField(err error) error {
	return autoRetarget(err, func(field, msg string) (string, string) {
		switch {
		case field == "" || field == "yaml":
			return "yaml", msg
		case field == "files" || strings.HasPrefix(field, "files"):
			return "files", msg
		}
		return "", field + ": " + msg
	})
}

// yamlExportData is the export page's data.
type yamlExportData struct {
	Tabs  template.HTML
	Seq   sequences.Sequence
	Slug  string // file name of the YAML in the zip, without .yaml
	YAML  string
	Files []yamlFile
}

// yamlExport shows a sequence in the YAML format with its body files.
func (u *UI) yamlExport(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	seq, err := u.seqGet(ctx, r)
	if err != nil {
		return err
	}
	y, err := u.d.Sequences.ExportYAML(ctx, seq.ID)
	if err != nil {
		return err
	}
	d := yamlExportData{Seq: seq, Slug: yamlSlug(seq.Name), YAML: y.YAML}
	for n, b := range y.Files {
		d.Files = append(d.Files, yamlFile{Name: n, Body: b})
	}
	sort.Slice(d.Files, func(i, j int) bool { return d.Files[i].Name < d.Files[j].Name })
	if d.Tabs, err = seqTabs(seq, "export"); err != nil {
		return err
	}
	return u.Page(w, r, "automation/seq_export", View{Title: "Export", Subtitle: seq.Name, Nav: "sequences",
		Crumbs: seqCrumbs(seq), Data: d})
}

var yamlSlugUnsafe = regexp.MustCompile(`[^a-z0-9]+`)

// yamlSlug is a file name for a sequence ("community-welcome").
func yamlSlug(name string) string {
	s := strings.Trim(yamlSlugUnsafe.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if s == "" {
		s = "sequence"
	}
	return truncate(60, s)
}

// yamlDownload sends the export as a zip: <slug>.yaml and the body files,
// ready for import-sequence.
func (u *UI) yamlDownload(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	seq, err := u.seqGet(ctx, r)
	if err != nil {
		return err
	}
	y, err := u.d.Sequences.ExportYAML(ctx, seq.ID)
	if err != nil {
		return err
	}
	slug := yamlSlug(seq.Name)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	names := make([]string, 0, len(y.Files))
	for n := range y.Files {
		names = append(names, n)
	}
	sort.Strings(names)
	entries := []yamlFile{{Name: slug + ".yaml", Body: y.YAML}}
	for _, n := range names {
		entries = append(entries, yamlFile{Name: path.Base(n), Body: y.Files[n]})
	}
	for _, e := range entries {
		fw, err := zw.CreateHeader(&zip.FileHeader{Name: e.Name, Method: zip.Deflate, Modified: seq.UpdatedAt})
		if err != nil {
			return err
		}
		if _, err := io.WriteString(fw, e.Body); err != nil {
			return err
		}
	}
	if err := zw.Close(); err != nil {
		return err
	}
	h := w.Header()
	h.Set("Content-Type", "application/zip")
	h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.zip"`, slug))
	_, _ = w.Write(buf.Bytes())
	return nil
}
