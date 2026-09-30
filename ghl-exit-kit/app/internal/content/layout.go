package content

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"mailroom/internal/api"
	"mailroom/internal/audit"
	"mailroom/internal/core"
	"mailroom/internal/db"
)

// LayoutKey is the settings-table key of the base layout document.
const LayoutKey = "layout"

// DefaultBrandColor is used when the layout sets no brand colour.
const DefaultBrandColor = "#2563eb"

// Layout limits.
const (
	MaxLogoURLLength    = 2048
	MaxCustomHTMLLength = 256 << 10 // bytes
)

// Layout field names usable in custom_html.
const (
	layoutContent     = "content"
	layoutUnsubscribe = "unsubscribe_url"
	layoutPreheader   = "preheader"
	layoutFooter      = "footer"
	layoutLogoURL     = "logo_url"
	layoutBrandColor  = "brand_color"
)

// LayoutInput is the editable part of the base layout (E-1, E-4).
type LayoutInput struct {
	LogoURL    string `json:"logo_url" required:"false" maxLength:"2048" doc:"Absolute http(s) URL of the logo shown at the top (e.g. an uploaded media URL); empty for none" example:"https://newsletter.example.com/media/3f9a0c1b2d4e5f60718293a4b5c6d7e8.png"`
	BrandColor string `json:"brand_color" required:"false" doc:"Brand colour as #rrggbb, used for the top border and links; empty for the default" example:"#2563eb"`
	CustomHTML string `json:"custom_html" required:"false" doc:"Optional complete HTML layout replacing the built-in one. It must contain {{content}}, a link to {{unsubscribe_url}} and {{footer}} (the company name and postal address from the settings, required in marketing email), and may use {{preheader}}, {{logo_url}} and {{brand_color}}" example:""`
}

// Layout is the stored base layout.
type Layout struct {
	LayoutInput
	Version int64 `json:"version" readOnly:"true" doc:"Hash-derived version of the layout; sent as the ETag" example:"4172631129"`
}

var rxColor = regexp.MustCompile(`^#[0-9a-f]{6}$`)

// ValidateLayout normalises in (trimmed, lowercase colour, default colour)
// and checks it. custom_html must contain {{content}} exactly once, use
// {{unsubscribe_url}} as the href of at least one link (E-4), and show
// {{footer}} outside HTML comments: the footer carries the company name,
// postal address and registration number every marketing email must have
// (E-1, CAN-SPAM, EU sender identification).
func ValidateLayout(in LayoutInput) (LayoutInput, error) {
	in.LogoURL = strings.TrimSpace(in.LogoURL)
	in.BrandColor = strings.ToLower(strings.TrimSpace(in.BrandColor))
	if in.BrandColor == "" {
		in.BrandColor = DefaultBrandColor
	}
	if !rxColor.MatchString(in.BrandColor) {
		return in, invalid("brand_color", "must be a colour like #1a2b3c")
	}
	if in.LogoURL != "" {
		if len(in.LogoURL) > MaxLogoURLLength {
			return in, invalid("logo_url", "is longer than %d characters", MaxLogoURLLength)
		}
		if !isHTTPURL(in.LogoURL) {
			return in, invalid("logo_url", "must be an absolute http or https URL")
		}
	}
	if strings.TrimSpace(in.CustomHTML) == "" {
		in.CustomHTML = ""
		return in, nil
	}
	if len(in.CustomHTML) > MaxCustomHTMLLength {
		return in, invalid("custom_html", "is larger than %d bytes", MaxCustomHTMLLength)
	}
	if !utf8.ValidString(in.CustomHTML) {
		return in, invalid("custom_html", "is not valid UTF-8")
	}
	t, err := parseMerge("custom_html", in.CustomHTML, layoutVocabulary)
	if err != nil {
		return in, err
	}
	if n := t.count(layoutContent); n != 1 {
		return in, invalid("custom_html", "must contain {{content}} exactly once (found %d)", n)
	}
	if t.count(layoutUnsubscribe) == 0 {
		return in, invalid("custom_html", "must contain {{unsubscribe_url}}: every marketing email needs an unsubscribe link")
	}
	pl, err := placement(in)
	if err != nil {
		return in, err
	}
	if !pl.unsubLink {
		return in, invalid("custom_html", `must use {{unsubscribe_url}} as a link, e.g. <a href="{{unsubscribe_url}}">Unsubscribe</a> (not only in a comment or as text)`)
	}
	if !pl.unsubVisible {
		return in, invalid("custom_html", "must show the {{unsubscribe_url}} link visibly: it is inside a hidden element (hidden, display:none, visibility:hidden, font-size:0, opacity:0 or similar)")
	}
	if t.count(layoutFooter) == 0 {
		return in, invalid("custom_html", "must contain {{footer}}: every marketing email shows the company name and postal address from the settings (company_footer)")
	}
	if !pl.footerText {
		return in, invalid("custom_html", "must show {{footer}} as text, not only inside an HTML comment")
	}
	if !pl.footerVisible {
		return in, invalid("custom_html", "must show {{footer}} visibly: it is inside a hidden element (hidden, display:none, visibility:hidden, font-size:0, opacity:0 or similar)")
	}
	return in, nil
}

