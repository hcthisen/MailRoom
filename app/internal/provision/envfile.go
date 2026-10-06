package provision

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
)

// EnvFileMode is the permission of the server env file: it holds secrets.
const EnvFileMode fs.FileMode = 0o600

// envKeys are the variables "provision ses" owns in the env file, in the
// order they are appended when missing.
var envKeys = []string{
	"AWS_ACCESS_KEY_ID",
	"AWS_SECRET_ACCESS_KEY",
	"AWS_REGION",
	"SNS_TOPIC_ARN",
	"SES_CONFIGURATION_SET",
	"BACKUP_BUCKET",
}

// envLine matches "KEY=value" and "export KEY=value" assignments.
var envLine = regexp.MustCompile(`^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=(.*)$`)

// ReadEnvFile parses the assignments of an env file. A missing file is an
// empty map. Values lose surrounding quotes and trailing comments on
// unquoted values; later assignments win.
func ReadEnvFile(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read env file: %w", err)
	}
	return parseEnv(string(b)), nil
}

func parseEnv(content string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(content, "\n") {
		m := envLine.FindStringSubmatch(strings.TrimSuffix(line, "\r"))
		if m == nil {
			continue
		}
		out[m[1]] = envValue(m[2])
	}
	return out
}

// envValue unquotes a raw value.
func envValue(raw string) string {
	v := strings.TrimSpace(raw)
	if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
		return v[1 : len(v)-1]
	}
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		return envUnescape.Replace(v[1 : len(v)-1])
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return v
}

