package forms

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"slices"
	"strings"

	"mailroom/internal/api"
	"mailroom/internal/core"
)

// Mount registers the public form endpoints on mux (server.PublicMux):
//
//	GET     /f/{public_id}        hosted form page (F-3); ?submitted=1 is the thank-you page
//	POST    /f/{public_id}        submission, form-encoded or JSON (F-2)
//	OPTIONS /f/{public_id}        CORS preflight for the form's allowed origins
//	GET     /f/confirm/{token}    confirmation page with a button (link prefetchers don't confirm)
//	POST    /f/confirm/{token}    confirms a double opt-in or resubscribe
func (s *Service) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /f/{public_id}", s.handleFormPage)
	mux.HandleFunc("POST /f/{public_id}", s.handleSubmit)
	mux.HandleFunc("OPTIONS /f/{public_id}", s.handlePreflight)
	mux.HandleFunc("GET /f/confirm/{token}", s.handleConfirmPage)
	mux.HandleFunc("POST /f/confirm/{token}", s.handleConfirm)
}

// Messages of the public pages.
const (
	msgNotFound    = "This sign-up form doesn't exist (any more). Please check the link."
	msgRateLimited = "You've tried this too often in a short time. Please wait a minute and try again."
	msgInternal    = "Something went wrong on our side. Please try again in a few minutes."
)

// successMessage is the only answer an accepted submission gets. It
// depends on the form alone, never on the address, so the response can't
// tell a new, existing, unsubscribed or suppressed address apart (F-7).
func successMessage(f Form) string {
	if f.DoubleOptin {
		return "Almost done: please check your inbox and click the link in the email we just sent you to confirm your subscription."
	}
	return "Thank you for signing up! If we need you to confirm your address, you'll get an email from us shortly."
}

// publicCtx marks the request as coming from the public.
func publicCtx(r *http.Request) context.Context {
	return core.WithActor(r.Context(), core.Actor{Type: core.ActorPublic})
}

func (s *Service) clientKey(r *http.Request) (ip, key string) {
	ip = api.ClientIP(r, s.trusted)
	return ip, api.IPKey(ip)
}

// allowPage applies the page limiter; it answers 429 itself when refused.
func (s *Service) allowPage(w http.ResponseWriter, r *http.Request, jsonReply bool) bool {
	_, key := s.clientKey(r)
	d := s.pageLimiter.Allow(key)
	if d.Allowed {
		return true
	}
	d.WriteHeaders(w.Header())
	if jsonReply {
		api.ErrRateLimited().Write(w)
	} else {
		s.writeMessage(w, r, http.StatusTooManyRequests, "Too many requests", msgRateLimited)
	}
	return false
}

// lookupForm returns the form of the request's public_id; ok=false means
// the response (404 or 500) was written.
func (s *Service) lookupForm(w http.ResponseWriter, r *http.Request, jsonReply bool) (Form, bool) {
	pid := r.PathValue("public_id")
	var f Form
	err := fmt.Errorf("form: %w", core.ErrNotFound)
	if validPublicID(pid) {
		f, err = s.GetByPublicID(r.Context(), nil, pid)
	}
	if err == nil {
		return f, true
	}
	switch {
	case errors.Is(err, core.ErrNotFound) && jsonReply:
		api.NewError(http.StatusNotFound, api.CodeNotFound, "form not found", "").Write(w)
	case errors.Is(err, core.ErrNotFound):
		s.writeMessage(w, r, http.StatusNotFound, "Form not found", msgNotFound)
	default:
		s.internalError(w, r, jsonReply, err)
	}
	return Form{}, false
}

func (s *Service) internalError(w http.ResponseWriter, r *http.Request, jsonReply bool, err error) {
	s.log.ErrorContext(r.Context(), "forms: public request failed", "path", "/f/…", "err", err)
	if jsonReply {
		api.NewError(http.StatusInternalServerError, api.CodeInternal, "internal server error", "").Write(w)
		return
	}
	s.writeMessage(w, r, http.StatusInternalServerError, "Something went wrong", msgInternal)
}

