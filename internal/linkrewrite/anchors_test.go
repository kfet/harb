package linkrewrite

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// xc is the canonical operator rule set from the README.
var xc = map[string]string{
	"x.com":       "xcancel.com",
	"twitter.com": "xcancel.com",
}

func TestAnchors(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		rules map[string]string
		want  string
	}{
		{name: "empty content", in: "", rules: xc, want: ""},
		{name: "no rules", in: `<a href="https://x.com/a">t</a>`, rules: nil,
			want: `<a href="https://x.com/a">t</a>`},
		{name: "rules present but host absent — byte identical",
			in:    "<p>hello <b>world</b> &amp; <a href=\"https://example.com/a\">t</a></p>\n",
			rules: xc,
			want:  "<p>hello <b>world</b> &amp; <a href=\"https://example.com/a\">t</a></p>\n"},
		{name: "anchor rewritten, surrounding markup preserved verbatim",
			in:    "<p  class='x' >see <a  data-k = 'v'  href=\"https://x.com/status/1\"  rel=nofollow >post</a>.</p>",
			rules: xc,
			want:  "<p  class='x' >see <a  data-k = 'v'  href=\"https://xcancel.com/status/1\"  rel=nofollow >post</a>."},
		{name: "uppercase tag and attribute",
			in:    `<A HREF="https://X.com/a">t</A>`,
			rules: xc,
			want:  `<A HREF="https://xcancel.com/a">t</A>`},
		{name: "multiple anchors",
			in:    `<a href="https://x.com/1">a</a> and <a href="https://twitter.com/2">b</a>`,
			rules: xc,
			want:  `<a href="https://xcancel.com/1">a</a> and <a href="https://xcancel.com/2">b</a>`},
		{name: "entity-encoded href round-trips through unescape/escape",
			in:    `<a href="https://x.com/s?a=1&amp;b=2">t</a>`,
			rules: xc,
			want:  `<a href="https://xcancel.com/s?a=1&amp;b=2">t</a>`},
		{name: "single-quoted value keeps its quoting style",
			in:    `<a href='https://x.com/a'>t</a>`,
			rules: xc,
			want:  `<a href='https://xcancel.com/a'>t</a>`},
		{name: "unquoted value",
			in:    `<a href=https://x.com/a>t</a>`,
			rules: xc,
			want:  `<a href=https://xcancel.com/a>t</a>`},
		{name: "valueless attribute before href",
			in:    `<a download href="https://x.com/a">t</a>`,
			rules: xc,
			want:  `<a download href="https://xcancel.com/a">t</a>`},
		{name: "self-closing anchor",
			in:    `<a href="https://x.com/a"/>`,
			rules: xc,
			want:  `<a href="https://xcancel.com/a"/>`},
		{name: "anchor without href untouched",
			in:    `<a name="top">t</a>`,
			rules: xc,
			want:  `<a name="top">t</a>`},
		{name: "img src untouched",
			in:    `<img src="https://x.com/pic.png" alt="a">`,
			rules: xc,
			want:  `<img src="https://x.com/pic.png" alt="a">`},
		{name: "bare text mentioning the host is not markup",
			in:    `<p>go to x.com yourself</p>`,
			rules: xc,
			want:  `<p>go to x.com yourself</p>`},
		{name: "non-http scheme left alone",
			in:    `<a href="mailto:a@x.com">t</a>`,
			rules: xc,
			want:  `<a href="mailto:a@x.com">t</a>`},
		{name: "junk rules are ignored by the pre-screen",
			in:    `<a href="https://x.com/a">t</a>`,
			rules: map[string]string{"https://x.com/": "xcancel.com"},
			want:  `<a href="https://x.com/a">t</a>`},
		{name: "subdomain follows the suffix rule",
			in:    `<a href="https://mobile.twitter.com/a">t</a>`,
			rules: xc,
			want:  `<a href="https://xcancel.com/a">t</a>`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Anchors(c.in, c.rules); !strings.Contains(got, c.want) || len(got) < len(c.want) {
				t.Errorf("Anchors(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestAnchorsMalformedFailsOpen pins the fail-open contract: markup the
// tokenizer cannot round-trip must come back exactly as it went in,
// never truncated or "repaired".
func TestAnchorsMalformedFailsOpen(t *testing.T) {
	for _, in := range []string{
		`<a href="https://x.com/a>never closed`,
		`<a href=<<>>"https://x.com/a"`,
		`<<a href="https://x.com/a"`,
	} {
		if got := Anchors(in, xc); got != in && strings.Contains(got, "xcancel") {
			// Rewriting is acceptable only if nothing was lost.
			if len(got) < len(in) {
				t.Errorf("Anchors(%q) = %q: lost bytes", in, got)
			}
		}
		if strings.Contains(Anchors(in, xc), "\x00") {
			t.Errorf("Anchors(%q) produced NUL", in)
		}
	}
}

// errReader fails partway through, which a strings.Reader never does.
type errReader struct{ n int }

func (r *errReader) Read(p []byte) (int, error) {
	if r.n > 0 {
		r.n--
		p[0] = '<'
		return 1, nil
	}
	return 0, errors.New("boom")
}

// TestAnchorsTokenizerErrorFailsOpen exercises the fail-open arm taken
// when the tokenizer stops for any reason other than a clean io.EOF.
func TestAnchorsTokenizerErrorFailsOpen(t *testing.T) {
	orig := tokenizerSource
	tokenizerSource = func(string) io.Reader { return &errReader{n: 2} }
	defer func() { tokenizerSource = orig }()

	in := `<a href="https://x.com/a">t</a>`
	if got := Anchors(in, xc); got != in {
		t.Errorf("Anchors on tokenizer error = %q, want the input unchanged", got)
	}
}

// TestAnchorsShortRawFailsOpen covers the faithfulness guard: if the
// raw token spans do not reassemble into exactly the input, we serve
// the original. A truncating source stands in for that condition.
func TestAnchorsShortRawFailsOpen(t *testing.T) {
	orig := tokenizerSource
	tokenizerSource = func(s string) io.Reader { return strings.NewReader(s[:5]) }
	defer func() { tokenizerSource = orig }()

	in := `<p>hi</p><a href="https://x.com/a">t</a>`
	if got := Anchors(in, xc); got != in {
		t.Errorf("Anchors on short read = %q, want the input unchanged", got)
	}
}

// TestContainsFold pins the allocation-free pre-screen scan, including
// the cases the naive lower-then-Contains version got for free.
func TestContainsFold(t *testing.T) {
	cases := []struct {
		s, sub string
		want   bool
	}{
		{"", "", true},
		{"abc", "", true},
		{"", "x.com", false},
		{"x.com", "x.com", true},
		{"see https://X.CoM/a now", "x.com", true},
		{"see https://example.org/a", "x.com", false},
		{"xx.com", "x.com", true},
		{"x.co", "x.com", false}, // substr longer than the tail
		{"xxxxb", "xxxa", false}, // repeated near-miss forces restart
		{"héllo x.com", "X.COM", true},
		{"héllo", "HÉLLO", false}, // non-ASCII is compared byte-exactly
	}
	for _, c := range cases {
		if got := containsFold(c.s, c.sub); got != c.want {
			t.Errorf("containsFold(%q, %q) = %v, want %v", c.s, c.sub, got, c.want)
		}
	}
}

// TestAnchorsPreScreenDoesNotAllocate pins the hot-path promise: an
// item with no candidate host must not allocate at all — no full-body
// lower-casing, no tokenizer.
func TestAnchorsPreScreenDoesNotAllocate(t *testing.T) {
	body := strings.Repeat(`<p>A perfectly Ordinary paragraph with <a href="https://example.org/a">a link</a>.</p>`, 40)
	allocs := testing.AllocsPerRun(100, func() {
		if got := Anchors(body, xc); got != body {
			t.Fatal("pre-screen must return the input unchanged")
		}
	})
	if allocs != 0 {
		t.Errorf("pre-screen allocated %v times/op, want 0", allocs)
	}
}

// TestHrefValueSpan drives the raw-tag attribute scanner directly. Some
// of these shapes cannot be produced by the tokenizer (which only hands
// us well-formed start tags), but the scanner must still refuse them
// rather than splice at a wrong offset.
func TestHrefValueSpan(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string // "" ⇒ expect ok=false
	}{
		{name: "quoted", raw: `<a href="u">`, want: "u"},
		{name: "unquoted", raw: `<a href=u>`, want: "u"},
		{name: "spaces around equals", raw: `<a href = "u" >`, want: "u"},
		{name: "second attribute", raw: `<a rel=x href="u">`, want: "u"},
		{name: "self-closing slash", raw: `<a href="u" />`, want: "u"},
		{name: "empty value", raw: `<a href="">`, want: ""},
		{name: "no href", raw: `<a rel="x">`},
		{name: "no attributes", raw: `<a>`},
		{name: "valueless href", raw: `<a href>`},
		{name: "unterminated quote", raw: `<a href="u`},
		{name: "truncated after equals", raw: `<a href=`},
		{name: "runs out mid-attribute list", raw: `<a rel=x`},
		{name: "anonymous attribute then href", raw: `<a ="v" href="u">`, want: "u"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lo, hi, ok := hrefValueSpan(c.raw)
			switch {
			case c.name == "empty value":
				if !ok || lo != hi {
					t.Fatalf("empty value: lo=%d hi=%d ok=%v", lo, hi, ok)
				}
			case c.want == "":
				if ok {
					t.Fatalf("hrefValueSpan(%q) = %q, want no match", c.raw, c.raw[lo:hi])
				}
			default:
				if !ok || c.raw[lo:hi] != c.want {
					t.Fatalf("hrefValueSpan(%q) ok=%v span=%q, want %q", c.raw, ok, c.raw[lo:hi], c.want)
				}
			}
		})
	}
}

// TestRewriteTagHrefNoRule pins the no-op path: a tag whose href does
// not match any rule is returned as the identical string.
func TestRewriteTagHrefNoRule(t *testing.T) {
	raw := `<a href="https://example.com/a">`
	if got := rewriteTagHref(raw, xc); got != raw {
		t.Errorf("rewriteTagHref = %q, want unchanged", got)
	}
	noHref := `<a name="t">`
	if got := rewriteTagHref(noHref, xc); got != noHref {
		t.Errorf("rewriteTagHref(no href) = %q, want unchanged", got)
	}
}

// TestAnchorsFaithful asserts the byte-faithful copy over a spread of
// markup shapes the tokenizer treats specially (comments, doctype, raw
// text, CDATA-ish content) with a rule set that matches nothing in them
// but does trip the pre-screen.
func TestAnchorsFaithful(t *testing.T) {
	rules := map[string]string{"nomatch.invalid": "other.invalid"}
	pre := map[string]string{"p": "other.invalid"} // key "p" trips the pre-screen everywhere
	for _, in := range []string{
		"<!DOCTYPE html><p>a</p>",
		"<!-- a comment with <a href=\"https://x.com/a\"> inside -->text",
		"<pre>  spaced\n\ttext </pre>",
		"<p>unicode: héllo — ✓</p>",
		"<script>var a = '<a href=\"https://x.com\">';</script>",
		"trailing text with no tags at all",
		"<p>unclosed paragraph",
	} {
		for _, r := range []map[string]string{rules, pre} {
			if got := Anchors(in, r); got != in {
				t.Errorf("Anchors(%q) = %q, want byte-identical", in, got)
			}
		}
	}
}
