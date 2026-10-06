package events

import (
	"bytes"
	"html/template"
	"net/http"
	"strings"

	"mailroom/internal/settings"
)

// page is the data of a public HTML page.
type page struct {
	Title   string
	Heading string
	Message string
	Company string
	// Form shows the unsubscribe buttons (the confirmation page).
	Form bool
	// SequenceOption adds "Only stop these emails" to the form.
	SequenceOption bool
}

// pageTemplate is the one self-contained layout of the public pages: no
// external assets, no scripts, no tracking.
var pageTemplate = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex, nofollow">
<title>{{.Title}}</title>
<style>
:root { color-scheme: light dark; --bg: #f4f5f7; --card: #fff; --text: #1f2328; --muted: #57606a; --accent: #1f2328; --accent-text: #fff; --border: #d0d7de; }
@media (prefers-color-scheme: dark) { :root { --bg: #0d1117; --card: #161b22; --text: #e6edf3; --muted: #9da7b3; --accent: #e6edf3; --accent-text: #0d1117; --border: #30363d; } }
* { box-sizing: border-box; }
body { margin: 0; min-height: 100vh; display: flex; align-items: center; justify-content: center; padding: 16px; background: var(--bg); color: var(--text); font: 16px/1.5 system-ui, -apple-system, "Segoe UI", Roboto, sans-serif; }
main { width: 100%; max-width: 440px; background: var(--card); border: 1px solid var(--border); border-radius: 12px; padding: 32px 28px; }
.company { margin: 0 0 20px; font-size: 14px; font-weight: 600; letter-spacing: .02em; color: var(--muted); }
h1 { margin: 0 0 12px; font-size: 22px; line-height: 1.3; }
p { margin: 0 0 20px; color: var(--muted); }
form { display: flex; flex-direction: column; gap: 10px; margin: 0; }
button { width: 100%; padding: 12px 16px; font: inherit; font-weight: 600; border-radius: 8px; cursor: pointer; border: 1px solid var(--accent); background: var(--accent); color: var(--accent-text); }
button.secondary { background: transparent; color: var(--text); border-color: var(--border); font-weight: 500; }
</style>
</head>
<body>
<main>
{{if .Company}}<p class="company">{{.Company}}</p>{{end}}
<h1>{{.Heading}}</h1>
<p>{{.Message}}</p>
{{if .Form}}<form method="post" action="">
<button type="submit" name="scope" value="all">Unsubscribe</button>
{{if .SequenceOption}}<button class="secondary" type="submit" name="scope" value="sequence">Only stop these emails</button>{{end}}
</form>{{end}}
</main>
</body>
</html>
`))

// pageCSP locks public pages down to their own inline styles.
const pageCSP = "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

// renderPage writes p with status.
func (s *Service) renderPage(w http.ResponseWriter, status int, p page) {
	var buf bytes.Buffer
	if err := pageTemplate.Execute(&buf, p); err != nil {
		s.log.Error("events: render page", "err", err)
		plain(w, http.StatusInternalServerError, "internal error")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Robots-Tag", "noindex, nofollow")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", pageCSP)
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// companyName is the sender name shown on public pages: the From name,
// else the first line of the company footer, else the sending domain.
func companyName(st settings.Settings, domain string) string {
	if n := strings.TrimSpace(st.Sender.FromName); n != "" {
		return n
	}
	if first, _, _ := strings.Cut(strings.TrimSpace(st.CompanyFooter), "\n"); strings.TrimSpace(first) != "" {
		first = strings.TrimSpace(first)
		if name, _, ok := strings.Cut(first, ","); ok && strings.TrimSpace(name) != "" {
			return strings.TrimSpace(name)
		}
		return first
	}
	if st.Domain != "" {
		return st.Domain
	}
	return domain
}

// The public pages.
func confirmPage(company string, sequenceOption bool) page {
	msg := "You will stop receiving emails from " + company + " at this address."
	if company == "" {
		msg = "You will stop receiving emails at this address."
	}
	return page{
		Title: "Unsubscribe", Heading: "Unsubscribe?", Company: company,
		Message: msg, Form: true, SequenceOption: sequenceOption,
	}
}

func unsubscribedPage(company string) page {
	msg := "You won’t receive any more emails from " + company + " at this address."
	if company == "" {
		msg = "You won’t receive any more emails at this address."
	}
	return page{Title: "Unsubscribed", Heading: "You’re unsubscribed", Company: company, Message: msg}
}

func sequenceStoppedPage(company string) page {
	return page{
		Title: "Emails stopped", Heading: "Done", Company: company,
		Message: "You won’t receive any more emails from this series. Other emails still reach you; use the unsubscribe link in any email to stop them all.",
	}
}

func notFoundPage() page {
	return page{
		Title: "Link not valid", Heading: "This link isn’t valid",
		Message: "The link may be incomplete. Try opening it again from the email, or use the unsubscribe link in a newer email.",
	}
}

func errorPage(company string) page {
	return page{
		Title: "Something went wrong", Heading: "Something went wrong", Company: company,
		Message: "We couldn’t process your request right now. Please try again in a minute.",
	}
}
