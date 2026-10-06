package ui

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
)

// Flash kinds.
const (
	FlashSuccess = "success"
	FlashError   = "error"
	FlashWarning = "warning"
	FlashInfo    = "info"
)

// Flash is a one-time message shown at the top of the next page.
type Flash struct {
	Kind string `json:"k"`
	Text string `json:"t"`
}

// flashCookie carries flashes across a redirect. It is signed, so only
// the server can put text into it.
const flashCookie = "mailroom_flash"

// Flash limits: a cookie stays small.
const (
	maxFlashes    = 4
	maxFlashBytes = 400
)

// Flash queues a message for the page this request ends on: the next page
// after Redirect, the page itself after Page, or an out-of-band swap into
// the flash area after Partial.
func (u *UI) Flash(r *http.Request, kind, text string) {
	st := stateOf(r)
	if len(st.flashes) >= maxFlashes {
		return
	}
	if len(text) > maxFlashBytes {
		text = text[:maxFlashBytes] + "…"
	}
	switch kind {
	case FlashSuccess, FlashError, FlashWarning, FlashInfo:
	default:
		kind = FlashInfo
	}
	st.flashes = append(st.flashes, Flash{Kind: kind, Text: text})
}

// Redirect ends a request that changed something (post/redirect/get): it
// keeps the queued flashes for the next page and redirects with 303, or,
// for htmx requests, tells htmx to load the page (HX-Redirect).
func (u *UI) Redirect(w http.ResponseWriter, r *http.Request, target string) {
	u.saveFlashes(w, r)
	if isHTMX(r) {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// flashKey derives the cookie signing key from the install secret.
func (u *UI) flashKey() []byte {
	m := hmac.New(sha256.New, u.d.Config.Secret)
	m.Write([]byte("mailroom ui flash v1"))
	return m.Sum(nil)
}

func (u *UI) sign(payload string) string {
	m := hmac.New(sha256.New, u.flashKey())
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// saveFlashes writes the request's queued flashes to the flash cookie.
func (u *UI) saveFlashes(w http.ResponseWriter, r *http.Request) {
	st := stateOf(r)
	if len(st.flashes) == 0 {
		return
	}
	b, err := json.Marshal(st.flashes)
	if err != nil {
		return
	}
	payload := base64.RawURLEncoding.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Value: payload + "." + u.sign(payload), Path: Prefix + "/",
		MaxAge: 300, HttpOnly: true, Secure: u.secure, SameSite: http.SameSiteLaxMode})
	st.flashes = nil
}

// takeFlashes returns the flashes to show on the page being rendered: the
// ones from the flash cookie (which is then cleared) and the ones queued
// during this request.
func (u *UI) takeFlashes(w http.ResponseWriter, r *http.Request) []Flash {
	st := stateOf(r)
	var out []Flash
	if !st.read {
		st.read = true
		if c, err := r.Cookie(flashCookie); err == nil && c.Value != "" {
			http.SetCookie(w, &http.Cookie{Name: flashCookie, Value: "", Path: Prefix + "/", MaxAge: -1,
				HttpOnly: true, Secure: u.secure, SameSite: http.SameSiteLaxMode})
			out = append(out, u.parseFlashCookie(c.Value)...)
		}
	}
	out = append(out, st.flashes...)
	st.flashes = nil
	return out
}

func (u *UI) parseFlashCookie(v string) []Flash {
	payload, sig, ok := strings.Cut(v, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(u.sign(payload))) {
		return nil
	}
	b, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return nil
	}
	var fs []Flash
	if json.Unmarshal(b, &fs) != nil || len(fs) > maxFlashes {
		return nil
	}
	return fs
}