// footerSentinel stands in for the company footer when a custom layout is
// checked.
const footerSentinel = "mailroom-footer-check"

// unsubscribeSentinel stands in for the unsubscribe URL when a custom
// layout is checked.
const unsubscribeSentinel = "https://unsubscribe.mailroom.invalid/check"

// layoutPlacement is where a rendered custom layout puts the unsubscribe
// link and the footer.
type layoutPlacement struct {
	unsubLink, unsubVisible   bool // an <a> whose href is the unsubscribe URL (outside comments); one not hidden
	footerText, footerVisible bool // the footer in a text node (outside comments); one not hidden
}

// placement renders the custom layout with sentinels and reports whether
// it links the unsubscribe URL (E-4) and shows the footer (E-1), and
// whether it does so outside hidden elements (concealed): a pasted design
// with hidden blocks must not hide either.
func placement(l LayoutInput) (layoutPlacement, error) {
	var pl layoutPlacement
	out, err := wrapCustom(l, wrapParams{UnsubscribeURL: unsubscribeSentinel, Footer: footerSentinel})
	if err != nil {
		return pl, err
	}
	doc, err := html.Parse(strings.NewReader(out))
	if err != nil {
		return pl, invalid("custom_html", "can't be parsed as HTML: %v", err)
	}
	var walk func(n *html.Node, hiddenAbove bool)
	walk = func(n *html.Node, hiddenAbove bool) {
		h := hiddenAbove || (n.Type == html.ElementNode && concealed(n))
		switch {
		case n.Type == html.ElementNode && n.DataAtom == atom.A:
			for _, a := range n.Attr {
				if a.Namespace == "" && strings.EqualFold(a.Key, "href") && strings.TrimSpace(a.Val) == unsubscribeSentinel {
					pl.unsubLink = true
					pl.unsubVisible = pl.unsubVisible || !h
				}
			}
		case n.Type == html.TextNode && strings.Contains(n.Data, footerSentinel):
			pl.footerText = true
			pl.footerVisible = pl.footerVisible || !h
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, h)
		}
	}
	walk(doc, false)
	return pl, nil
}

// concealed reports whether an element hides its content from the reader:
// the elements and attributes the text part leaves out (hidden), and
// inline styles that hide it (visibility:hidden, font-size:0, opacity:0,
// mso-hide:all, a zero height with overflow:hidden).
func concealed(n *html.Node) bool {
	if hidden(n) {
		return true
	}
	style := attr(n, "style")
	if style == "" {
		return false
	}
	decl := map[string]string{}
	for _, d := range strings.Split(strings.ToLower(style), ";") {
		k, v, ok := strings.Cut(d, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "!important"))
		decl[strings.TrimSpace(k)] = v
	}
	zero := func(v string) bool {
		v = strings.TrimSpace(v)
		if v == "" {
			return false
		}
		num := strings.TrimRight(v, "abcdefghijklmnopqrstuvwxyz%")
		f, err := strconv.ParseFloat(num, 64)
		return err == nil && f == 0
	}
	switch {
	case decl["display"] == "none", decl["visibility"] == "hidden", decl["visibility"] == "collapse",
		decl["mso-hide"] == "all", zero(decl["font-size"]), zero(decl["opacity"]):
		return true
	case decl["overflow"] == "hidden" && (zero(decl["height"]) || zero(decl["max-height"]) || zero(decl["width"]) || zero(decl["max-width"])):
		return true
	}
	return false
}

