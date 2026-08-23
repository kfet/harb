package ui

import (
	"html/template"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"github.com/kfet/harb/internal/linkrewrite"
)

// sanitizeHTML cleans untrusted feed HTML for safe rendering in the web
// UI. Feed content is attacker-controlled (anyone can publish a feed a
// user subscribes to), so rendering it verbatim is a stored-XSS vector.
//
// Strategy — parse into a node tree with golang.org/x/net/html, then
// rebuild it through a strict allow-list and re-serialize. Re-parsing +
// re-serialization is what neutralises mutation-XSS (misnested / exotic
// markup that a naive regex stripper would mangle into something the
// browser re-interprets as script). The rules:
//
//   - Only allow-listed elements survive. Known-dangerous elements
//     (script, style, iframe, object, embed, svg, …) are dropped
//     together with their subtree. Any other unrecognised element is
//     "unwrapped": the tag is discarded but its safe children are kept.
//   - Only allow-listed attributes survive on each element. Every
//     `on*` event handler, `style`, and presentational/scripting hook
//     is stripped.
//   - URL-bearing attributes (href, src, …) are scheme-checked: only
//     http, https, mailto and scheme-relative / relative references
//     are kept. `javascript:`, `data:`, `vbscript:` and friends — even
//     obfuscated with embedded whitespace/control chars or mixed case
//     — are dropped.
//   - Comments are dropped (they can smuggle conditional-comment script
//     in legacy engines and add no display value).
//
// We use golang.org/x/net/html rather than a third-party sanitizer
// (e.g. bluemonday): x/net is already a transitive dependency via
// gofeed, so this adds no new module while keeping the project's
// stdlib-mostly constraint intact.
//
// rules is the optional view-layer link-host rewrite map (see
// linkrewrite.Host): surviving <a href> values — and only those — have
// their host remapped after the safety check.
func sanitizeHTML(s string, rules map[string]string) string {
	if s == "" {
		return ""
	}
	// Parse as a fragment in a <body> context so bare inline/block
	// markup parses the way a browser would inside the document body.
	// html.ParseFragment with a valid context node does not error on
	// real input (the tokenizer is total); on the off chance it returns
	// nothing we simply emit an empty string — fail closed.
	ctx := &html.Node{Type: html.ElementNode, Data: "body", DataAtom: atom.Body}
	nodes, _ := html.ParseFragment(strings.NewReader(s), ctx)
	var b strings.Builder
	for _, n := range nodes {
		for _, c := range cleanNode(n, rules) {
			_ = html.Render(&b, c)
		}
	}
	return b.String()
}

// cleanNode returns the sanitized replacement for n: zero nodes (the
// node and its subtree are dropped), exactly one node (an allow-listed
// element or text node), or several nodes (an unwrapped unknown element
// replaced by its cleaned children).
func cleanNode(n *html.Node, rules map[string]string) []*html.Node {
	switch n.Type {
	case html.TextNode:
		return []*html.Node{{Type: html.TextNode, Data: n.Data}}
	case html.ElementNode:
		// fall through below
	default:
		// Comments, doctypes, etc. — drop entirely.
		return nil
	}

	tag := strings.ToLower(n.Data)
	if droppedSubtree[tag] {
		return nil
	}

	kids := cleanChildren(n, rules)

	allowed, ok := allowedAttrs[tag]
	if !ok {
		// Unknown but not dangerous: unwrap — keep the safe children,
		// discard the tag itself.
		return kids
	}

	el := &html.Node{Type: html.ElementNode, Data: tag, DataAtom: n.DataAtom}
	for _, a := range n.Attr {
		name := strings.ToLower(a.Key)
		// Namespaced attributes (xlink:href, xml:*) are never needed by
		// our allow-listed elements and are a known SVG/XML XSS surface.
		if a.Namespace != "" || strings.ContainsAny(name, ":") {
			continue
		}
		if !allowed[name] {
			continue
		}
		if urlAttrs[name] {
			if !safeURL(a.Val) {
				continue
			}
		}
		el.Attr = append(el.Attr, html.Attribute{Key: name, Val: a.Val})
	}

	if tag == "a" {
		// Host rewrite piggybacks on the existing <a> pass — href only,
		// and only after safeURL has judged the ORIGINAL value above, so
		// a dropped href is never resurrected. src/poster/srcset/cite are
		// deliberately untouched (image hosts, multi-URL syntax).
		for i, a := range el.Attr {
			if a.Key == "href" {
				el.Attr[i].Val = linkrewrite.Host(a.Val, rules)
			}
		}
		el.Attr = openLinkAttrs(el.Attr)
	}

	for _, c := range kids {
		el.AppendChild(c)
	}
	return []*html.Node{el}
}