// handleFormPage serves the hosted form page, or its thank-you page.
func (s *Service) handleFormPage(w http.ResponseWriter, r *http.Request) {
	if !s.allowPage(w, r, false) {
		return
	}
	f, ok := s.lookupForm(w, r, false)
	if !ok {
		return
	}
	if r.URL.Query().Get("submitted") == "1" {
		s.writePage(w, r, http.StatusOK, pageData{Title: f.Name, Heading: "Thank you!", Message: successMessage(f)})
		return
	}
	s.writePage(w, r, http.StatusOK, pageData{Title: f.Name, Heading: f.Name, Form: newFormView(f, nil, nil)})
}

// handlePreflight answers CORS preflights: 204 with the CORS headers for
// an allowed origin, 403 otherwise.
func (s *Service) handlePreflight(w http.ResponseWriter, r *http.Request) {
	if !s.allowPage(w, r, true) {
		return
	}
	f, ok := s.lookupForm(w, r, true)
	if !ok {
		return
	}
	w.Header().Add("Vary", "Origin")
	origin := r.Header.Get("Origin")
	if !s.corsAllowed(f, origin) {
		api.NewError(http.StatusForbidden, api.CodeForbidden, "origin not allowed for this form; add it to the form's allowed_origins", "").Write(w)
		return
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", origin)
	h.Set("Access-Control-Allow-Methods", "POST")
	h.Set("Access-Control-Allow-Headers", "Content-Type, Accept, X-Requested-With")
	h.Set("Access-Control-Max-Age", "600")
	w.WriteHeader(http.StatusNoContent)
}

// corsAllowed reports whether origin may read responses of form f: one of
// its allowed_origins, or Mailroom's own origin (the hosted page).
func (s *Service) corsAllowed(f Form, origin string) bool {
	if origin == "" {
		return false
	}
	o := strings.ToLower(strings.TrimSpace(origin))
	return slices.Contains(f.AllowedOrigins, o) || o == originOf(s.baseURL)
}

// handleSubmit processes a sign-up (F-2, F-5, F-7).
func (s *Service) handleSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := publicCtx(r)
	r = r.WithContext(ctx)
	jsonReply := wantsJSON(r)
	f, ok := s.lookupForm(w, r, jsonReply)
	if !ok {
		return
	}
	w.Header().Add("Vary", "Origin")
	origin := r.Header.Get("Origin")
	if s.corsAllowed(f, origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
	} else if origin != "" && r.Header.Get("Sec-Fetch-Mode") == "cors" {
		// A script on a foreign site: refuse instead of acting blind.
		api.NewError(http.StatusForbidden, api.CodeForbidden, "origin not allowed for this form; add it to the form's allowed_origins", "").Write(w)
		return
	}
	values, err := parseValues(w, r)
	if err != nil {
		s.badRequest(w, r, f, jsonReply, err)
		return
	}
	ip, key := s.clientKey(r)
	d := s.limiter.Allow(key)
	if strings.TrimSpace(values[HoneypotField]) != "" {
		// Bots get the normal answer and learn nothing (F-5).
		if d.Allowed {
			s.recordStandalone(ctx, f.ID, OutcomeSpam)
		}
		s.writeSuccess(w, r, f, jsonReply)
		return
	}
	if !d.Allowed {
		if s.logLimiter.Allow(key).Allowed {
			s.recordStandalone(ctx, f.ID, OutcomeRateLimited)
		}
		d.WriteHeaders(w.Header())
		if jsonReply {
			api.ErrRateLimited().Write(w)
		} else {
			s.writeMessage(w, r, http.StatusTooManyRequests, "Too many requests", msgRateLimited)
		}
		return
	}
	d.WriteHeaders(w.Header())
	clean, errs := validateSubmission(f, SubmissionInput{Values: values, IP: ip, UserAgent: r.UserAgent()})
	if len(errs) > 0 {
		if jsonReply {
			api.NewError(http.StatusUnprocessableEntity, api.CodeValidation, errs[0].Message, errs[0].Field).Write(w)
			return
		}
		s.writePage(w, r, http.StatusUnprocessableEntity, pageData{Title: f.Name, Heading: f.Name, Form: newFormView(f, values, errs)})
		return
	}
	if _, err := s.submit(ctx, f.ID, clean); err != nil {
		if errors.Is(err, core.ErrNotFound) { // the form was deleted meanwhile
			if jsonReply {
				api.NewError(http.StatusNotFound, api.CodeNotFound, "form not found", "").Write(w)
			} else {
				s.writeMessage(w, r, http.StatusNotFound, "Form not found", msgNotFound)
			}
			return
		}
		s.internalError(w, r, jsonReply, err)
		return
	}
	s.writeSuccess(w, r, f, jsonReply)
}

