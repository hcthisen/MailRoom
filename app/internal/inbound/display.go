package inbound

import (
	"context"
	"encoding/base64"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// maxInlineDataURI is the largest inline image put into the displayed HTML
// as a data: URI.
const maxInlineDataURI = 2 << 20

// Display is a received HTML body prepared for the sandboxed viewer.
type Display struct {
	HTML string
	// BlockedImages counts remote images that were left out (they would
	// tell the sender when and where the message was read).
	BlockedImages int
}

// DisplayHTML prepares a received message's HTML for showing in a
// sandboxed iframe: scripts, forms, frames, <base> and meta refreshes are
// removed, links open in a new tab without a referrer, inline images
// (cid:) become data: URIs, and remote images are left out unless
// showImages. The iframe must still be sandboxed without allow-scripts:
// this is about privacy and usability, not the security boundary.
func (s *Service) DisplayHTML(ctx context.Context, m Message, showImages bool) Display {
	cids := map[string]string{}
	for _, a := range m.AttachmentsList {
		if a.ContentID == "" || !strings.HasPrefix(strings.ToLower(a.ContentType), "image/") || a.SizeBytes > maxInlineDataURI ||
			strings.EqualFold(a.ContentType, "image/svg+xml") {
			continue
		}
		_, data, err := s.AttachmentData(ctx, m.ID, a.Position)
		if err != nil {
			continue
		}
		cids[strings.ToLower(a.ContentID)] = "data:" + strings.ToLower(a.ContentType) + ";base64," + base64.StdEncoding.EncodeToString(data)
	}
	return prepareHTML(m.HTML, cids, showImages)
}

func prepareHTML(src string, cids map[string]string, showImages bool) Display {
	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		return Display{HTML: "<pre>" + html.EscapeString(htmlToText(src)) + "</pre>"}
	}
	var d Display
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		for c := n.FirstChild; c != nil; {
			next := c.NextSibling
			if c.Type == html.ElementNode {
				switch c.DataAtom {
				case atom.Script, atom.Base, atom.Iframe, atom.Frame, atom.Frameset, atom.Object, atom.Embed,
					atom.Applet, atom.Form, atom.Link:
					n.RemoveChild(c)
					c = next
					continue
				case atom.Meta:
					if strings.EqualFold(attr(c, "http-equiv"), "refresh") {
						n.RemoveChild(c)
						c = next
						continue
					}
				case atom.A, atom.Area:
					setAttr(c, "target", "_blank")
					setAttr(c, "rel", "noopener noreferrer")
				case atom.Img, atom.Input, atom.Source, atom.Video, atom.Audio:
					src := strings.TrimSpace(attr(c, "src"))
					low := strings.ToLower(src)
					switch {
					case strings.HasPrefix(low, "cid:"):
						if uri, ok := cids[strings.ToLower(strings.Trim(src[4:], "<>"))]; ok {
							setAttr(c, "src", uri)
						} else {
							delAttr(c, "src")
						}
					case strings.HasPrefix(low, "data:"):
					case src != "" && !showImages:
						delAttr(c, "src")
						delAttr(c, "srcset")
						d.BlockedImages++
					}
				}
				// No event handlers or javascript: URLs survive, even
				// though the sandbox wouldn't run them.
				kept := c.Attr[:0]
				for _, a := range c.Attr {
					k := strings.ToLower(a.Key)
					v := strings.ToLower(strings.TrimSpace(a.Val))
					if strings.HasPrefix(k, "on") || (k == "href" || k == "src" || k == "action") && strings.HasPrefix(v, "javascript:") {
						continue
					}
					if !showImages && k == "background" && (strings.HasPrefix(v, "http:") || strings.HasPrefix(v, "https:") || strings.HasPrefix(v, "//")) {
						d.BlockedImages++
						continue
					}
					kept = append(kept, a)
				}
				c.Attr = kept
			}
			walk(c)
			c = next
		}
	}
	walk(doc)
	var b strings.Builder
	if err := html.Render(&b, doc); err != nil {
		return Display{HTML: "<pre>" + html.EscapeString(htmlToText(src)) + "</pre>"}
	}
	d.HTML = b.String()
	return d
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

func setAttr(n *html.Node, key, val string) {
	for i, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			n.Attr[i].Val = val
			return
		}
	}
	n.Attr = append(n.Attr, html.Attribute{Key: key, Val: val})
}

func delAttr(n *html.Node, key string) {
	kept := n.Attr[:0]
	for _, a := range n.Attr {
		if !strings.EqualFold(a.Key, key) {
			kept = append(kept, a)
		}
	}
	n.Attr = kept
}
