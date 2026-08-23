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