// cleanChildren returns the flattened, cleaned children of n as a fresh
// slice of detached nodes (no parent links), ready to be re-attached.
func cleanChildren(n *html.Node, rules map[string]string) []*html.Node {
	var out []*html.Node
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		out = append(out, cleanNode(c, rules)...)
	}
	return out
}

// openLinkAttrs reproduces openLinksInNewTab's behaviour at the node
// level: every <a> opens in a new tab with rel="noopener noreferrer",
// unless the author already specified a target.
func openLinkAttrs(attrs []html.Attribute) []html.Attribute {
	for _, a := range attrs {
		if a.Key == "target" {
			return attrs // author intent wins
		}
	}
	return append(attrs,
		html.Attribute{Key: "target", Val: "_blank"},
		html.Attribute{Key: "rel", Val: "noopener noreferrer"},
	)
}

// safeURL reports whether a URL-valued attribute is safe to keep.
//
// The threat in an href/src sink is not "exotic scheme" but *active
// content*: schemes the browser interprets as script or as a document
// in the page's context. That set is small and has been stable for
// ~15 years — javascript:, data:, vbscript: — and no browser has since
// added a new auto-rendering scheme. Every other scheme is *navigable*:
// the browser dispatches the string to an external/OS handler (mailto:,
// tel:, magnet:, ed2k:, feed:, xmpp:, custom protocols) and no script
// runs in our origin. Allow-listing the navigable set means every such
// scheme breaks until someone adds it (the magnet: bug); the safer-AND-
// broader cut is to deny the script/document schemes and permit the
// rest.
//
// Relative references, scheme-relative ("//host/…") and fragment/query/
// path colons are kept. The scheme test is robust against the classic
// obfuscations — leading/embedded whitespace and control characters
// ("java\tscript:") and mixed case — because browsers strip those
// before resolving the scheme, so we must too.
func safeURL(v string) bool {
	// Strip all ASCII whitespace and C0 control chars anywhere up to
	// the first ':' the same way a browser's URL parser tolerates them.
	var stripped strings.Builder
	for _, r := range v {
		if r <= 0x20 || r == 0x7f {
			continue
		}
		stripped.WriteRune(r)
	}
	cleaned := stripped.String()
	colon := strings.IndexByte(cleaned, ':')
	if colon < 0 {
		return true // no scheme: relative reference
	}
	// A '/', '?' or '#' before the colon means the colon is part of a
	// path/query/fragment, not a scheme (e.g. "foo/bar:baz").
	if i := strings.IndexAny(cleaned, "/?#"); i >= 0 && i < colon {
		return true
	}
	// Deny-list: only the script/document schemes are unsafe in an
	// href/src context. Everything else is navigable and inert.
	return !dangerousScheme[strings.ToLower(cleaned[:colon])]
}

// dangerousScheme is the set of URL schemes that execute script or load
// attacker-authored markup as a document when navigated/rendered, and so
// must never survive sanitization of attacker-controlled feed HTML.
// blob: is included defensively: a feed cannot mint a live blob URL, but
// denying it costs nothing.
var dangerousScheme = map[string]bool{
	"javascript": true,
	"data":       true,
	"vbscript":   true,
	"blob":       true,
}