// MergeEnv returns content with each key of set assigned its value: every
// existing assignment of the key is rewritten in place and missing keys are
// appended (in envKeys order, then alphabetically). All other lines,
// comments and the file's line endings are kept.
func MergeEnv(content string, set map[string]string) string {
	nl := "\n"
	if strings.Contains(content, "\r\n") {
		nl = "\r\n"
	}
	body := strings.TrimSuffix(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	var lines []string
	if body != "" || content != "" {
		lines = strings.Split(body, "\n")
	}
	seen := map[string]bool{}
	for i, line := range lines {
		m := envLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		v, ok := set[m[1]]
		if !ok {
			continue
		}
		prefix := ""
		if strings.HasPrefix(strings.TrimSpace(line), "export ") {
			prefix = "export "
		}
		lines[i] = prefix + m[1] + "=" + quoteEnv(v)
		seen[m[1]] = true
	}
	var missing []string
	for _, k := range envKeys {
		if _, ok := set[k]; ok && !seen[k] {
			missing = append(missing, k)
			seen[k] = true
		}
	}
	var rest []string
	for k := range set {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	slices.Sort(rest)
	missing = append(missing, rest...)
	if len(missing) > 0 {
		if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
			lines = append(lines, "")
		}
		lines = append(lines, "# Written by mailroom provision ses")
		for _, k := range missing {
			lines = append(lines, k+"="+quoteEnv(set[k]))
		}
	}
	return strings.Join(lines, nl) + nl
}

// quoteEnv quotes a value only when it would not survive unquoted.
func quoteEnv(v string) string {
	if v == "" || !strings.ContainsAny(v, " \t#\"'$`\\") {
		return v
	}
	return `"` + envEscape.Replace(v) + `"`
}

var (
	envEscape   = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`")
	envUnescape = strings.NewReplacer(`\\`, `\`, `\"`, `"`, `\$`, "$", "\\`", "`")
)

// WriteEnvFile merges set into the env file at path, creating it (and its
// directory) when missing. The file is replaced atomically when possible
// and always ends up with mode 0600. It reports whether anything changed.
//
// When the file cannot be replaced by a rename because it is a mount point
// of its own (a single-file bind mount into a container gives EBUSY), lies
// on another device (EXDEV), or its directory is not writable although the
// file is (a bind-mounted file owned by the container's non-root user, in
// a directory the container runtime created as root), it is rewritten in
// place instead.
func WriteEnvFile(path string, set map[string]string) (changed bool, err error) {
	target := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		target = resolved
	}
	old, err := os.ReadFile(target)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("read env file: %w", err)
	}
	merged := MergeEnv(string(old), set)
	if exists && merged == string(old) {
		fi, err := os.Stat(target)
		if err != nil {
			return false, fmt.Errorf("stat env file: %w", err)
		}
		if fi.Mode().Perm() == EnvFileMode {
			return false, nil
		}
		if err := os.Chmod(target, EnvFileMode); err != nil {
			return false, fmt.Errorf("chmod env file: %w", err)
		}
		return true, nil
	}
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, fmt.Errorf("create env file directory: %w", err)
	}
	if err := replaceFile(target, merged); err != nil {
		inPlace := errors.Is(err, syscall.EBUSY) || errors.Is(err, syscall.EXDEV) || errors.Is(err, errNoTempFile)
		if !exists || !inPlace {
			return false, err
		}
		if err := writeInPlace(target, merged); err != nil {
			return false, err
		}
	}
	if err := os.Chmod(target, EnvFileMode); err != nil {
		return false, fmt.Errorf("chmod env file: %w", err)
	}
	return true, nil
}

// renameFile is os.Rename and createTemp os.CreateTemp; tests replace
// them to simulate mount points and read-only directories.
var (
	renameFile = os.Rename
	createTemp = os.CreateTemp
)

// errNoTempFile marks a replace that failed because the directory refused
// the temporary file (permission denied), before anything was written.
var errNoTempFile = errors.New("the env file's directory does not accept a temporary file")

// replaceFile writes content to a 0600 temporary file next to target and
// renames it over target. The temporary file never outlives a failure.
func replaceFile(target, content string) (err error) {
	tmp, err := createTemp(filepath.Dir(target), ".env.mailroom-*")
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return fmt.Errorf("write env file: %w: %w", errNoTempFile, err)
		}
		return fmt.Errorf("write env file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpName)
		}
	}()
	if err = tmp.Chmod(EnvFileMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod env file: %w", err)
	}
	if _, err = tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write env file: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write env file: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("write env file: %w", err)
	}
	if err = renameFile(tmpName, target); err != nil {
		return fmt.Errorf("replace env file: %w", err)
	}
	return nil
}

// writeInPlace truncates and rewrites the existing target, restricting its
// mode before the secrets are written.
func writeInPlace(target, content string) error {
	if err := os.Chmod(target, EnvFileMode); err != nil {
		return fmt.Errorf("chmod env file: %w", err)
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_TRUNC, EnvFileMode)
	if err != nil {
		return fmt.Errorf("write env file in place: %w", err)
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		return fmt.Errorf("write env file in place: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("write env file in place: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write env file in place: %w", err)
	}
	return nil
}

// CheckEnvFileWritable reports whether WriteEnvFile can store the keys at
// path: an existing file must be readable and writable by this process
// (WriteEnvFile falls back to rewriting it in place), and a missing one
// must be creatable in its directory. It changes nothing. The error says
// how to fix the usual cause: an env file that doesn't belong to the
// container's user (uid 65532 in the Mailroom image).
func CheckEnvFileWritable(path string) error {
	target := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		target = resolved
	}
	fi, err := os.Stat(target)
	switch {
	case err == nil && fi.IsDir():
		return fmt.Errorf("env file %s is a directory (Docker creates one when the bind-mounted file is missing): "+
			"remove it, create the file and give it to the container user (see deploy/README.md)", path)
	case err == nil:
		f, err := os.OpenFile(target, os.O_RDWR, 0)
		if err != nil {
			return fmt.Errorf("env file %s is not readable and writable by this user (uid %d): %w; "+
				"on the host run: sudo chown 65532:65532 .env && sudo chmod 600 .env (or the MAILROOM_UID/MAILROOM_GID "+
				"the container runs as, see deploy/README.md)", path, os.Getuid(), err)
		}
		return f.Close()
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("env file %s: %w", path, err)
	}
	dir := filepath.Dir(target)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil // WriteEnvFile creates the directory (and its parents)
	}
	probe, err := createTemp(dir, ".env.mailroom-check-*")
	if err != nil {
		return fmt.Errorf("env file %s does not exist and its directory is not writable by this user (uid %d): %w; "+
			"create the file on the host first and give it to the container user (see deploy/README.md)", path, os.Getuid(), err)
	}
	name := probe.Name()
	_ = probe.Close()
	return os.Remove(name)
}
