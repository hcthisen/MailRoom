package sequences

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"mailroom/internal/content"
	"mailroom/internal/core"
	"mailroom/internal/settings"
)

// YAML import limits.
const (
	MaxYAMLBytes  = 256 << 10
	MaxYAMLFiles  = MaxSteps
	MaxFileName   = 200
	maxTotalFiles = 16 << 20
)

// yamlSequence is the YAML sequence format (I-3, docs/sequence-format.md).
type yamlSequence struct {
	Name       string       `yaml:"name"`
	Status     string       `yaml:"status,omitempty"`
	Trigger    *yamlTrigger `yaml:"trigger,omitempty"`
	ExitTags   []string     `yaml:"exit_tags,omitempty"`
	ExitAddTag string       `yaml:"exit_add_tag,omitempty"`
	Reentry    string       `yaml:"reentry,omitempty"`
	SendWindow *yamlWindow  `yaml:"send_window,omitempty"`
	Steps      []yamlStep   `yaml:"steps"`
}

type yamlTrigger struct {
	Tag string `yaml:"tag"`
}

type yamlWindow struct {
	Days  []string `yaml:"days,flow"`
	Start quoted   `yaml:"start"`
	End   quoted   `yaml:"end"`
}

type yamlStep struct {
	Delay     quoted `yaml:"delay"`
	Subject   string `yaml:"subject"`
	Preheader string `yaml:"preheader,omitempty"`
	Format    string `yaml:"format,omitempty"`
	Body      string `yaml:"body,omitempty"`
	BodyFile  string `yaml:"body_file,omitempty"`
}

// quoted is a string written double-quoted, so "08:00" and "0" stay
// strings for every YAML reader.
type quoted string

// MarshalYAML writes the value double-quoted.
func (q quoted) MarshalYAML() (any, error) {
	return &yaml.Node{Kind: yaml.ScalarNode, Style: yaml.DoubleQuotedStyle, Value: string(q)}, nil
}

// weekdays maps YAML day names to ISO weekdays.
var weekdays = map[string]int{
	"mon": 1, "monday": 1, "tue": 2, "tuesday": 2, "wed": 3, "wednesday": 3, "thu": 4, "thursday": 4,
	"fri": 5, "friday": 5, "sat": 6, "saturday": 6, "sun": 7, "sunday": 7,
}

var dayNames = [...]string{1: "mon", 2: "tue", 3: "wed", 4: "thu", 5: "fri", 6: "sat", 7: "sun"}

// Definition is a parsed and validated YAML sequence: its configuration
// and its steps' content.
type Definition struct {
	Sequence SequenceInput
	Steps    []DefinitionStep
}

// DefinitionStep is one step of a Definition.
type DefinitionStep struct {
	DelaySeconds int64
	Email        StepEmail
}

// ParseYAML parses and validates a sequence in the YAML format. files
// holds the contents of the body_file references by file name; every file
// must be referenced by a step. Errors are validation errors whose field
// points into the document (e.g. "steps[2].delay") or at a file
// ("files.03-offer.md").
func ParseYAML(doc string, files map[string]string) (Definition, error) {
	if len(doc) > MaxYAMLBytes {
		return Definition{}, core.Invalid("yaml", "is larger than %d bytes", MaxYAMLBytes)
	}
	if err := checkFiles(files); err != nil {
		return Definition{}, err
	}
	var y yamlSequence
	dec := yaml.NewDecoder(strings.NewReader(doc))
	dec.KnownFields(true)
	if err := dec.Decode(&y); err != nil {
		if errors.Is(err, io.EOF) {
			return Definition{}, core.Invalid("yaml", "is empty")
		}
		return Definition{}, core.Invalid("yaml", "%s", strings.TrimPrefix(err.Error(), "yaml: "))
	}
	var extra yamlSequence
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return Definition{}, core.Invalid("yaml", "must contain exactly one document")
	}
	return y.definition(files)
}

// checkFiles bounds the attached files.
func checkFiles(files map[string]string) error {
	if len(files) > MaxYAMLFiles {
		return core.Invalid("files", "at most %d files are allowed", MaxYAMLFiles)
	}
	total := 0
	for name, body := range files {
		if name == "" || len(name) > MaxFileName || strings.ContainsAny(name, "/\\") {
			return core.Invalid("files", "file name %q must be 1-%d characters without slashes", name, MaxFileName)
		}
		if len(body) > content.MaxBodyBytes {
			return core.Invalid("files."+name, "is larger than %d bytes", content.MaxBodyBytes)
		}
		total += len(body)
	}
	if total > maxTotalFiles {
		return core.Invalid("files", "are larger than %d bytes together", maxTotalFiles)
	}
	return nil
}