// LinkURL returns v as a template.URL when it is a scheme we trust to
// render as a user-followable href (the same policy safeURL applies to
// body links), or "" otherwise.
//
// It exists because html/template's built-in URL filter only trusts
// http/https/mailto and rewrites anything else — notably magnet: links
// from torrent feeds like showRSS — to the "#ZgotmplZ" placeholder,
// silently breaking the "source" link. Templates that render a feed
// item's Link must pass it through LinkURL and emit a template.URL so
// our wider scheme policy (safeURL: deny only script/document schemes)
// is what governs, then guard with {{if}} so an unsafe link is omitted
// rather than rendered as a dead "#ZgotmplZ" anchor.
func LinkURL(v string, rules map[string]string) template.URL {
	if v == "" || !safeURL(v) {
		return ""
	}
	return template.URL(linkrewrite.Host(v, rules))
}

// droppedSubtree are elements whose tag AND contents are removed — they
// are either active content (script, scripting hooks) or carry their
// own scripting/embedding surface that an allow-list can't tame.
var droppedSubtree = map[string]bool{
	"script": true, "style": true, "iframe": true, "object": true,
	"embed": true, "applet": true, "noscript": true, "link": true,
	"meta": true, "base": true, "form": true, "input": true,
	"button": true, "textarea": true, "select": true, "option": true,
	"svg": true, "math": true, "frame": true, "frameset": true,
	"title": true, "head": true, "template": true, "canvas": true,
	"audio": true, "video": true, "track": true, "portal": true,
}

// urlAttrs are attributes whose value is a URL and must be scheme-checked.
var urlAttrs = map[string]bool{
	"href": true, "src": true, "cite": true, "longdesc": true,
	"poster": true, "srcset": true,
}

// allowedAttrs maps each allow-listed element to its permitted
// attribute set. An element not present here is unwrapped (tag dropped,
// children kept) unless it is in droppedSubtree.
var allowedAttrs = map[string]map[string]bool{
	"a":          {"href": true, "title": true, "name": true, "target": true, "rel": true},
	"abbr":       {"title": true},
	"address":    {},
	"b":          {},
	"blockquote": {"cite": true},
	"br":         {},
	"caption":    {},
	"cite":       {},
	"code":       {},
	"col":        {"span": true},
	"colgroup":   {"span": true},
	"dd":         {},
	"del":        {"cite": true},
	"details":    {},
	"dfn":        {"title": true},
	"div":        {},
	"dl":         {},
	"dt":         {},
	"em":         {},
	"figcaption": {},
	"figure":     {},
	"h1":         {}, "h2": {}, "h3": {}, "h4": {}, "h5": {}, "h6": {},
	"hr":      {},
	"i":       {},
	"img":     {"src": true, "alt": true, "title": true, "width": true, "height": true},
	"ins":     {"cite": true},
	"kbd":     {},
	"li":      {"value": true},
	"mark":    {},
	"ol":      {"start": true, "reversed": true, "type": true},
	"p":       {},
	"pre":     {},
	"q":       {"cite": true},
	"s":       {},
	"samp":    {},
	"small":   {},
	"span":    {},
	"strike":  {},
	"strong":  {},
	"sub":     {},
	"summary": {},
	"sup":     {},
	"table":   {},
	"tbody":   {},
	"td":      {"colspan": true, "rowspan": true},
	"tfoot":   {},
	"th":      {"colspan": true, "rowspan": true, "scope": true},
	"thead":   {},
	"time":    {"datetime": true},
	"tr":      {},
	"u":       {},
	"ul":      {},
	"var":     {},
	"wbr":     {},
}

// sourceLinkClass marks the element poll-time enrichment wraps an
// aggregator entry's original discussion ("Comments") anchor in.
const sourceLinkClass = "enriched-source-link"

