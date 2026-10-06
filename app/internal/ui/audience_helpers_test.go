package ui_test

import (
	"bytes"
	"context"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"mailroom/internal/app"
	"mailroom/internal/jobs"
)

// Helpers of the audience tests (contacts, tags, suppressions, imports,
// segments). Names start with "aud" so they don't clash with other
// groups' test helpers.

// audMultipart posts a multipart form with one file (the CSV upload).
// csrf adds the session's token as a form field.
func (c *client) audMultipart(path string, fields map[string]string, filename, content string, csrf bool) resp {
	c.hs.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if csrf {
		_ = mw.WriteField("csrf_token", c.csrf)
	}
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	if filename != "" {
		fw, err := mw.CreateFormFile("file", filename)
		if err != nil {
			c.hs.t.Fatal(err)
		}
		_, _ = fw.Write([]byte(content))
	}
	if err := mw.Close(); err != nil {
		c.hs.t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return c.do(req)
}

// audLocationID returns the numeric ID at the end of a redirect Location
// that starts with prefix ("/admin/contacts/").
func audLocationID(t *testing.T, r resp, prefix string) int64 {
	t.Helper()
	loc := r.header.Get("Location")
	if loc == "" {
		loc = r.header.Get("HX-Redirect")
	}
	if !strings.HasPrefix(loc, prefix) {
		t.Fatalf("redirect to %q, want %s<id>\n%.2000s", loc, prefix, r.body)
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(loc, prefix), 10, 64)
	if err != nil {
		t.Fatalf("redirect to %q: no ID", loc)
	}
	return id
}

// audStartJobs runs the jobs runner until the test ends.
func (hs *harness) audStartJobs() {
	ctx, cancel := context.WithCancel(hs.ctx)
	done := make(chan error, 1)
	runner := hs.app.Jobs
	go func() { done <- runner.Run(ctx) }()
	hs.t.Cleanup(func() {
		cancel()
		<-done
	})
}

// audWaitJob waits for a job and fails unless it succeeded.
func (hs *harness) audWaitJob(id int64) jobs.Job {
	hs.t.Helper()
	ctx, cancel := context.WithTimeout(hs.ctx, 30*time.Second)
	defer cancel()
	j, err := hs.app.Jobs.Wait(ctx, id)
	if err != nil {
		hs.t.Fatalf("wait for job %d: %v", id, err)
	}
	if j.Status != jobs.StatusSucceeded {
		hs.t.Fatalf("job %d = %s: %s", id, j.Status, j.Error)
	}
	return j
}

// audPages checks the logged-out redirect of each page.
func audLoggedOut(t *testing.T, hs *harness, paths ...string) {
	t.Helper()
	anon := hs.anon()
	for _, p := range paths {
		anon.get(p).wantLogin(t)
	}
}

// audHarness is newHarness with error logs shown in the test output, so
// a 500 page says why.
func audHarness(t *testing.T) *harness {
	t.Helper()
	return newHarness(t, func(o *app.Options) {
		o.Logger = slog.New(slog.NewTextHandler(audLogWriter{t}, &slog.HandlerOptions{Level: slog.LevelError}))
	})
}

type audLogWriter struct{ t *testing.T }

func (w audLogWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}
