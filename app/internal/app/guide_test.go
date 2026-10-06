package app_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"mailroom/docs"
	"mailroom/internal/api"
)

// TestAPIGuide checks the agent guide docs/api.md (PRD G-3) against the
// operations the app registers, so the recipes can't drift from the API:
//
//   - the guide is embedded and served verbatim at /api/v1/docs;
//   - every "/api/v1/..." path in it is a registered route (or the spec or
//     guide itself), and every "METHOD /path" mention (in the text or the
//     endpoint table) exists with that method;
//   - every curl command in a bash block calls an existing operation with
//     query parameters, If-Match, Idempotency-Key and content types the
//     operation declares, and its literal JSON body passes the operation's
//     request schema (shell variables are replaced by 1 first).
func TestAPIGuide(t *testing.T) {
	guide := docs.APIGuide()
	if len(guide) == 0 {
		t.Fatal("docs/api.md is not embedded (docs.APIGuide() is empty)")
	}
	hs := newHarness(t, nil)
	r := hs.raw(http.MethodGet, api.Prefix+"/docs", "", "").want(t, http.StatusOK)
	if !bytes.Equal(r.body, guide) {
		t.Fatal("/api/v1/docs does not serve docs/api.md")
	}
	if ct := r.header.Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Fatalf("/api/v1/docs Content-Type = %q", ct)
	}

	c := newGuideChecker(hs.app.API)
	md := string(guide)
	paths := c.checkPathMentions(t, md)
	methods := c.checkMethodMentions(t, md)
	curls := c.checkCurls(t, md)
	t.Logf("checked %d paths, %d method mentions, %d curl commands, %d JSON bodies", paths, methods, curls, c.bodies)
	// Guard against a parser that silently finds nothing.
	if paths < 50 || methods < 60 || curls < 40 || c.bodies < 15 {
		t.Fatalf("found only %d paths, %d method mentions, %d curl commands and %d JSON bodies in docs/api.md",
			paths, methods, curls, c.bodies)
	}
}

// guideChecker matches guide references against the registered routes and
// the generated spec.
type guideChecker struct {
	routes []api.RouteInfo
	oapi   *huma.OpenAPI
	bodies int // JSON bodies validated
}

func newGuideChecker(a *api.API) *guideChecker {
	routes := a.Routes()
	// Served by the API outside Huma, without credentials.
	for _, p := range []string{"/docs", "/openapi.json", "/openapi.yaml"} {
		routes = append(routes, api.RouteInfo{Method: http.MethodGet, Path: api.Prefix + p})
	}
	return &guideChecker{routes: routes, oapi: a.OpenAPI()}
}

var (
	// rxAPIPath: any path under /api/v1 (placeholders {x} and $X included).
	rxAPIPath = regexp.MustCompile(`/api/v1(/[A-Za-z0-9_.{}$/-]*)?`)
	// rxMethodPath: "GET /x", "GET/POST /x" or "PATCH /api/v1/x?y=1".
	rxMethodPath = regexp.MustCompile("\\b((?:GET|POST|PUT|PATCH|DELETE)(?:/(?:GET|POST|PUT|PATCH|DELETE))*) (/[A-Za-z0-9_.{}$/-]*)(\\?[^\\s`\"')]*)?")
	// rxShellVar is a shell variable in a recipe: $X or ${X}.
	rxShellVar = regexp.MustCompile(`\$\{?[A-Za-z_][A-Za-z0-9_]*\}?`)
	// rxHeredoc is a here-document marker: <<EOF, <<'EOF', <<-EOF.
	rxHeredoc = regexp.MustCompile(`<<-?\s*['"]?([A-Za-z_][A-Za-z0-9_]*)['"]?`)
)

// nonAPIPaths are paths outside the API that the guide may mention with a
// method (public endpoints and the UI).
var nonAPIPaths = []string{"/healthz", "/f/", "/u/", "/c/", "/o/", "/hooks/", "/admin"}

