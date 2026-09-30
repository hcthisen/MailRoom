package content

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// TextWidth is the soft wrap width of the plain-text version.
const TextWidth = 78

// HTMLToText converts an email's HTML into its plain-text alternative
// (E-5): headings and paragraphs as blocks separated by blank lines
// (h1/h2 underlined), list items as "- " or "1. ", links as
// "text (url)", images as their alt text, no tags, whitespace collapsed,
// and lines soft-wrapped at TextWidth characters (words longer than that,
// e.g. URLs, are never broken). Hidden preheaders, <head>, <style> and
// <script> are skipped.
func HTMLToText(src string) string {
	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		return ""
	}
	c := &textConv{}
	c.walk(doc)
	c.flush(blockPara)
	return strings.TrimSpace(c.out.String()) + "\n"
}

// Block kinds decide the separator before the next block.
const (
	blockPara = iota
	blockItem
)

type listCtx struct {
	ordered bool
	n       int
}

type textConv struct {
	out     strings.Builder
	cur     strings.Builder // inline text of the current block
	started bool            // something was written to out
	last    int             // kind of the last written block
	lists   []listCtx
	indents []string // continuation indents of the open list items
	prefix  string   // first line of the next flushed block: indent + marker ("- ")
	pre     int      // depth of <pre>
}

// inline appends text with whitespace collapsed.
func (c *textConv) inline(s string) {
	if c.pre > 0 {
		c.cur.WriteString(s)
		return
	}
	for _, r := range s {
		if unicode.IsSpace(r) || r == ' ' || r == '‌' || r == '͏' {
			if c.cur.Len() > 0 && !c.endsWithSpace() {
				c.cur.WriteByte(' ')
			}
			continue
		}
		c.cur.WriteRune(r)
	}
}

func (c *textConv) endsWithSpace() bool {
	s := c.cur.String()
	r, _ := utf8.DecodeLastRuneInString(s)
	return r == ' ' || r == '\n'
}

// lineBreak ends the current line inside a block (<br>).
func (c *textConv) lineBreak() {
	s := strings.TrimRight(c.cur.String(), " ")
	c.cur.Reset()
	c.cur.WriteString(s)
	c.cur.WriteByte('\n')
}

// indent is the indent of text inside the open list items.
func (c *textConv) indent() string {
	if len(c.indents) == 0 {
		return ""
	}
	return c.indents[len(c.indents)-1]
}

// flush writes the current block, if any, as kind.
func (c *textConv) flush(kind int) {
	text := c.cur.String()
	c.cur.Reset()
	if c.pre == 0 {
		lines := strings.Split(text, "\n")
		for i := range lines {
			lines[i] = strings.TrimSpace(lines[i])
		}
		text = strings.Trim(strings.Join(lines, "\n"), "\n")
	} else {
		text = strings.Trim(text, "\n")
	}
	if strings.TrimSpace(text) == "" {
		return
	}
	if c.started {
		if kind == blockItem && c.last == blockItem {
			c.out.WriteString("\n")
		} else {
			c.out.WriteString("\n\n")
		}
	}
	indent := c.indent()
	first := indent
	if c.prefix != "" {
		first, c.prefix = c.prefix, ""
	}
	if c.pre > 0 {
		c.out.WriteString(text)
	} else {
		for i, line := range strings.Split(text, "\n") {
			p := indent
			if i == 0 {
				p = first
			} else {
				c.out.WriteByte('\n')
			}
			c.out.WriteString(wrapText(line, TextWidth, p, indent))
		}
	}
	c.started = true
	c.last = kind
}

// hidden reports whether n is an element the text version leaves out.
func hidden(n *html.Node) bool {
	switch n.DataAtom {
	case atom.Head, atom.Style, atom.Script, atom.Title, atom.Template, atom.Noscript:
		return true
	}
	for _, a := range n.Attr {
		switch {
		case a.Key == "data-mr-preheader", a.Key == "hidden":
			return true
		case a.Key == "style":
			st := strings.ReplaceAll(strings.ToLower(a.Val), " ", "")
			if strings.Contains(st, "display:none") {
				return true
			}
		}
	}
	return false
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Namespace == "" && a.Key == key {
			return a.Val
		}
	}
	return ""
}

func (c *textConv) children(n *html.Node) {
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		c.walk(ch)
	}
}

