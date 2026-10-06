package sequences_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mailroom/internal/core"
	"mailroom/internal/sequences"
)

const welcomeYAML = `
name: Community welcome
trigger:
  tag: joined-community
exit_tags: [customer, Refunded]
exit_add_tag: community-drip-exited
reentry: after_completion
send_window:
  days: [mon, tue, wed, thu, Friday]
  start: "08:00"
  end: "20:00"
steps:
  - delay: 0
    subject: Welcome to the community, {{first_name | "friend"}}!
    preheader: Here is what happens next
    body_file: 01-welcome.md
  - delay: 90m
    subject: Your first win
    body: |
      Hi {{first_name | "there"}},

      Inline body.
  - delay: 2d
    subject: A gift
    format: html
    body_file: 03-gift.html
`

var welcomeFiles = map[string]string{
	"01-welcome.md": "Hi {{first_name | \"there\"}},\n\nThanks for joining.\n",
	"03-gift.html":  "<p>Here is <a href=\"https://example.com/gift\">your gift</a>.</p>",
}

func TestParseYAML(t *testing.T) {
	def, err := sequences.ParseYAML(welcomeYAML, welcomeFiles)
	if err != nil {
		t.Fatal(err)
	}
	in := def.Sequence
	if in.Name != "Community welcome" || *in.TriggerTag != "joined-community" || *in.ExitAddTag != "community-drip-exited" ||
		in.Reentry != "after_completion" || in.Status != "active" || !reflect.DeepEqual(in.ExitTags, []string{"customer", "Refunded"}) {
		t.Fatalf("sequence = %+v", in)
	}
	if w := in.SendWindow; w == nil || !reflect.DeepEqual(w.Days, []int{1, 2, 3, 4, 5}) || w.Start != "08:00" || w.End != "20:00" {
		t.Fatalf("window = %+v", in.SendWindow)
	}
	wantDelays := []int64{0, 5400, 172800}
	for i, st := range def.Steps {
		if st.DelaySeconds != wantDelays[i] {
			t.Errorf("step %d delay = %d", i, st.DelaySeconds)
		}
	}
	if def.Steps[0].Email.Body != welcomeFiles["01-welcome.md"] || def.Steps[2].Email.Format != "html" || def.Steps[1].Email.Format != "markdown" ||
		!strings.Contains(def.Steps[1].Email.Body, "Inline body.") {
		t.Fatalf("steps = %+v", def.Steps)
	}
}

func TestParseYAMLErrors(t *testing.T) {
	base := "name: S\nsteps:\n  - delay: 1h\n    subject: Hi\n    body: Hello\n"
	tests := []struct {
		name  string
		yaml  string
		files map[string]string
		field string
	}{
		{"empty", "", nil, "yaml"},
		{"syntax", "name: [unclosed", nil, "yaml"},
		{"unknown key", base + "colour: blue\n", nil, "yaml"},
		{"two documents", base + "---\nname: T\n", nil, "yaml"},
		{"no name", "steps:\n  - delay: 0\n    subject: Hi\n    body: x\n", nil, "name"},
		{"no steps", "name: S\n", nil, "steps"},
		{"bad delay", "name: S\nsteps:\n  - delay: 3 weeks\n    subject: Hi\n    body: x\n", nil, "steps[0].delay"},
		{"missing delay", "name: S\nsteps:\n  - subject: Hi\n    body: x\n", nil, "steps[0].delay"},
		{"no subject", "name: S\nsteps:\n  - delay: 0\n    body: x\n", nil, "steps[0].subject"},
		{"no body", "name: S\nsteps:\n  - delay: 0\n    subject: Hi\n", nil, "steps[0].body"},
		{"bad merge field", "name: S\nsteps:\n  - delay: 0\n    subject: Hi\n    body: '{{shoe_size}}'\n", nil, "steps[0].body"},
		{"bad format", "name: S\nsteps:\n  - delay: 0\n    subject: Hi\n    format: rtf\n    body: x\n", nil, "steps[0].format"},
		{"body and file", "name: S\nsteps:\n  - delay: 0\n    subject: Hi\n    body: x\n    body_file: a.md\n", map[string]string{"a.md": "x"}, "steps[0].body_file"},
		{"missing file", "name: S\nsteps:\n  - delay: 0\n    subject: Hi\n    body_file: a.md\n", nil, "steps[0].body_file"},
		{"unreferenced file", base, map[string]string{"extra.md": "x"}, "files.extra.md"},
		{"bad file name", base, map[string]string{"../x.md": "x"}, "files"},
		{"bad reentry", base + "reentry: sometimes\n", nil, "reentry"},
		{"bad status", base + "status: gone\n", nil, "status"},
		{"empty trigger tag", base + "trigger:\n  tag: ''\n", nil, "trigger.tag"},
		{"trigger is exit tag", base + "trigger:\n  tag: a\nexit_tags: [A]\n", nil, "exit_tags"},
		{"bad weekday", base + "send_window:\n  days: [mon, funday]\n  start: '08:00'\n  end: '20:00'\n", nil, "send_window.days[1]"},
		{"window end before start", base + "send_window:\n  days: [mon]\n  start: '20:00'\n  end: '08:00'\n", nil, "send_window.end"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := sequences.ParseYAML(tc.yaml, tc.files)
			if !errors.Is(err, core.ErrValidation) || fieldOf(err) != tc.field {
				t.Fatalf("err = %v (field %q), want field %q", err, fieldOf(err), tc.field)
			}
		})
	}
}