// definition validates y and resolves its body files.
func (y yamlSequence) definition(files map[string]string) (Definition, error) {
	in := SequenceInput{Name: y.Name, Status: y.Status, ExitTags: y.ExitTags, Reentry: y.Reentry}
	if y.Trigger != nil {
		if strings.TrimSpace(y.Trigger.Tag) == "" {
			return Definition{}, core.Invalid("trigger.tag", "is required when trigger is given")
		}
		t := y.Trigger.Tag
		in.TriggerTag = &t
	}
	if y.ExitAddTag != "" {
		t := y.ExitAddTag
		in.ExitAddTag = &t
	}
	if y.SendWindow != nil {
		w, err := y.SendWindow.window()
		if err != nil {
			return Definition{}, err
		}
		in.SendWindow = &w
	}
	n, err := normalize(in)
	if err != nil {
		if f, ok := validationField(err); ok && f == "trigger_tag" {
			return Definition{}, retarget(err, "trigger.tag")
		}
		return Definition{}, err
	}
	def := Definition{Sequence: n.input()}
	if len(y.Steps) == 0 {
		return Definition{}, core.Invalid("steps", "at least one step is required")
	}
	if len(y.Steps) > MaxSteps {
		return Definition{}, core.Invalid("steps", "at most %d steps are allowed", MaxSteps)
	}
	used := map[string]bool{}
	for i, st := range y.Steps {
		field := fmt.Sprintf("steps[%d]", i)
		d, err := ParseDelay(string(st.Delay))
		if err != nil {
			return Definition{}, core.Invalid(field+".delay", "%v", err)
		}
		body := st.Body
		switch {
		case st.Body != "" && st.BodyFile != "":
			return Definition{}, core.Invalid(field+".body_file", "give either body or body_file")
		case st.BodyFile != "":
			b, ok := files[st.BodyFile]
			if !ok {
				return Definition{}, core.Invalid(field+".body_file", "file %q is not among the files", st.BodyFile)
			}
			used[st.BodyFile] = true
			body = b
		}
		e, err := content.ValidateEmail(content.EmailInput{
			Name: "step", Subject: st.Subject, Preheader: st.Preheader, Format: st.Format, Body: body,
		})
		if err != nil {
			return Definition{}, prefixField(err, field+".")
		}
		def.Steps = append(def.Steps, DefinitionStep{DelaySeconds: d, Email: StepEmail{
			Subject: e.Subject, Preheader: e.Preheader, Format: e.Format, Body: e.Body,
		}})
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !used[name] {
			return Definition{}, core.Invalid("files."+name, "is not referenced by any step's body_file")
		}
	}
	return def, nil
}

// window converts a YAML send window.
func (w yamlWindow) window() (settings.SendWindow, error) {
	out := settings.SendWindow{Start: string(w.Start), End: string(w.End)}
	for i, d := range w.Days {
		n, ok := weekdays[strings.ToLower(strings.TrimSpace(d))]
		if !ok {
			return out, core.Invalid(fmt.Sprintf("send_window.days[%d]", i), "must be a weekday: mon, tue, wed, thu, fri, sat or sun")
		}
		out.Days = append(out.Days, n)
	}
	return out, nil
}

// input returns the normalised configuration as a SequenceInput.
func (n normalized) input() SequenceInput {
	in := SequenceInput{Name: n.Name, Status: n.Status, ExitTags: n.ExitTags, Reentry: n.Reentry, SendWindow: n.Window}
	if n.TriggerTag != "" {
		t := n.TriggerTag
		in.TriggerTag = &t
	}
	if n.ExitAddTag != "" {
		t := n.ExitAddTag
		in.ExitAddTag = &t
	}
	return in
}

// SequenceYAML is a sequence in the YAML format with its body files.
type SequenceYAML struct {
	YAML  string            `json:"yaml" doc:"The sequence definition (docs/sequence-format.md)" example:"name: Community welcome\nsteps:\n  - delay: \"0\"\n    subject: Welcome\n    body_file: 01-welcome.md\n"`
	Files map[string]string `json:"files" doc:"Step bodies by file name, referenced by body_file" example:"{\"01-welcome.md\":\"Hi {{first_name | \\\"there\\\"}}!\"}"`
}

// marshalDefinition writes a definition in the YAML format, with each
// step's body in its own file.
func marshalDefinition(def Definition) (SequenceYAML, error) {
	in := def.Sequence
	y := yamlSequence{Name: in.Name, Status: in.Status, ExitTags: in.ExitTags, Reentry: in.Reentry}
	if in.TriggerTag != nil {
		y.Trigger = &yamlTrigger{Tag: *in.TriggerTag}
	}
	if in.ExitAddTag != nil {
		y.ExitAddTag = *in.ExitAddTag
	}
	if w := in.SendWindow; w != nil {
		yw := &yamlWindow{Start: quoted(w.Start), End: quoted(w.End)}
		days := slices.Clone(w.Days)
		slices.Sort(days)
		for _, d := range days {
			yw.Days = append(yw.Days, dayNames[d])
		}
		y.SendWindow = yw
	}
	out := SequenceYAML{Files: map[string]string{}}
	for i, st := range def.Steps {
		name := stepFileName(i+1, st.Email.Subject, st.Email.Format)
		out.Files[name] = st.Email.Body
		y.Steps = append(y.Steps, yamlStep{
			Delay: quoted(FormatDelay(st.DelaySeconds)), Subject: st.Email.Subject, Preheader: st.Email.Preheader,
			Format: st.Email.Format, BodyFile: name,
		})
	}
	var buf bytes.Buffer
	buf.WriteString("# Mailroom sequence. Format: docs/sequence-format.md\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(y); err != nil {
		return SequenceYAML{}, fmt.Errorf("sequences: encode yaml: %w", err)
	}
	if err := enc.Close(); err != nil {
		return SequenceYAML{}, fmt.Errorf("sequences: encode yaml: %w", err)
	}
	out.YAML = buf.String()
	return out, nil
}

// stepFileName is the body file name of step position: "02-subject-slug.md".
func stepFileName(position int, subject, format string) string {
	ext := "md"
	if format == content.FormatHTML {
		ext = "html"
	}
	const maxSlug = 40
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(subject) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
			continue
		}
		dash = true
	}
	slug := b.String()
	if len(slug) > maxSlug {
		// Cut at a word boundary when there is one.
		cut := slug[:maxSlug+1]
		if i := strings.LastIndexByte(cut, '-'); i > 0 {
			slug = cut[:i]
		} else {
			slug = slug[:maxSlug]
		}
	}
	if slug == "" {
		slug = "step"
	}
	return fmt.Sprintf("%02d-%s.%s", position, slug, ext)
}