// checkPathMentions checks that every /api/v1 path is a route with any
// method, and returns how many it checked.
func (c *guideChecker) checkPathMentions(t *testing.T, md string) int {
	t.Helper()
	n := 0
	for _, m := range rxAPIPath.FindAllStringSubmatch(md, -1) {
		p := strings.TrimRight(m[1], ".,:;")
		if p == "" || p == "/" {
			continue // the base URL
		}
		n++
		if len(c.match("", api.Prefix+p)) == 0 {
			t.Errorf("docs/api.md mentions %s, which is not an API route", api.Prefix+p)
		}
	}
	return n
}

// checkMethodMentions checks every "METHOD /path" mention; paths without
// the /api/v1 prefix (as in the endpoint table) are relative to it.
func (c *guideChecker) checkMethodMentions(t *testing.T, md string) int {
	t.Helper()
	n := 0
	for _, m := range rxMethodPath.FindAllStringSubmatch(md, -1) {
		p := strings.TrimRight(m[2], ".,:;")
		if !strings.HasPrefix(p, api.Prefix) {
			if nonAPI(p) {
				continue
			}
			p = api.Prefix + p
		}
		for _, method := range strings.Split(m[1], "/") {
			n++
			op, ok := c.operation(method, p)
			if !ok {
				t.Errorf("docs/api.md mentions %s %s, which is not an API operation", method, p)
				continue
			}
			c.checkQuery(t, method+" "+p, op, strings.TrimPrefix(m[3], "?"))
		}
	}
	return n
}

