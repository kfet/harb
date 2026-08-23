package linkrewrite

import (
	"testing"
)

// xcancel is the canonical operator rule set from the README.
var xcancel = map[string]string{
	"x.com":       "xcancel.com",
	"twitter.com": "xcancel.com",
}

func TestHost(t *testing.T) {
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
			if got := Host(c.in, c.rules); got != c.want {
				t.Errorf("Host(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestHostAppliedOnce pins the single-application rule: a
// mutually recursive map (a→b, b→a) must swap once and stop, never
// iterate to a fixpoint (which would not terminate).
func TestHostAppliedOnce(t *testing.T) {
	rules := map[string]string{"a.invalid": "b.invalid", "b.invalid": "a.invalid"}
	if got := Host("https://a.invalid/p", rules); got != "https://b.invalid/p" {
		t.Fatalf("a→b: got %q", got)
	}
	if got := Host("https://b.invalid/p", rules); got != "https://a.invalid/p" {
		t.Fatalf("b→a: got %q", got)
	}
}
