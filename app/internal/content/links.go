package content

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"mailroom/internal/db"
	"mailroom/internal/tokens"
)

// NoTrackAttr on an <a> keeps the link out of click tracking. It is
// removed from every rendered email, tracked or not. Links whose href
// contains a merge field get it automatically: their URL differs per
// recipient (see placeholderSet.substituteHTML).
const NoTrackAttr = "data-mr-notrack"

// MaxTrackedURLLength bounds the URLs stored in links; longer links are
// sent untracked.
const MaxTrackedURLLength = 4096

// linkCacheSize bounds the in-memory url -> links.id cache. Links rows
// are never deleted, so cached IDs stay valid.
const linkCacheSize = 10_000

// linkCache remembers link IDs, so a broadcast inserts or looks up each
// URL once instead of once per recipient.
type linkCache struct {
	mu  sync.Mutex
	max int
	ids map[string]int64
}

func newLinkCache(max int) *linkCache {
	return &linkCache{max: max, ids: make(map[string]int64)}
}

func (c *linkCache) get(url string) (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id, ok := c.ids[url]
	return id, ok
}

func (c *linkCache) put(url string, id int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.ids) >= c.max {
		c.ids = make(map[string]int64, c.max)
	}
	c.ids[url] = id
}

// linkID returns the links.id of url, inserting the row when it is new.
// The insert is a single autocommit statement on the write pool (no
// transaction, so no commit hooks run). It must not be called while the
// caller holds a write transaction.
func (s *Service) linkID(ctx context.Context, url string) (int64, error) {
	if id, ok := s.links.get(url); ok {
		return id, nil
	}
	var id int64
	err := s.db.Read.QueryRowContext(ctx, `SELECT id FROM links WHERE url = ?`, url).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		err = s.db.Write.QueryRowContext(ctx,
			`INSERT INTO links (url, created_at) VALUES (?, ?)
			 ON CONFLICT (url) DO UPDATE SET url = excluded.url
			 RETURNING id`, url, db.FormatTime(s.clock.Now())).Scan(&id)
	}
	if err != nil {
		return 0, fmt.Errorf("content: link id: %w", err)
	}
	s.links.put(url, id)
	return id, nil
}

// trackable reports whether href is an http(s) link that click tracking
// may rewrite: not the unsubscribe link, not already a tracking link, not
// mailto:, tel: or #anchors, not too long.
func (s *Service) trackable(href, unsubscribeURL string) bool {
	h := strings.TrimSpace(href)
	if h == "" || len(h) > MaxTrackedURLLength || !isHTTPURL(h) {
		return false
	}
	if h == unsubscribeURL || strings.HasPrefix(h, s.baseURL+"/u/") || strings.HasPrefix(h, s.baseURL+"/c/") {
		return false
	}
	return true
}

// linkRewrite says what rewriteLinks does.
type linkRewrite struct {
	Track          bool // rewrite trackable links; false: only drop NoTrackAttr
	MessageID      int64
	ContactID      int64
	UnsubscribeURL string
}

// rewriteLinks drops NoTrackAttr from every <a> of src and, with Track,
// rewrites every trackable <a href> to BaseURL/c/<click token>. Links
// carrying NoTrackAttr are never rewritten. Everything else is copied
// byte for byte.
func (s *Service) rewriteLinks(ctx context.Context, src string, p linkRewrite) (string, error) {
	if !p.Track && !strings.Contains(strings.ToLower(src), NoTrackAttr) {
		return src, nil
	}
	var b strings.Builder
	b.Grow(len(src) + len(src)/8)
	z := html.NewTokenizer(strings.NewReader(src))
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			if err := z.Err(); !errors.Is(err, io.EOF) {
				return "", fmt.Errorf("content: parse html: %w", err)
			}
			return b.String(), nil
		}
		raw := z.Raw()
		if tt != html.StartTagToken && tt != html.SelfClosingTagToken {
			b.Write(raw)
			continue
		}
		// Token (not TagName) must be the first accessor: TagName consumes
		// the name and attributes.
		tok := z.Token()
		if tok.DataAtom != atom.A || len(tok.Attr) == 0 {
			b.Write(raw)
			continue
		}
		out, changed, err := s.rewriteAnchor(ctx, &tok, p)
		if err != nil {
			return "", err
		}
		if !changed {
			b.Write(raw)
			continue
		}
		b.WriteString(out)
	}
}

// rewriteAnchor rewrites one <a> start tag. changed is false when the tag
// is copied unchanged.
func (s *Service) rewriteAnchor(ctx context.Context, tok *html.Token, p linkRewrite) (string, bool, error) {
	hrefIdx, noTrack := -1, false
	attrs := tok.Attr[:0]
	for _, a := range tok.Attr {
		if a.Namespace == "" && strings.EqualFold(a.Key, NoTrackAttr) {
			noTrack = true
			continue
		}
		if a.Namespace == "" && strings.EqualFold(a.Key, "href") && hrefIdx < 0 {
			hrefIdx = len(attrs)
		}
		attrs = append(attrs, a)
	}
	tok.Attr = attrs
	if noTrack {
		return tok.String(), true, nil
	}
	if !p.Track || hrefIdx < 0 || !s.trackable(tok.Attr[hrefIdx].Val, p.UnsubscribeURL) {
		return "", false, nil
	}
	target := strings.TrimSpace(tok.Attr[hrefIdx].Val)
	linkID, err := s.linkID(ctx, target)
	if err != nil {
		return "", false, err
	}
	t, err := s.signer.Sign(tokens.PurposeClick, tokens.Payload{MessageID: p.MessageID, LinkID: linkID, ContactID: p.ContactID})
	if err != nil {
		return "", false, fmt.Errorf("content: sign click token: %w", err)
	}
	tok.Attr[hrefIdx].Val = s.baseURL + "/c/" + t
	return tok.String(), true, nil
}

// addOpenPixel inserts the 1x1 open-tracking image before </body>, or
// appends it when there is none.
func (s *Service) addOpenPixel(src string, messageID, contactID int64) (string, error) {
	t, err := s.signer.Sign(tokens.PurposeOpen, tokens.Payload{ContactID: contactID, MessageID: messageID})
	if err != nil {
		return "", fmt.Errorf("content: sign open token: %w", err)
	}
	pixel := `<img src="` + html.EscapeString(s.baseURL+"/o/"+t) + `" width="1" height="1" alt="" style="display:block;width:1px;height:1px;border:0;margin:0;padding:0;">`
	if i := strings.LastIndex(strings.ToLower(src), "</body>"); i >= 0 {
		return src[:i] + pixel + "\n" + src[i:], nil
	}
	return src + pixel, nil
}