func (c *textConv) walk(n *html.Node) {
	switch n.Type {
	case html.TextNode:
		c.inline(n.Data)
		return
	case html.DocumentNode:
		c.children(n)
		return
	case html.ElementNode:
	default:
		return
	}
	if hidden(n) {
		return
	}
	switch n.DataAtom {
	case atom.Br:
		c.lineBreak()
	case atom.Hr:
		c.flush(blockPara)
		c.cur.WriteString(strings.Repeat("-", 20))
		c.flush(blockPara)
	case atom.A:
		c.anchor(n)
	case atom.Img:
		if alt := strings.TrimSpace(attr(n, "alt")); alt != "" {
			c.inline(alt)
		}
	case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
		c.flush(blockPara)
		c.children(n)
		c.heading(n.DataAtom)
	case atom.Ul, atom.Ol:
		if len(c.lists) > 0 {
			c.flush(blockItem) // text of the enclosing item
		} else {
			c.flush(blockPara)
		}
		c.lists = append(c.lists, listCtx{ordered: n.DataAtom == atom.Ol})
		c.children(n)
		c.flush(blockItem)
		c.lists = c.lists[:len(c.lists)-1]
	case atom.Li:
		c.flush(blockItem)
		marker := "- "
		if l := len(c.lists); l > 0 && c.lists[l-1].ordered {
			c.lists[l-1].n++
			marker = strconv.Itoa(c.lists[l-1].n) + ". "
		}
		parent := c.indent()
		c.prefix = parent + marker
		c.indents = append(c.indents, parent+strings.Repeat(" ", len(marker)))
		c.children(n)
		c.flush(blockItem)
		c.indents = c.indents[:len(c.indents)-1]
		c.prefix = ""
	case atom.Pre:
		c.flush(blockPara)
		c.pre++
		c.children(n)
		c.flush(blockPara)
		c.pre--
	case atom.Tr:
		c.row(n)
	case atom.P, atom.Div, atom.Blockquote, atom.Table, atom.Tbody, atom.Thead, atom.Tfoot,
		atom.Td, atom.Th, atom.Section, atom.Article, atom.Header, atom.Footer, atom.Center,
		atom.Dl, atom.Dt, atom.Dd, atom.Figure, atom.Figcaption, atom.Address, atom.Main, atom.Nav, atom.Aside:
		c.flush(blockPara)
		c.children(n)
		c.flush(blockPara)
	default:
		c.children(n)
	}
}

// heading writes the current text as a heading; h1 and h2 are underlined.
func (c *textConv) heading(a atom.Atom) {
	text := strings.TrimSpace(strings.ReplaceAll(c.cur.String(), "\n", " "))
	c.cur.Reset()
	if text == "" {
		return
	}
	c.cur.WriteString(text)
	if a == atom.H1 || a == atom.H2 {
		mark := "="
		if a == atom.H2 {
			mark = "-"
		}
		n := utf8.RuneCountInString(text)
		if n > TextWidth {
			n = TextWidth
		}
		c.cur.WriteString("\n" + strings.Repeat(mark, n))
	}
	c.flush(blockPara)
}

// anchor writes a link as "text (url)". The URL is left out when it is the
// text itself, an #anchor or a javascript: URL.
func (c *textConv) anchor(n *html.Node) {
	start := c.cur.Len()
	c.children(n)
	href := strings.TrimSpace(attr(n, "href"))
	text := strings.TrimSpace(c.cur.String()[start:])
	switch {
	case href == "", strings.HasPrefix(href, "#"), strings.HasPrefix(strings.ToLower(href), "javascript:"):
		return
	}
	shown := href
	if rest, ok := strings.CutPrefix(strings.ToLower(href), "mailto:"); ok {
		shown = href[len(href)-len(rest):]
	}
	if text == "" {
		c.inline(shown)
		return
	}
	if text == shown || text == href {
		return
	}
	c.inline(" (" + shown + ")")
}

// row writes a data table row as "cell | cell"; rows of layout tables
// (role="presentation") are walked as blocks.
func (c *textConv) row(n *html.Node) {
	if presentational(n) {
		c.flush(blockPara)
		c.children(n)
		c.flush(blockPara)
		return
	}
	c.flush(blockItem)
	var cells []string
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		if ch.Type != html.ElementNode || (ch.DataAtom != atom.Td && ch.DataAtom != atom.Th) || hidden(ch) {
			continue
		}
		sub := &textConv{}
		sub.children(ch)
		sub.flush(blockPara)
		cells = append(cells, strings.Join(strings.Fields(sub.out.String()), " "))
	}
	c.cur.WriteString(strings.Join(cells, " | "))
	c.flush(blockItem)
}

// presentational reports whether the table containing n is a layout table.
func presentational(n *html.Node) bool {
	for p := n.Parent; p != nil; p = p.Parent {
		if p.Type == html.ElementNode && p.DataAtom == atom.Table {
			return strings.EqualFold(attr(p, "role"), "presentation") || !hasHeaderRow(p)
		}
	}
	return true
}

// hasHeaderRow reports whether table has a <th> cell, which marks a data
// table (Markdown tables always have one).
func hasHeaderRow(table *html.Node) bool {
	var found bool
	var visit func(*html.Node)
	visit = func(n *html.Node) {
		for ch := n.FirstChild; ch != nil && !found; ch = ch.NextSibling {
			if ch.Type != html.ElementNode {
				continue
			}
			if ch.DataAtom == atom.Th {
				found = true
				return
			}
			if ch.DataAtom != atom.Table {
				visit(ch)
			}
		}
	}
	visit(table)
	return found
}

// wrapText wraps line at width, prefixing the first line with first and
// the others with indent. Words are never split.
func wrapText(line string, width int, first, indent string) string {
	words := strings.Fields(line)
	if len(words) == 0 {
		return strings.TrimRight(first, " ")
	}
	var b strings.Builder
	b.WriteString(first)
	col := utf8.RuneCountInString(first)
	lineStart := true
	for _, w := range words {
		wl := utf8.RuneCountInString(w)
		if !lineStart && col+1+wl > width {
			b.WriteString("\n" + indent)
			col = utf8.RuneCountInString(indent)
			lineStart = true
		}
		if !lineStart {
			b.WriteByte(' ')
			col++
		}
		b.WriteString(w)
		col += wl
		lineStart = false
	}
	return b.String()
}