// hoistSourceLink moves the enrichment source-link marker — and the anchor
// it belongs to — to the front of fragment s, returning the reordered HTML
// (or s unchanged when there is nothing to move).
//
// Enrichment emits the marker at store time and stored Content is never
// rewritten on disk, so this repairs two legacy shapes at render time:
//
//   - Entries enriched before the ordering flipped store the marker as a
//     trailing footer; it is moved to the front.
//   - Entries enriched while the marker was a <p> store INVALID nesting:
//     the aggregator body is itself "<p><a>Comments</a></p>", and a <p>
//     inside a <p> is split by every HTML parser. The stored bytes
//     "<p class=marker><p><a>Comments</a></p></p>" therefore parse as
//     THREE siblings: an empty marker <p>, the real anchor <p>, and a
//     trailing empty <p> produced by the outer close tag. Hoisting the
//     marker alone would move an empty paragraph and strand the anchor.
//
// So when the marker parses empty, its group is extended forward through
// the split-out siblings up to and including the terminating empty <p>
// (the outer close tag), that whole group is moved to the front, and the
// empty paragraphs within it are dropped so no stray <p></p> is rendered.
// If no terminator is found the markup is not the known split shape, so
// the group stays just the marker itself rather than risk swallowing the
// article.
//
// It runs BEFORE sanitizeHTML, while the class attribute the marker is
// identified by still exists (the sanitizer's allow-list drops class),
// so the reordering survives sanitization even though the marker does
// not. Web UI only — GReader clients still see the stored order.
func hoistSourceLink(s string) string {
	if !strings.Contains(s, sourceLinkClass) {
		return s
	}
	ctx := &html.Node{Type: html.ElementNode, Data: "body", DataAtom: atom.Body}
	// ParseFragment with a valid context node is total on string input,
	// as in sanitizeHTML; an empty node set means nothing to hoist.
	nodes, _ := html.ParseFragment(strings.NewReader(s), ctx)
	idx := -1
	for i, n := range nodes {
		if isSourceLinkNode(n) {
			idx = i
			break
		}
	}
	if idx < 0 {
		return s
	}

	// end is exclusive: nodes[idx:end] is the marker's group.
	end := idx + 1
	if isEmptyParagraph(nodes[idx]) {
		for i := idx + 1; i < len(nodes); i++ {
			if isEmptyParagraph(nodes[i]) {
				// The outer </p> terminator: group ends here.
				end = i + 1
				break
			}
		}
	}

	group := make([]*html.Node, 0, end-idx)
	for _, n := range nodes[idx:end] {
		if isEmptyParagraph(n) {
			continue // drop the empty marker / orphan close-tag paragraph
		}
		group = append(group, n)
	}
	if idx == 0 && end == idx+1 && len(group) == 1 {
		// Already first, nothing split or dropped: pass the stored markup
		// through byte-for-byte rather than reparse-reserialise it.
		return s
	}

	reordered := make([]*html.Node, 0, len(nodes))
	reordered = append(reordered, group...)
	reordered = append(reordered, nodes[:idx]...)
	reordered = append(reordered, nodes[end:]...)
	var b strings.Builder
	for _, n := range reordered {
		// Render only fails on a Writer error; strings.Builder never errors.
		_ = html.Render(&b, n)
	}
	return b.String()
}

// isEmptyParagraph reports whether n is a <p> element carrying no
// meaningful content — no child elements and only whitespace text. These
// are the artefacts an HTML parser leaves behind when it splits the
// invalid p-inside-p nesting older enrichment produced.
func isEmptyParagraph(n *html.Node) bool {
	if n.Type != html.ElementNode || n.DataAtom != atom.P {
		return false
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type != html.TextNode || strings.TrimSpace(c.Data) != "" {
			return false
		}
	}
	return true
}