func TestYAMLRoundTrip(t *testing.T) {
	hs := newHarness(t)
	want, err := sequences.ParseYAML(welcomeYAML, welcomeFiles)
	if err != nil {
		t.Fatal(err)
	}
	seq, err := hs.svc.ImportYAML(hs.ctx, welcomeYAML, welcomeFiles, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(seq.Steps) != 3 || seq.Steps[1].DelaySeconds != 5400 || seq.Steps[1].Delay != "90m" || seq.Steps[2].Format != "html" {
		t.Fatalf("imported = %+v", seq)
	}
	out, err := hs.svc.ExportYAML(hs.ctx, seq.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{`delay: "0"`, `delay: "90m"`, `delay: "2d"`, `start: "08:00"`, "days: [mon, tue, wed, thu, fri]", "body_file: 02-your-first-win.md"} {
		if !strings.Contains(out.YAML, s) {
			t.Errorf("export lacks %q:\n%s", s, out.YAML)
		}
	}
	got, err := sequences.ParseYAML(out.YAML, out.Files)
	if err != nil {
		t.Fatalf("re-parse export: %v\n%s", err, out.YAML)
	}
	// Tag names come back in their stored spelling.
	want.Sequence.ExitTags = []string{"customer", "Refunded"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("export(import(x)) != x\ngot  %+v\nwant %+v", got, want)
	}

	// Re-importing the export under another name gives the same export.
	renamed := strings.Replace(out.YAML, "name: Community welcome", "name: Community welcome 2", 1)
	seq2, err := hs.svc.ImportYAML(hs.ctx, renamed, out.Files, false)
	if err != nil {
		t.Fatal(err)
	}
	out2, err := hs.svc.ExportYAML(hs.ctx, seq2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if out2.YAML != renamed || !reflect.DeepEqual(out2.Files, out.Files) {
		t.Fatalf("second export differs:\n%s\n---\n%s", out2.YAML, renamed)
	}

	// A duplicate name is a conflict and leaves nothing behind.
	emails := hs.count(`SELECT count(*) FROM emails`)
	if _, err := hs.svc.ImportYAML(hs.ctx, welcomeYAML, welcomeFiles, false); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("duplicate import: %v", err)
	}
	if n := hs.count(`SELECT count(*) FROM emails`); n != emails {
		t.Fatalf("emails %d -> %d after a failed import", emails, n)
	}
}

func TestImportYAMLDryRun(t *testing.T) {
	hs := newHarness(t)
	seq, err := hs.svc.ImportYAML(hs.ctx, welcomeYAML, welcomeFiles, true)
	if err != nil {
		t.Fatal(err)
	}
	if seq.Name != "Community welcome" || len(seq.Steps) != 3 {
		t.Fatalf("dry run = %+v", seq)
	}
	for _, table := range []string{"sequences", "sequence_steps", "emails", "tags"} {
		if n := hs.count(`SELECT count(*) FROM ` + table); n != 0 {
			t.Errorf("dry run left %d row(s) in %s", n, table)
		}
	}
}

func TestDelayNotation(t *testing.T) {
	parse := map[string]int64{"0": 0, "0s": 0, "45s": 45, "90m": 5400, "36h": 129600, "2d": 172800, " 1D ": 86400, "365d": 31536000}
	for in, want := range parse {
		if got, err := sequences.ParseDelay(in); err != nil || got != want {
			t.Errorf("ParseDelay(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "1", "-1h", "+1h", "1.5h", "h", "2w", "366d", "1d2h"} {
		if _, err := sequences.ParseDelay(bad); err == nil {
			t.Errorf("ParseDelay(%q) should fail", bad)
		}
	}
	format := map[int64]string{0: "0", 45: "45s", 60: "1m", 5400: "90m", 3600: "1h", 129600: "36h", 172800: "2d", 90061: "90061s"}
	for in, want := range format {
		if got := sequences.FormatDelay(in); got != want {
			t.Errorf("FormatDelay(%d) = %q, want %q", in, got, want)
		}
	}
}

// TestDocExample checks that the example in docs/sequence-format.md
// imports as documented.
func TestDocExample(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "sequence-format.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	start := strings.Index(doc, "```yaml\n")
	end := strings.Index(doc[start+8:], "```")
	if start < 0 || end < 0 {
		t.Fatal("no yaml example in the doc")
	}
	example := doc[start+8 : start+8+end]
	files := map[string]string{}
	for _, line := range strings.Split(example, "\n") {
		if name, ok := strings.CutPrefix(strings.TrimSpace(line), "body_file: "); ok {
			files[name] = "Hello {{first_name | \"there\"}}"
		}
	}
	def, err := sequences.ParseYAML(example, files)
	if err != nil {
		t.Fatalf("doc example: %v", err)
	}
	if len(def.Steps) != 4 || def.Steps[2].DelaySeconds != 36*3600 || *def.Sequence.TriggerTag != "joined-community" {
		t.Fatalf("doc example = %+v", def)
	}
	hs := newHarness(t)
	seq, err := hs.svc.ImportYAML(hs.ctx, example, files, false)
	if err != nil {
		t.Fatal(err)
	}
	out, err := hs.svc.ExportYAML(hs.ctx, seq.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := out.Files["01-welcome-to-the-community-first-name.md"]; !ok {
		t.Fatalf("export files = %v", out.Files)
	}
}