// layoutVocabulary allows the layout fields in custom_html.
func layoutVocabulary(name string) string {
	switch name {
	case layoutContent, layoutUnsubscribe, layoutPreheader, layoutFooter, layoutLogoURL, layoutBrandColor:
		return ""
	}
	return fmt.Sprintf("unknown layout field %q; use content, unsubscribe_url, preheader, footer, logo_url or brand_color", name)
}

// isHTTPURL reports whether s is an absolute http(s) URL with a host.
func isHTTPURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Scheme, "http") || strings.EqualFold(u.Scheme, "https")
}

// layoutVersion derives a positive version from the layout's content, so
// the ETag changes exactly when the layout does.
func layoutVersion(in LayoutInput) int64 {
	h := sha256.New()
	for _, s := range []string{in.LogoURL, in.BrandColor, in.CustomHTML} {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(s)))
		h.Write(n[:])
		h.Write([]byte(s))
	}
	v := int64(binary.BigEndian.Uint64(h.Sum(nil)[:8]) >> 11) // 53 bits: exact in JSON numbers
	if v == 0 {
		v = 1
	}
	return v
}

// GetLayout returns the base layout (the defaults when none was saved).
// A nil q reads from the read pool.
func (s *Service) GetLayout(ctx context.Context, q db.Querier) (Layout, error) {
	in := LayoutInput{BrandColor: DefaultBrandColor}
	raw, err := s.settings.GetRaw(ctx, q, LayoutKey)
	switch {
	case errors.Is(err, core.ErrNotFound):
	case err != nil:
		return Layout{}, err
	default:
		if err := json.Unmarshal(raw, &in); err != nil {
			return Layout{}, fmt.Errorf("content: decode layout: %w", err)
		}
		if in.BrandColor == "" {
			in.BrandColor = DefaultBrandColor
		}
	}
	return Layout{LayoutInput: in, Version: layoutVersion(in)}, nil
}

// PutLayout validates and stores the layout in tx with an audit record.
// ifVersion (from If-Match) must match the current version unless it is 0.
func (s *Service) PutLayout(ctx context.Context, tx *sql.Tx, in LayoutInput, ifVersion int64) (Layout, error) {
	cur, err := s.GetLayout(ctx, tx)
	if err != nil {
		return Layout{}, err
	}
	if err := api.CheckVersion(ifVersion, cur.Version); err != nil {
		return Layout{}, err
	}
	in, err = ValidateLayout(in)
	if err != nil {
		return Layout{}, err
	}
	next := Layout{LayoutInput: in, Version: layoutVersion(in)}
	if next.Version == cur.Version {
		return cur, nil
	}
	if err := s.settings.PutRaw(ctx, tx, LayoutKey, in); err != nil {
		return Layout{}, err
	}
	changes := audit.Diff(nil, "logo_url", cur.LogoURL, in.LogoURL)
	audit.Diff(changes, "brand_color", cur.BrandColor, in.BrandColor)
	if cur.CustomHTML != in.CustomHTML {
		changes["custom_html_bytes"] = audit.Change{From: len(cur.CustomHTML), To: len(in.CustomHTML)}
	}
	if err := audit.Write(ctx, tx, audit.Entry{
		Operation: "layout.update", TargetType: "layout", TargetID: LayoutKey, Changes: changes, At: s.clock.Now(),
	}); err != nil {
		return Layout{}, err
	}
	return next, nil
}
