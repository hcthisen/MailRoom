package provision

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

func TestMergeEnv(t *testing.T) {
	tests := []struct {
		name    string
		content string
		set     map[string]string
		want    string
	}{
		{
			name:    "empty file",
			content: "",
			set:     map[string]string{"AWS_REGION": "eu-west-1", "AWS_ACCESS_KEY_ID": "AKIA1"},
			want:    "# Written by mailroom provision ses\nAWS_ACCESS_KEY_ID=AKIA1\nAWS_REGION=eu-west-1\n",
		},
		{
			name: "updates in place and keeps comments and order",
			content: "# Mailroom settings\n" +
				"DOMAIN=example.com\n" +
				"\n" +
				"# AWS\n" +
				"AWS_REGION=us-east-1 # old\n" +
				"export AWS_ACCESS_KEY_ID='AKIAOLD'\n" +
				"FROM_NAME=\"Example Co\"\n",
			set: map[string]string{"AWS_REGION": "eu-west-1", "AWS_ACCESS_KEY_ID": "AKIANEW", "SNS_TOPIC_ARN": "arn:t"},
			want: "# Mailroom settings\n" +
				"DOMAIN=example.com\n" +
				"\n" +
				"# AWS\n" +
				"AWS_REGION=eu-west-1\n" +
				"export AWS_ACCESS_KEY_ID=AKIANEW\n" +
				"FROM_NAME=\"Example Co\"\n" +
				"\n" +
				"# Written by mailroom provision ses\n" +
				"SNS_TOPIC_ARN=arn:t\n",
		},
		{
			name:    "every duplicate is rewritten",
			content: "BACKUP_BUCKET=a\nBACKUP_BUCKET=b\n",
			set:     map[string]string{"BACKUP_BUCKET": "c"},
			want:    "BACKUP_BUCKET=c\nBACKUP_BUCKET=c\n",
		},
		{
			name:    "no trailing newline and commented-out key",
			content: "#AWS_REGION=x\nDOMAIN=example.com",
			set:     map[string]string{"AWS_REGION": "eu-west-1"},
			want:    "#AWS_REGION=x\nDOMAIN=example.com\n\n# Written by mailroom provision ses\nAWS_REGION=eu-west-1\n",
		},
		{
			name:    "CRLF is kept",
			content: "DOMAIN=example.com\r\nAWS_REGION=x\r\n",
			set:     map[string]string{"AWS_REGION": "eu-west-1"},
			want:    "DOMAIN=example.com\r\nAWS_REGION=eu-west-1\r\n",
		},
		{
			name:    "empty value and quoting",
			content: "BACKUP_BUCKET=old\n",
			set:     map[string]string{"BACKUP_BUCKET": "", "X_ODD": `a b"$c`},
			want:    "BACKUP_BUCKET=\n\n# Written by mailroom provision ses\nX_ODD=\"a b\\\"\\$c\"\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MergeEnv(tt.content, tt.set)
			if got != tt.want {
				t.Errorf("MergeEnv =\n%q\nwant\n%q", got, tt.want)
			}
			// Merging again changes nothing.
			if again := MergeEnv(got, tt.set); again != got {
				t.Errorf("second merge changed the file:\n%q", again)
			}
			// What was set reads back.
			parsed := parseEnv(got)
			for k, v := range tt.set {
				if parsed[k] != v {
					t.Errorf("%s reads back as %q, want %q", k, parsed[k], v)
				}
			}
		})
	}
}

func TestParseEnv(t *testing.T) {
	got := parseEnv("# c\nA=1\nexport B = two \nC='x # y'\nD=\"q\\\"z\"\nE=v # comment\nnot a line\n=bad\nA=3\r\n")
	want := map[string]string{"A": "3", "B": "two", "C": "x # y", "D": `q"z`, "E": "v"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseEnv = %v, want %v", got, want)
	}
}

func TestWriteEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", ".env")

	changed, err := WriteEnvFile(path, map[string]string{"AWS_REGION": "eu-west-1"})
	if err != nil || !changed {
		t.Fatalf("create: changed=%v err=%v", changed, err)
	}
	assertMode(t, path)

	changed, err = WriteEnvFile(path, map[string]string{"AWS_REGION": "eu-west-1"})
	if err != nil || changed {
		t.Fatalf("same content: changed=%v err=%v", changed, err)
	}

	// A wrong mode alone is fixed and reported.
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		changed, err = WriteEnvFile(path, map[string]string{"AWS_REGION": "eu-west-1"})
		if err != nil || !changed {
			t.Fatalf("mode fix: changed=%v err=%v", changed, err)
		}
		assertMode(t, path)
	}

	changed, err = WriteEnvFile(path, map[string]string{"AWS_REGION": "eu-north-1"})
	if err != nil || !changed {
		t.Fatalf("update: changed=%v err=%v", changed, err)
	}
	env, err := ReadEnvFile(path)
	if err != nil || env["AWS_REGION"] != "eu-north-1" {
		t.Fatalf("env = %v err %v", env, err)
	}
	assertMode(t, path)

	// No temp files are left behind.
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("directory holds %d entries, want only .env", len(entries))
	}
}

func TestWriteEnvFileFollowsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	dir := t.TempDir()
	real := filepath.Join(dir, "real.env")
	if err := os.WriteFile(real, []byte("A=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, ".env")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteEnvFile(link, map[string]string{"AWS_REGION": "x"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(link)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the symlink was replaced")
	}
	env, _ := ReadEnvFile(real)
	if env["AWS_REGION"] != "x" || env["A"] != "1" {
		t.Errorf("target = %v", env)
	}
}

func TestReadEnvFileMissing(t *testing.T) {
	env, err := ReadEnvFile(filepath.Join(t.TempDir(), "none"))
	if err != nil || len(env) != 0 {
		t.Errorf("env = %v err %v", env, err)
	}
}

// TestWriteEnvFileRenameFails covers a single-file bind mount (rename onto
// it gives EBUSY) and other devices (EXDEV), which fall back to rewriting
// the file in place, and other rename errors, which fail cleanly.
func TestWriteEnvFileRenameFails(t *testing.T) {
	const original = "# keep me\nDOMAIN=example.com\n"
	tests := []struct {
		name     string
		errno    syscall.Errno
		exists   bool
		wantErr  bool
		wantFile string
	}{
		{"EBUSY rewrites in place", syscall.EBUSY, true, false, original + "\n# Written by mailroom provision ses\nAWS_REGION=eu-west-1\n"},
		{"EXDEV rewrites in place", syscall.EXDEV, true, false, original + "\n# Written by mailroom provision ses\nAWS_REGION=eu-west-1\n"},
		{"EBUSY without a file fails", syscall.EBUSY, false, true, ""},
		{"other errors fail", syscall.EACCES, true, true, original},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, ".env")
			if tt.exists {
				if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			failRename(t, tt.errno)
			changed, err := WriteEnvFile(path, map[string]string{"AWS_REGION": "eu-west-1"})
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "replace env file") {
					t.Fatalf("err = %v, want the rename error", err)
				}
			} else {
				if err != nil || !changed {
					t.Fatalf("changed=%v err=%v", changed, err)
				}
				assertMode(t, path)
			}
			if tt.exists {
				raw, _ := os.ReadFile(path)
				if string(raw) != tt.wantFile {
					t.Errorf("file = %q, want %q", raw, tt.wantFile)
				}
			}
			entries, _ := os.ReadDir(dir)
			want := 0
			if tt.exists {
				want = 1
			}
			if len(entries) != want {
				t.Errorf("directory holds %d entries, want %d (temp file left behind?)", len(entries), want)
			}
		})
	}
}

// TestWriteEnvFileDirectoryNotWritable covers the container case: the env
// file is bind-mounted and owned by the container's non-root user, but its
// directory was created by the container runtime as root, so no temporary
// file can be created next to it. The file is rewritten in place. Without
// an existing file, or on other errors, the write fails cleanly.
func TestWriteEnvFileDirectoryNotWritable(t *testing.T) {
	const original = "DOMAIN=example.com\n"
	tests := []struct {
		name    string
		errno   syscall.Errno
		exists  bool
		wantErr bool
	}{
		{"EACCES rewrites in place", syscall.EACCES, true, false},
		{"EPERM rewrites in place", syscall.EPERM, true, false},
		{"no file to rewrite fails", syscall.EACCES, false, true},
		{"other errors fail", syscall.ENOSPC, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, ".env")
			if tt.exists {
				if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			orig := createTemp
			createTemp = func(dir, pattern string) (*os.File, error) {
				return nil, &os.PathError{Op: "createtemp", Path: filepath.Join(dir, pattern), Err: tt.errno}
			}
			t.Cleanup(func() { createTemp = orig })

			changed, err := WriteEnvFile(path, map[string]string{"AWS_REGION": "eu-west-1"})
			raw, _ := os.ReadFile(path)
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "write env file") {
					t.Fatalf("err = %v, want the temp file error", err)
				}
				if tt.exists && string(raw) != original {
					t.Fatalf("file changed on failure: %q", raw)
				}
				return
			}
			if err != nil || !changed {
				t.Fatalf("changed=%v err=%v", changed, err)
			}
			if want := original + "\n# Written by mailroom provision ses\nAWS_REGION=eu-west-1\n"; string(raw) != want {
				t.Fatalf("file = %q, want %q", raw, want)
			}
			assertMode(t, path)
		})
	}
}