func nonAPI(p string) bool {
	for _, pre := range nonAPIPaths {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}

// match returns the routes whose template matches path (a guide path with
// literal segments and placeholders), with method "" matching any method.
// The routes with the most literal segment matches come first.
func (c *guideChecker) match(method, path string) []api.RouteInfo {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	var best []api.RouteInfo
	bestScore := -1
	for _, r := range c.routes {
		if method != "" && r.Method != method {
			continue
		}
		tmpl := strings.Split(strings.Trim(r.Path, "/"), "/")
		if len(tmpl) != len(segs) {
			continue
		}
		score, ok := 0, true
		for i, s := range segs {
			switch {
			case strings.HasPrefix(tmpl[i], "{"):
				// A parameter matches a value or a placeholder.
			case s == tmpl[i]:
				score++
			default:
				ok = false
			}
			if !ok {
				break
			}
		}
		switch {
		case !ok:
		case score > bestScore:
			best, bestScore = []api.RouteInfo{r}, score
		case score == bestScore:
			best = append(best, r)
		}
	}
	return best
}

// operation returns the spec operation for method and a guide path; ok is
// false when no route matches. op is nil for the spec and guide documents.
func (c *guideChecker) operation(method, path string) (op *huma.Operation, ok bool) {
	rs := c.match(method, path)
	if len(rs) == 0 {
		return nil, false
	}
	item := c.oapi.Paths[strings.TrimPrefix(rs[0].Path, api.Prefix)]
	if item == nil {
		return nil, true
	}
	switch method {
	case http.MethodGet:
		op = item.Get
	case http.MethodPost:
		op = item.Post
	case http.MethodPut:
		op = item.Put
	case http.MethodPatch:
		op = item.Patch
	case http.MethodDelete:
		op = item.Delete
	}
	return op, true
}

func hasParam(op *huma.Operation, in, name string) bool {
	if op == nil {
		return false
	}
	for _, p := range op.Parameters {
		if p.In == in && strings.EqualFold(p.Name, name) {
			return true
		}
	}
	return false
}

// checkQuery checks that every key of a query string ("a=1&b=2") is a
// query parameter of op.
func (c *guideChecker) checkQuery(t *testing.T, what string, op *huma.Operation, query string) {
	t.Helper()
	if query == "" {
		return
	}
	for _, kv := range strings.Split(query, "&") {
		k, _, _ := strings.Cut(kv, "=")
		if k != "" && !hasParam(op, "query", k) {
			t.Errorf("docs/api.md: %s: %q is not a query parameter of this operation", what, k)
		}
	}
}

// checkCurls checks every curl command in the guide's bash blocks and
// returns how many API calls it checked.
func (c *guideChecker) checkCurls(t *testing.T, md string) int {
	t.Helper()
	n := 0
	for _, cmd := range shellCommands(md) {
		rest := cmd.line
		for {
			i := curlIndex(rest)
			if i < 0 {
				break
			}
			args, end := shellWords(rest[i+len("curl"):])
			rest = rest[i+len("curl")+end:]
			if c.checkCurl(t, args, cmd.heredoc) {
				n++
			}
		}
	}
	return n
}

// curlIndex returns the index of the next "curl" command word in s, or -1.
func curlIndex(s string) int {
	for off := 0; ; {
		i := strings.Index(s[off:], "curl ")
		if i < 0 {
			return -1
		}
		i += off
		if i == 0 || strings.ContainsRune(" \t($|;&", rune(s[i-1])) {
			return i
		}
		off = i + 1
	}
}

// checkCurl checks one curl invocation; it returns false when the command
// doesn't call the API (e.g. a form's public endpoint).
func (c *guideChecker) checkCurl(t *testing.T, args []string, heredoc string) bool {
	t.Helper()
	var (
		method, url, contentType string
		get                      bool
		data, forms, headers     []string
	)
	next := func(i *int) string {
		*i++
		if *i >= len(args) {
			t.Errorf("docs/api.md: curl %s: %s needs an argument", strings.Join(args, " "), args[*i-1])
			return ""
		}
		return args[*i]
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "-X", "--request":
			method = next(&i)
		case "-H", "--header":
			h := next(&i)
			headers = append(headers, h)
			if name, v, ok := strings.Cut(h, ":"); ok && strings.EqualFold(strings.TrimSpace(name), "Content-Type") {
				contentType = strings.TrimSpace(v)
			}
		case "-d", "--data", "--data-raw", "--data-binary", "--data-urlencode":
			data = append(data, next(&i))
		case "-F", "--form":
			forms = append(forms, next(&i))
		case "-o", "--output", "-D", "--dump-header", "-w", "--write-out":
			next(&i)
		case "-G", "--get":
			get = true
		case "-s", "-S", "-sS", "-i", "--include":
		default:
			switch {
			case strings.HasPrefix(a, "-"):
				t.Errorf("docs/api.md: curl option %s is not known to this test; add it to checkCurl", a)
			case url == "":
				url = a
			default:
				t.Errorf("docs/api.md: curl with two URLs: %s and %s", url, a)
			}
		}
	}
	i := strings.Index(url, api.Prefix)
	if i < 0 {
		return false
	}
	path, query, _ := strings.Cut(url[i:], "?")
	switch {
	case method != "":
	case get:
		method = http.MethodGet
	case len(data) > 0 || len(forms) > 0:
		method = http.MethodPost
	default:
		method = http.MethodGet
	}
	what := "curl " + method + " " + path
	op, ok := c.operation(method, path)
	if !ok {
		t.Errorf("docs/api.md: %s is not an API operation", what)
		return true
	}
	c.checkQuery(t, what, op, query)
	if get {
		for _, d := range data {
			c.checkQuery(t, what, op, d)
		}
		data = nil
	}
	for _, h := range headers {
		name, _, _ := strings.Cut(h, ":")
		switch strings.TrimSpace(name) {
		case "If-Match":
			if !hasParam(op, "header", "If-Match") {
				t.Errorf("docs/api.md: %s sends If-Match, which this operation doesn't take", what)
			}
		case api.IdempotencyHeader:
			if method != http.MethodPost {
				t.Errorf("docs/api.md: %s sends an Idempotency-Key, which only applies to POST", what)
			}
		case "Authorization", "Content-Type":
		default:
			t.Errorf("docs/api.md: %s sends header %q, which this test doesn't know", what, name)
		}
	}
	if len(forms) > 0 {
		c.checkContentType(t, what, op, "multipart/form-data")
	}
	for _, d := range data {
		switch {
		case d == "@-":
			c.checkBody(t, what, op, contentType, heredoc)
		case strings.HasPrefix(d, "@"):
			c.checkContentType(t, what, op, contentType) // a file
		default:
			c.checkBody(t, what, op, contentType, d)
		}
	}
	return true
}