// recordStandalone records a spam or rate-limited submission in its own
// transaction (no other write is held here).
func (s *Service) recordStandalone(ctx context.Context, formID int64, outcome string) {
	if err := s.recordSubmission(ctx, s.db.Write, formID, 0, outcome); err != nil {
		s.log.WarnContext(ctx, "forms: record submission", "outcome", outcome, "err", err)
	}
}

// submitResponse is the JSON answer of an accepted submission.
type submitResponse struct {
	OK          bool   `json:"ok"`
	RedirectURL string `json:"redirect_url"`
	Message     string `json:"message"`
}

// writeSuccess sends the one answer every accepted submission gets: JSON
// for scripts, otherwise 303 to the redirect URL or the thank-you page.
func (s *Service) writeSuccess(w http.ResponseWriter, r *http.Request, f Form, jsonReply bool) {
	w.Header().Set("Cache-Control", "no-store")
	if jsonReply {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(submitResponse{OK: true, RedirectURL: f.RedirectURL, Message: successMessage(f)})
		return
	}
	target := f.RedirectURL
	if target == "" {
		target = f.HostedURL + "?submitted=1"
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// badRequest answers an unreadable body.
func (s *Service) badRequest(w http.ResponseWriter, r *http.Request, f Form, jsonReply bool, err error) {
	status, msg := http.StatusBadRequest, "The form data could not be read."
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		status, msg = http.StatusRequestEntityTooLarge, fmt.Sprintf("The form data is larger than %d bytes.", MaxBodyBytes)
	}
	if jsonReply {
		api.NewError(status, api.CodeValidation, msg, "body").Write(w)
		return
	}
	s.writePage(w, r, status, pageData{Title: f.Name, Heading: f.Name, Message: msg, Form: newFormView(f, nil, nil)})
}

// wantsJSON reports whether the client expects a JSON answer: a JSON body,
// an XMLHttpRequest, or an Accept header preferring JSON over HTML.
func wantsJSON(r *http.Request) bool {
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt == "application/json" {
		return true
	}
	if strings.EqualFold(r.Header.Get("X-Requested-With"), "XMLHttpRequest") {
		return true
	}
	accept := r.Header.Get("Accept")
	return strings.Contains(accept, "application/json") && !strings.Contains(accept, "text/html")
}

// parseValues reads the submitted values from a form-encoded, multipart
// or JSON body (one value per name; the first wins). JSON strings,
// numbers and booleans are accepted; other JSON values are ignored.
func parseValues(w http.ResponseWriter, r *http.Request) (map[string]string, error) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	values := map[string]string{}
	switch mt {
	case "application/json":
		var raw map[string]any
		dec := json.NewDecoder(r.Body)
		dec.UseNumber()
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		for k, v := range raw {
			switch x := v.(type) {
			case string:
				values[k] = x
			case json.Number, bool:
				values[k] = fmt.Sprint(x)
			}
		}
		return values, nil
	case "multipart/form-data":
		if err := r.ParseMultipartForm(MaxBodyBytes); err != nil {
			return nil, err
		}
	default:
		if err := r.ParseForm(); err != nil {
			return nil, err
		}
	}
	for k, vs := range r.PostForm {
		if len(vs) > 0 {
			values[k] = vs[0]
		}
	}
	return values, nil
}
