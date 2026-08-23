// Package linkrewrite implements harb's opt-in host→host link rewriting.
//
// It is shared by the two surfaces that serve article links: the web UI
// (internal/ui — sanitizer <a href> pass and the entry's own source
// link) and the Google Reader API (internal/reader — anchors inside the
// served item body). Both take their rules from the same resolved
// configuration value (Config.LinkRewrite, see internal/config).
//
// Host is the pure host-mapping primitive; Anchors is the byte-faithful
// <a href>-only pass used where content must otherwise be served
// verbatim.
package linkrewrite

import (
	"net"
	"net/url"
	"strings"
)

// Host remaps the HOST of an outbound link through the
// configured host→host rules (link_rewrite in config.json),
// returning raw unchanged when no rule applies.
//
// Why: some sites are unusable (or hostile) when followed directly —
// x.com being the motivating case — and a user may prefer a front-end
// mirror such as xcancel.com. The map is empty by default, so harb
// itself stays neutral; enabling it is an explicit operator choice.
//
// Scope is deliberately narrow — this is a SERVE-LAYER transform only,
// applied to rendered/served link values and never to stored data:
//
//   - it runs on the web UI's <a href> values, on the entry's own
//     source link (LinkURL), and on <a href> values inside the article
//     body the Reader API serves; never on a Reader item's `id` or
//     `alternate[].href`, which native clients treat as
//     identity-adjacent (dedupe / read state);
//   - never on src / poster / srcset / cite — rewriting an image host
//     breaks the image, and srcset is multi-URL syntax;
//   - it runs AFTER the sanitizer's scheme/safety check, which judges
//     the ORIGINAL URL, so a dropped href can never be resurrected.
//
// Rules:
//
//   - only absolute http/https URLs with a non-empty host qualify;
//     relative references, mailto:, magnet: and friends pass through;
//   - userinfo-bearing URLs are left alone (credentials must not be
//     handed to a different host);
//   - matching is case-insensitive: exact host, else the host with one
//     leading "www." stripped, else a suffix match on "."+key so
//     mobile.twitter.com follows twitter.com (longest key wins);
//   - only the host is replaced; scheme, port, path, query and fragment
//     survive intact;
//   - the map is applied exactly ONCE — never to a fixpoint — so a
//     mutually-recursive map (a→b, b→a) cannot loop. A rule whose
//     result equals the input host is a no-op.
//
// Deliberately failing open (no rewrite) rather than guessing: a
// trailing-dot host ("x.com.") and IDN/punycode mismatches (a unicode
// rule key against an xn-- host, or vice versa) do not match. Rules are
// expected in the same form the feed publishes. When a URL IS rewritten
// it is re-serialised by net/url, which may normalise percent-escapes —
// harmless for navigation, and only ever on links the operator opted
// into rewriting.
//
// Junk rules (empty, or carrying a scheme, path, port, whitespace…)
// are ignored here rather than failing the whole config load: a typo in
// one entry must not take the server down.
func Host(raw string, rules map[string]string) string {
	if raw == "" || len(rules) == 0 {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	// url.Parse already lower-cases the scheme.
	switch u.Scheme {
	case "http", "https":
	default:
		return raw
	}
	if u.Host == "" || u.User != nil {
		return raw
	}
	repl, ok := lookupHostRule(u.Hostname(), rules)
	if !ok {
		return raw
	}
	host := repl
	if port := u.Port(); port != "" {
		host = net.JoinHostPort(repl, port)
	}
	if strings.EqualFold(host, u.Host) {
		return raw // no-op rule (incl. a→a)
	}
	u.Host = host
	return u.String()
}

// lookupHostRule resolves host against rules, applying exact, www.- and
// suffix matching in that order (see Host). It returns the
// replacement host and whether any rule matched.
func lookupHostRule(host string, rules map[string]string) (string, bool) {
	h := strings.ToLower(host)
	cands := []string{h}
	if bare := strings.TrimPrefix(h, "www."); bare != h {
		cands = append(cands, bare)
	}
	for _, c := range cands {
		for k, v := range rules {
			if !validRule(k, v) {
				continue
			}
			if strings.ToLower(k) == c {
				return strings.ToLower(v), true
			}
		}
	}
	// Suffix match on "."+key, so sub.domain.example follows
	// example. Longest matching key wins, which keeps the result
	// deterministic despite Go's randomised map iteration.
	best, repl := "", ""
	for k, v := range rules {
		if !validRule(k, v) {
			continue
		}
		lk := strings.ToLower(k)
		if strings.HasSuffix(h, "."+lk) && len(lk) > len(best) {
			best, repl = lk, strings.ToLower(v)
		}
	}
	return repl, best != ""
}

// validRule reports whether a link_rewrite entry is usable: both sides
// must be bare hosts — non-empty, and free of scheme, path, port,
// userinfo, query/fragment and whitespace.
func validRule(key, val string) bool {
	return validRuleHost(key) && validRuleHost(val)
}

func validRuleHost(h string) bool {
	if h == "" {
		return false
	}
	return !strings.ContainsAny(h, ":/\\@?# \t")
}