// isSourceLinkNode reports whether n is the element marked with
// sourceLinkClass by poll-time enrichment. Both the current <div> wrapper
// and the legacy <p> one are recognised, since stored content is never
// rewritten.
func isSourceLinkNode(n *html.Node) bool {
	if n.Type != html.ElementNode || (n.DataAtom != atom.Div && n.DataAtom != atom.P) {
		return false
	}
	for _, a := range n.Attr {
		if strings.ToLower(a.Key) != "class" {
			continue
		}
		for _, f := range strings.Fields(a.Val) {
			if f == sourceLinkClass {
				return true
			}
		}
	}
	return false
}

// isLinkOnly reports whether s, parsed as HTML, carries no meaningful
// content outside of <a> link labels — i.e. it is empty, whitespace, or
// only a bare link. Some feeds publish exactly this as content:encoded
// (a lone "Source"/"Read more" link), which is useless as an entry
// preview, so entryBody falls back to the feed's Summary in that case.
//
// Text inside an <a> is treated as a link label, not article content;
// any non-link text, or any embedded media (img, figure, video, …),
// counts as meaningful. A parse failure is reported as meaningful so a
// genuine body is never discarded on a tokenizer edge case.
func isLinkOnly(s string) bool {
	if strings.TrimSpace(s) == "" {
		return true
	}
	ctx := &html.Node{Type: html.ElementNode, Data: "body", DataAtom: atom.Body}
	// ParseFragment with a valid context node is total on string input
	// (the tokenizer never errors), so the error is ignored exactly as
	// sanitizeHTML does; an empty node set simply yields link-only=true.
	nodes, _ := html.ParseFragment(strings.NewReader(s), ctx)
	for _, n := range nodes {
		if hasMeaningfulContent(n, false) {
			return false
		}
	}
	return true
}

// hasMeaningfulContent reports whether node n (or any descendant) holds
// content worth showing as a preview. inLink is true when n is inside an
// <a> subtree, in which case its text is a link label and ignored;
// embedded media is meaningful regardless of link nesting.
func hasMeaningfulContent(n *html.Node, inLink bool) bool {
	switch n.Type {
	case html.TextNode:
		if inLink {
			return false
		}
		return strings.TrimSpace(n.Data) != ""
	case html.ElementNode:
		switch strings.ToLower(n.Data) {
		case "img", "picture", "figure", "video", "audio", "iframe", "embed", "object", "table":
			return true
		case "a":
			inLink = true
		}
	default:
		return false
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if hasMeaningfulContent(c, inLink) {
			return true
		}
	}
	return false
}

// htmlToText extracts a plain-text rendering of HTML fragment s: all
// tags are dropped and their text content concatenated, with runs of
// whitespace collapsed to single spaces and the result trimmed. Block
// elements are separated by a space so adjacent paragraphs/line breaks
// do not run words together. Used to synthesise a display title for
// feeds (e.g. Mastodon) whose items carry no <title>.
func htmlToText(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	ctx := &html.Node{Type: html.ElementNode, Data: "body", DataAtom: atom.Body}
	// ParseFragment with a valid context node is total on string input
	// (the tokenizer never errors), so the error is ignored as isLinkOnly
	// does; an empty node set simply yields the empty string.
	nodes, _ := html.ParseFragment(strings.NewReader(s), ctx)
	var b strings.Builder
	for _, n := range nodes {
		collectText(n, &b)
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// collectText walks n appending text-node data to b, inserting a space
// around block-level boundaries so words from separate blocks stay
// separated after whitespace collapsing.
func collectText(n *html.Node, b *strings.Builder) {
	switch n.Type {
	case html.TextNode:
		b.WriteString(n.Data)
		return
	case html.ElementNode:
		switch strings.ToLower(n.Data) {
		case "br", "p", "div", "li", "tr", "blockquote", "h1", "h2", "h3", "h4", "h5", "h6":
			b.WriteByte(0x20)
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		collectText(c, b)
	}
	if n.Type == html.ElementNode {
		switch strings.ToLower(n.Data) {
		case "p", "div", "li", "tr", "blockquote", "h1", "h2", "h3", "h4", "h5", "h6":
			b.WriteByte(0x20)
		}
	}
}
