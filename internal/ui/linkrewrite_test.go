package ui

import (
	"strings"
	"testing"
)

// xcancel is the canonical operator rule set from the README.
var xcancel = map[string]string{
	"x.com":       "xcancel.com",
	"twitter.com": "xcancel.com",
}

func TestRewriteLinkHost(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		rules map[string]string
		want  string
	}{
		{"exact host", "https://x.com/foo", xcancel, "https://xcancel.com/foo"},
		{"www stripped", "https://www.x.com/foo", xcancel, "https://xcancel.com/foo"},
		{"subdomain suffix", "https://mobile.twitter.com/a/b", xcancel, "https://xcancel.com/a/b"},
		{"path query fragment kept", "https://x.com/a/b?c=d&e=f#g", xcancel, "https://xcancel.com/a/b?c=d&e=f#g"},
		{"http kept as http", "http://x.com/foo", xcancel, "http://xcancel.com/foo"},
		{"uppercase host", "https://X.CoM/foo", xcancel, "https://xcancel.com/foo"},
		{"port preserved", "https://x.com:8443/foo", xcancel, "https://xcancel.com:8443/foo"},
		{"no rule pass-through", "https://example.com/foo", xcancel, "https://example.com/foo"},
		{"empty rules", "https://x.com/foo", nil, "https://x.com/foo"},
		{"empty url", "", xcancel, ""},
		{"relative untouched", "/local/path", xcancel, "/local/path"},
		{"scheme-relative untouched", "//x.com/foo", xcancel, "//x.com/foo"},
		{"mailto untouched", "mailto:a@x.com", xcancel, "mailto:a@x.com"},
		{"magnet untouched", "magnet:?xt=urn:btih:deadbeef", xcancel, "magnet:?xt=urn:btih:deadbeef"},
		{"userinfo untouched", "https://user:pw@x.com/foo", xcancel, "https://user:pw@x.com/foo"},
		{"unparseable untouched", "https://x.com/%zz\x7f%", xcancel, "https://x.com/%zz\x7f%"},
		{"empty host untouched", "https:///foo", xcancel, "https:///foo"},
		{"identity rule is a no-op", "https://x.com/foo", map[string]string{"x.com": "X.com"}, "https://x.com/foo"},
		{"junk key ignored", "https://x.com/foo", map[string]string{"https://x.com": "xcancel.com"}, "https://x.com/foo"},
		{"junk value ignored", "https://x.com/foo", map[string]string{"x.com": "xcancel.com/path"}, "https://x.com/foo"},
		{"empty key ignored", "https://x.com/foo", map[string]string{"": "xcancel.com"}, "https://x.com/foo"},
		{"empty value ignored", "https://x.com/foo", map[string]string{"x.com": ""}, "https://x.com/foo"},
		{"junk suffix rule ignored", "https://a.x.com/foo", map[string]string{"x.com:443": "xcancel.com"}, "https://a.x.com/foo"},
		{"longest suffix key wins", "https://a.b.example.com/x",
			map[string]string{"example.com": "one.invalid", "b.example.com": "two.invalid"},
			"https://two.invalid/x"},
		{"www then suffix", "https://www.a.twitter.com/x", xcancel, "https://xcancel.com/x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rewriteLinkHost(c.in, c.rules); got != c.want {
				t.Errorf("rewriteLinkHost(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestRewriteLinkHostAppliedOnce pins the single-application rule: a
// mutually recursive map (a→b, b→a) must swap once and stop, never
// iterate to a fixpoint (which would not terminate).
func TestRewriteLinkHostAppliedOnce(t *testing.T) {
	rules := map[string]string{"a.invalid": "b.invalid", "b.invalid": "a.invalid"}
	if got := rewriteLinkHost("https://a.invalid/p", rules); got != "https://b.invalid/p" {
		t.Fatalf("a→b: got %q", got)
	}
	if got := rewriteLinkHost("https://b.invalid/p", rules); got != "https://a.invalid/p" {
		t.Fatalf("b→a: got %q", got)
	}
}

// TestSanitizeLinkRewrite covers the sanitizer call site: anchors are
// rewritten, media/citation URL attributes never are.
func TestSanitizeLinkRewrite(t *testing.T) {
	cases := []struct {
		name          string
		in            string
		want, notWant string
		rules         map[string]string
	}{
		{name: "anchor href rewritten",
			in:    `<a href="https://x.com/a">t</a>`,
			want:  `href="https://xcancel.com/a"`,
			rules: xcancel},
		{name: "img src untouched",
			in:    `<img src="https://x.com/pic.png" alt="a">`,
			want:  `src="https://x.com/pic.png"`,
			rules: xcancel},
		// srcset is multi-URL syntax and never rewritten; the img
		// allow-list drops it outright, and nothing leaks a rewrite.
		{name: "srcset untouched",
			in:      `<img src="https://x.com/a.png" srcset="https://x.com/a.png 1x, https://x.com/b.png 2x">`,
			want:    `src="https://x.com/a.png"`,
			notWant: "xcancel",
			rules:   xcancel},
		{name: "cite untouched",
			in:    `<blockquote cite="https://x.com/q">q</blockquote>`,
			want:  `cite="https://x.com/q"`,
			rules: xcancel},
		{name: "no rules leaves anchor alone",
			in:    `<a href="https://x.com/a">t</a>`,
			want:  `href="https://x.com/a"`,
			rules: nil},
		{name: "dropped href is not resurrected",
			in:      `<a href="javascript:alert(1)//x.com">t</a>`,
			want:    `<a target="_blank"`,
			notWant: "javascript",
			rules:   xcancel},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := sanitizeHTML(c.in, c.rules)
			if !strings.Contains(got, c.want) {
				t.Errorf("sanitizeHTML(%q) = %q, want substring %q", c.in, got, c.want)
			}
			if c.notWant != "" && strings.Contains(got, c.notWant) {
				t.Errorf("sanitizeHTML(%q) = %q, must not contain %q", c.in, got, c.notWant)
			}
		})
	}
}

// TestLinkURLRewrite covers the second call site: the entry's own Link.
func TestLinkURLRewrite(t *testing.T) {
	if got := LinkURL("https://x.com/status/1", xcancel); string(got) != "https://xcancel.com/status/1" {
		t.Errorf("LinkURL rewrite = %q", got)
	}
	if got := LinkURL("magnet:?xt=urn:btih:x", xcancel); string(got) != "magnet:?xt=urn:btih:x" {
		t.Errorf("LinkURL magnet = %q", got)
	}
	if got := LinkURL("javascript:alert(1)", xcancel); got != "" {
		t.Errorf("LinkURL unsafe = %q, want empty", got)
	}
	if got := LinkURL("", xcancel); got != "" {
		t.Errorf("LinkURL empty = %q, want empty", got)
	}
}
