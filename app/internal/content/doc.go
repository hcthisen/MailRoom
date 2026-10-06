// Package content owns email content (PRD §6.6): the emails library, the
// base layout, merge fields, Markdown, the renderer used at send time and
// for previews (it implements core.Renderer), click and open tracking
// links (K-1, K-2), the plain-text alternative (E-5), the branded system
// templates (opt-in, resubscribe, alert), test sends (B-7, S-10) and the
// media library served from /media/ (E-6).
//
// Merge fields use a small engine of their own rather than text/template
// or html/template on user content: {{first_name}}, {{last_name}},
// {{email}}, {{attr.<key>}} and {{unsubscribe_url}}, each with an optional
// fallback, e.g. {{first_name | "there"}}. Unknown fields are rejected when
// an email is saved. Values are HTML-escaped in HTML and inserted as they
// are in the subject and the text version. In URL attributes (href, src,
// ...) a value that starts the URL must have the http, https, mailto or
// tel scheme (or none), else the fallback or "#" is used, and a value
// later in the URL is URL-encoded. Links whose href contains a merge
// field are not click-tracked: their URL differs per recipient and would
// otherwise put every recipient's personal data into links. A missing or
// empty value uses the fallback, or "".
//
// Test sends (message data {"test": true}, written by SendTest) never get
// a working unsubscribe link or tracking, whatever the caller passes as
// Marketing.
//
// Render reads the email, the layout and the settings from the read pool
// and, when click tracking is on, may insert new rows into links with a
// short write of its own on the write pool. It must therefore never be
// called while the caller holds a write transaction (the write pool has a
// single connection, so that would wait forever).
package content