// checkContentType checks that op accepts a request body of type ct.
func (c *guideChecker) checkContentType(t *testing.T, what string, op *huma.Operation, ct string) {
	t.Helper()
	if op == nil || op.RequestBody == nil || op.RequestBody.Content[ct] == nil {
		t.Errorf("docs/api.md: %s sends a %q body, which this operation doesn't accept", what, ct)
	}
}

// checkBody validates a JSON request body against op's request schema.
// Bodies built with command substitution ($(...)) can't be checked.
func (c *guideChecker) checkBody(t *testing.T, what string, op *huma.Operation, ct, body string) {
	t.Helper()
	if ct != "application/json" {
		t.Errorf("docs/api.md: %s sends a body with Content-Type %q; JSON bodies need application/json", what, ct)
		return
	}
	c.checkContentType(t, what, op, ct)
	if strings.Contains(body, "$(") || op == nil || op.RequestBody == nil || op.RequestBody.Content[ct] == nil {
		return
	}
	var v any
	if err := json.Unmarshal([]byte(rxShellVar.ReplaceAllString(body, "1")), &v); err != nil {
		t.Errorf("docs/api.md: %s: the body is not valid JSON: %v\n%s", what, err, body)
		return
	}
	res := &huma.ValidateResult{}
	huma.Validate(c.oapi.Components.Schemas, op.RequestBody.Content[ct].Schema,
		huma.NewPathBuffer([]byte("body"), 0), huma.ModeWriteToServer, v, res)
	for _, err := range res.Errors {
		t.Errorf("docs/api.md: %s: %v\n%s", what, err, body)
	}
	c.bodies++
}

// shellCommand is a logical line of a bash block, with the here-document
// that follows it, if any.
type shellCommand struct {
	line    string
	heredoc string
}

// shellCommands returns the logical command lines of the guide's bash and
// sh code blocks: backslash continuations and quoted strings spanning lines
// are joined, comment lines dropped, and here-documents attached.
func shellCommands(md string) []shellCommand {
	var (
		out     []shellCommand
		inBlock bool
		shell   bool
		cur     strings.Builder
		pending *shellCommand
		endTag  string
		doc     []string
	)
	for _, line := range strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			if !inBlock {
				lang := strings.TrimPrefix(trimmed, "```")
				inBlock, shell = true, lang == "bash" || lang == "sh"
			} else {
				inBlock = false
			}
			continue
		}
		if !inBlock || !shell {
			continue
		}
		if pending != nil { // inside a here-document
			if trimmed == endTag {
				pending.heredoc = strings.Join(doc, "\n")
				out = append(out, *pending)
				pending, doc = nil, nil
			} else {
				doc = append(doc, line)
			}
			continue
		}
		if cur.Len() == 0 && strings.HasPrefix(trimmed, "#") {
			continue
		}
		if cur.Len() > 0 {
			cur.WriteString("\n")
		}
		cur.WriteString(line)
		s := cur.String()
		if openQuote(s) {
			continue
		}
		if strings.HasSuffix(s, "\\") {
			cur.Reset()
			cur.WriteString(strings.TrimSuffix(s, "\\") + " ")
			continue
		}
		cur.Reset()
		cmd := shellCommand{line: s}
		if i := heredocIndex(s); i >= 0 {
			if m := rxHeredoc.FindStringSubmatch(s[i:]); m != nil {
				pending, endTag = &cmd, m[1]
				continue
			}
		}
		out = append(out, cmd)
	}
	return out
}

// openQuote reports whether s ends inside a single- or double-quoted string.
func openQuote(s string) bool {
	var q byte
	for i := 0; i < len(s); i++ {
		switch ch := s[i]; {
		case q == 0 && (ch == '\'' || ch == '"'):
			q = ch
		case q == '"' && ch == '\\':
			i++
		case q == 0 && ch == '\\':
			i++
		case ch == q:
			q = 0
		}
	}
	return q != 0
}

