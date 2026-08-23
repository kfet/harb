package linkrewrite

import (
	"io"
	"strings"

	"golang.org/x/net/html"
)

// Hookable for testing. In production the tokenizer always reads from an
// in-memory strings.Reader, which never fails — so the "tokenizer broke"
// fail-open arm below has no natural trigger. A test swaps this for a
// reader that errors mid-stream to prove the arm serves the original
// content rather than a truncated body.
var tokenizerSource = func(s string) io.Reader { return strings.NewReader(s) }

// Anchors rewrites the host of every <a href> in an HTML fragment
// through rules, copying every other byte of content through
// UNCHANGED, and returns content untouched when nothing applies.
//
// Why not the ui sanitizer: the Reader API's contract is "serve the
// publisher's content verbatim; the client sanitizes". A
// parse→clean→re-render round trip would silently normalise markup we
// promised not to touch (and costs a full parse per item). This pass
// therefore tokenizes and splices: each token is emitted as its exact
// raw bytes, and only the value span of an <a> tag's href attribute is
// ever replaced.
//
// It FAILS OPEN. If the tokenizer reports anything other than a clean
// io.EOF, or if the raw token bytes do not reassemble into exactly the
// input, the original content is returned unchanged. Serving an
// un-rewritten link is a cosmetic miss; serving mangled HTML is a bug.
//
// Cost control, in order:
//
//   - no rules → immediate return (a zero-config deployment pays
//     nothing);
//   - an allocation-free case-folded substring pre-screen for each rule
//     key → items with no candidate host (the overwhelming majority)
//     pay only a couple of scans and never tokenize.
//
// Only the href of an <a> is considered: src / poster / srcset / cite
// are left alone exactly as in the UI sanitizer (rewriting an image
// host breaks the image; srcset is multi-URL syntax). Tag and attribute
// names are matched case-insensitively, so <A HREF=...> is rewritten.
func Anchors(content string, rules map[string]string) string {
	if content == "" || len(rules) == 0 || !mayContainRuleHost(content, rules) {
		return content
	}
	var b strings.Builder
	b.Grow(len(content) + 32)
	raws := 0
	z := html.NewTokenizer(tokenizerSource(content))
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			if z.Err() != io.EOF || raws != len(content) {
				return content // fail open
			}
			return b.String()
		}
		raw := string(z.Raw())
		raws += len(raw)
		if (tt == html.StartTagToken || tt == html.SelfClosingTagToken) && isAnchorTag(raw) {
			b.WriteString(rewriteTagHref(raw, rules))
			continue
		}
		b.WriteString(raw)
	}
}

// isAnchorTag reports whether the raw bytes of a start tag open an <a>
// element. We read the name straight out of the raw token rather than
// calling Tokenizer.TagName, which lower-cases the tag name IN PLACE in
// the tokenizer's buffer and would therefore corrupt our verbatim copy
// of an upper-case <A>.
func isAnchorTag(raw string) bool {
	if len(raw) < 2 || raw[0] != '<' || (raw[1] != 'a' && raw[1] != 'A') {
		return false
	}
	return len(raw) == 2 || isTagSpace(raw[2]) || raw[2] == '>' || raw[2] == '/'
}

// mayContainRuleHost is the pre-screen: it reports whether content
// mentions any rule key at all. Cheap and deliberately over-eager — a
// false positive only costs a tokenize pass that changes nothing.
//
// It uses containsFold rather than strings.Contains(strings.ToLower(…)):
// article bodies essentially always contain an upper-case letter, so
// ToLower would allocate a full copy of EVERY item's body on the sync
// hot path just to answer a question that is almost always "no".
func mayContainRuleHost(content string, rules map[string]string) bool {
	for k, v := range rules {
		if validRule(k, v) && containsFold(content, k) {
			return true
		}
	}
	return false
}

// containsFold reports whether s contains substr under ASCII
// case-folding, without allocating. Rule keys are hosts — short — so
// the naive scan is the right shape here. Non-ASCII bytes compare
// exactly, which is what we want: harb does not attempt IDN case
// folding (see Host's documented punycode caveat).
func containsFold(s, substr string) bool {
	n := len(substr)
	if n == 0 {
		return true
	}
	c0 := lowerASCII(substr[0])
	for i := 0; i+n <= len(s); i++ {
		if lowerASCII(s[i]) != c0 {
			continue
		}
		j := 1
		for ; j < n; j++ {
			if lowerASCII(s[i+j]) != lowerASCII(substr[j]) {
				break
			}
		}
		if j == n {
			return true
		}
	}
	return false
}

func lowerASCII(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

// rewriteTagHref takes the RAW bytes of a single <a> start tag and
// returns them with only the href attribute's value span replaced (when
// a rule applies). Everything else — whitespace, attribute order,
// quoting style, casing — is preserved byte for byte.
func rewriteTagHref(raw string, rules map[string]string) string {
	lo, hi, ok := hrefValueSpan(raw)
	if !ok {
		return raw
	}
	old := html.UnescapeString(raw[lo:hi])
	repl := Host(old, rules)
	if repl == old {
		return raw
	}
	return raw[:lo] + html.EscapeString(repl) + raw[hi:]
}

// hrefValueSpan locates the byte span of the href attribute's value
// inside the raw bytes of a start tag, following the HTML attribute
// grammar (quoted or unquoted values, case-insensitive names). It
// returns ok=false when the tag has no href, or an href with no value.
//
// The span excludes the surrounding quotes, so splicing a replacement
// in keeps the original quoting style.
func hrefValueSpan(raw string) (int, int, bool) {
	i := 0
	// Skip "<" and the tag name.
	for i < len(raw) && !isTagSpace(raw[i]) && raw[i] != '/' && raw[i] != '>' {
		i++
	}
	for i < len(raw) {
		for i < len(raw) && (isTagSpace(raw[i]) || raw[i] == '/') {
			i++
		}
		if i >= len(raw) || raw[i] == '>' {
			return 0, 0, false
		}
		nameStart := i
		for i < len(raw) && !isTagSpace(raw[i]) && raw[i] != '=' && raw[i] != '/' && raw[i] != '>' {
			i++
		}
		name := raw[nameStart:i]
		for i < len(raw) && isTagSpace(raw[i]) {
			i++
		}
		if i >= len(raw) || raw[i] != '=' {
			continue // valueless attribute
		}
		i++ // consume '='
		for i < len(raw) && isTagSpace(raw[i]) {
			i++
		}
		if i >= len(raw) {
			return 0, 0, false
		}
		valStart, valEnd := i, i
		if q := raw[i]; q == '"' || q == '\'' {
			valStart = i + 1
			valEnd = valStart
			for valEnd < len(raw) && raw[valEnd] != q {
				valEnd++
			}
			if valEnd >= len(raw) {
				return 0, 0, false // unterminated quote
			}
			i = valEnd + 1
		} else {
			for valEnd < len(raw) && !isTagSpace(raw[valEnd]) && raw[valEnd] != '>' {
				valEnd++
			}
			i = valEnd
		}
		if strings.EqualFold(name, "href") {
			return valStart, valEnd, true
		}
	}
	return 0, 0, false
}

// isTagSpace reports whether c is whitespace as the HTML tokenizer
// counts it when separating attributes.
func isTagSpace(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '\f':
		return true
	}
	return false
}