// heredocIndex returns the index of the first "<<" in s outside quoted
// strings, or -1, so a "<<" inside a string isn't taken for a
// here-document.
func heredocIndex(s string) int {
	var q byte
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case q == 0 && (ch == '\'' || ch == '"'):
			q = ch
		case q != 0 && ch == q:
			q = 0
		case q == 0 && strings.HasPrefix(s[i:], "<<"):
			return i
		}
	}
	return -1
}

// shellWords splits s into words the way a shell would, well enough for
// the guide's curl commands: quotes are removed, $(...) substitutions are
// kept verbatim, and splitting stops at an unquoted |, ;, &, ), <, > or #.
// It returns the words and the index where it stopped.
func shellWords(s string) ([]string, int) {
	var (
		words []string
		cur   strings.Builder
		in    bool // a word has started
		i     int
	)
	flush := func() {
		if in {
			words = append(words, cur.String())
		}
		cur.Reset()
		in = false
	}
	for i = 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch == ' ' || ch == '\t' || ch == '\n':
			flush()
		case ch == '\'':
			in = true
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				cur.WriteString(s[i+1:])
				i = len(s)
				break
			}
			cur.WriteString(s[i+1 : i+1+j])
			i += j + 1
		case ch == '"':
			in = true
			for i++; i < len(s) && s[i] != '"'; i++ {
				switch {
				case s[i] == '\\' && i+1 < len(s) && strings.IndexByte("\"\\$`", s[i+1]) >= 0:
					i++
					cur.WriteByte(s[i])
				case strings.HasPrefix(s[i:], "$("):
					n := substitutionLen(s[i:])
					cur.WriteString(s[i : i+n])
					i += n - 1
				default:
					cur.WriteByte(s[i])
				}
			}
		case ch == '\\' && i+1 < len(s):
			in = true
			i++
			cur.WriteByte(s[i])
		case strings.HasPrefix(s[i:], "$("):
			in = true
			n := substitutionLen(s[i:])
			cur.WriteString(s[i : i+n])
			i += n - 1
		case strings.IndexByte("|;&)<>", ch) >= 0, ch == '#' && !in:
			flush()
			return words, i
		default:
			in = true
			cur.WriteByte(ch)
		}
	}
	flush()
	return words, i
}

// substitutionLen returns the length of the $(...) at the start of s,
// skipping quoted strings and nested parentheses.
func substitutionLen(s string) int {
	depth := 0
	var q byte
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case q != 0:
			if ch == '\\' && q == '"' {
				i++
			} else if ch == q {
				q = 0
			}
		case ch == '\'' || ch == '"':
			q = ch
		case ch == '(':
			depth++
		case ch == ')':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return len(s)
}

// TestGuideShellParsing pins the small shell parser used by TestAPIGuide.
func TestGuideShellParsing(t *testing.T) {
	words, _ := shellWords(` -sS -X POST "$MAILROOM/api/v1/x" -H "If-Match: $(etag "/api/v1/x/$ID")" -d '{"a": "b c"}' | jq .id`)
	want := []string{"-sS", "-X", "POST", "$MAILROOM/api/v1/x", "-H", `If-Match: $(etag "/api/v1/x/$ID")`, "-d", `{"a": "b c"}`}
	if fmt.Sprint(words) != fmt.Sprint(want) {
		t.Fatalf("shellWords = %q, want %q", words, want)
	}
	md := "```bash\n# comment\nJOB=$(curl -sS -X POST \"$MAILROOM/api/v1/y\" \\\n  -d '{\n  \"k\": 1\n}' | jq -r .id)\n" +
		"curl -sS -X POST \"$MAILROOM/api/v1/z\" --data @- <<'EOF'\n{\"yaml\": \"a\"}\nEOF\n```\n"
	cmds := shellCommands(md)
	if len(cmds) != 2 || !strings.Contains(cmds[0].line, "\"k\": 1") || cmds[1].heredoc != `{"yaml": "a"}` {
		t.Fatalf("shellCommands = %#v", cmds)
	}
	if i := curlIndex(cmds[0].line); i != len("JOB=$(") {
		t.Fatalf("curlIndex = %d", i)
	}
}
