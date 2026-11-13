// Package sign builds the headers MiniMax's account API expects.
//
// MiniMax Code signs each account API request: "yy" over the path, the
// query and the body, and "x-signature" over the time and the body. The
// secret is MiniMax Code's own.
//
// Three details make this different from an ordinary query builder, and
// all three come from how JavaScript does it:
//
//   - the query keeps its insertion order. URLSearchParams does not sort
//     its keys, and the signature is over the encoded text, so the order
//     is part of the contract. Go's url.Values.Encode() sorts, and would
//     produce a different signature every time;
//   - the encoding is JavaScript's. URLSearchParams keeps "*-._", encodes
//     a space as "+" and encodes "~"; encodeURIComponent keeps
//     "!*'()~", encodes a space as "%20" and does not touch "~". Go's
//     url.QueryEscape does neither exactly;
//   - the path is resolved before it is signed. The reference resolves
//     the path against the origin with `new URL(path, origin)` and signs
//     the resolved, percent-encoded path, so a path without a leading
//     slash, a path with a space in it and a path with a query of its own
//     all sign something other than what they look like.
package sign

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"mmxcode/internal/site"
)

// Secret is MiniMax Code's request-signing secret.
const Secret = "I*7Cf%WZ#S&%1RlZJ&C2"

// KV is one query parameter.
type KV struct {
	K string
	V string
}

// Options is what a request's signature is made of.
type Options struct {
	// Access is the account's access token, put in Authorization.
	Access string
	// UserID is the account's MiniMax user id. It is trimmed the way
	// JavaScript trims a string — the WhiteSpace and LineTerminator
	// characters, which is not quite what strings.TrimSpace does — and
	// an empty one becomes "0".
	UserID string
	// Body is the request's JSON body. Its bytes go into both hashes
	// as they are, so the caller must pass exactly the bytes it will
	// put on the wire. Go's json.Marshal is not JSON.stringify: it
	// escapes "<", ">" and "&" and it sorts a map's keys, and
	// JSON.stringify does neither. A body that has to match the
	// reference byte for byte is built by hand.
	//
	// A nil or empty body means a GET.
	Body []byte
	// Query holds the caller's extra query parameters, in order.
	//
	// The reference builds its query as an object literal with the
	// caller's query spread into the middle of it, and the two halves
	// do not treat a repeated key the same way:
	//
	//   - the four leading defaults — device_platform, biz_id, app_id
	//     and version_code — come before the spread, so a repeated key
	//     takes the caller's value and keeps the caller's place;
	//   - the trailing defaults — unix, timezone_offset, sys_language,
	//     lang, device_id, os_name, browser_name, user_id and client —
	//     come after it, so a repeated key keeps the caller's place but
	//     takes the reference's value. "lang" and "client" are the
	//     reference's, "user_id" is the option's.
	Query []KV
	// Now is the request's time in Unix milliseconds.
	//
	// It is required. There is no default on purpose — a default of
	// time.Now() would make a signed request unreproducible — and the
	// zero value signs the Unix epoch, which the server reads as a
	// stale request.
	Now int64
	// TZOffset is the client's timezone offset in seconds.
	TZOffset int
	// OSName is the client's platform name. An empty one is "linux".
	OSName string
	// Lang is the site's language. An empty one is the site's own.
	Lang string
}

// Signed is a signed request's URL and headers.
type Signed struct {
	// URL is the full URL, query included.
	URL string
	// YY is the header "yy".
	YY string
	// Timestamp is the header "x-timestamp", in seconds.
	Timestamp string
	// Signature is the header "x-signature".
	Signature string

	// access is the account's access token, put in Authorization.
	access string
}

// Headers gives the headers a signed request carries.
func (s Signed) Headers() map[string]string {
	return map[string]string{
		"Accept":        "application/json",
		"Content-Type":  "application/json",
		"User-Agent":    site.AccountUserAgent,
		"Authorization": "Bearer " + s.access,
		"yy":            s.YY,
		"x-timestamp":   s.Timestamp,
		"x-signature":   s.Signature,
	}
}

// Sign builds the signed request for a path on origin.
func Sign(origin, path string, o Options) Signed {
	lang := o.Lang
	if lang == "" {
		lang = site.Lang
	}
	osName := o.OSName
	if osName == "" {
		osName = "linux"
	}

	p := newParams()
	p.set("device_platform", "mcode")
	p.set("biz_id", "3")
	p.set("app_id", "3001")
	p.set("version_code", "22201")
	for _, kv := range o.Query {
		p.set(kv.K, kv.V)
	}
	p.set("unix", strconv.FormatInt(o.Now, 10))
	p.set("timezone_offset", strconv.Itoa(o.TZOffset))
	p.set("sys_language", lang)
	p.set("lang", lang)
	p.set("device_id", "0")
	p.set("os_name", osName)
	p.set("browser_name", "mcode")
	p.set("user_id", jsTrim(o.UserID))
	if p.get("user_id") == "" {
		p.set("user_id", "0")
	}
	p.set("client", "mcode")

	query := p.encode()
	target, at := resolveURL(origin, path, query)
	ts := o.Now / 1000
	if o.Now < 0 && o.Now%1000 != 0 {
		// The reference floors the seconds and Go's division truncates
		// towards zero, so -1500 milliseconds is -2 seconds there and
		// -1 second here.
		ts--
	}
	nowText := strconv.FormatInt(o.Now, 10)

	// The reference's signed() — packages/minimax/index.mjs of
	// @magpie-community/opencode-minimax-auth, the one copied verbatim
	// into testdata/regen-vectors.mjs — builds the two hashes out of
	// the same parts:
	//
	//	yy:          md5(encodeURIComponent(at) + "_" + body + md5(now) + "ooui")
	//	x-signature: md5(seconds + secret + body)
	//
	// The "_" separator, the md5 of the time and the "ooui" salt are
	// the reference's, verbatim. They read like typos and are not.
	var yy, signature string
	if len(o.Body) == 0 {
		// The reference has no body for a GET, and signs a literal
		// "{}" in the body's place. The "{}" is the reference's too,
		// and not a placeholder to be tidied away: dropping it changes
		// every GET signature.
		yy = md5Hex(encodeURIComponent(at) + "_" + "{}" + md5Hex(nowText) + "ooui")
		signature = md5Hex(strconv.FormatInt(ts, 10) + Secret)
	} else {
		body := string(o.Body)
		yy = md5Hex(encodeURIComponent(at) + "_" + body + md5Hex(nowText) + "ooui")
		signature = md5Hex(strconv.FormatInt(ts, 10) + Secret + body)
	}

	return Signed{
		URL:       target,
		YY:        yy,
		Timestamp: strconv.FormatInt(ts, 10),
		Signature: signature,
		access:    o.Access,
	}
}

// resolveURL gives the URL a signed request goes to and the string the
// reference signs.
//
// The reference resolves the path against the origin with
// `new URL(path, origin)` and signs url.pathname, which is the
// resolved and percent-encoded path. So a path without a leading slash
// is resolved against the origin's own path, a space in the path is
// escaped, and a query in the path is dropped — the reference assigns
// the defaults to url.search afterwards, and that replaces it.
//
// The fragment is left out of the signed string, as it is in the
// reference, and kept in the URL, as it is there too.
func resolveURL(origin, path, query string) (target, at string) {
	base, err := url.Parse(origin)
	if err != nil || base.Host == "" {
		// An origin that is not a URL is used as the prefix as it is,
		// and the path is taken as an absolute path.
		escaped := absolutePath(path)
		return strings.TrimRight(origin, "/") + escaped + "?" + query, escaped + "?" + query
	}
	if base.Scheme == "" {
		base.Scheme = "https"
	}
	ref, err := url.Parse(path)
	if err != nil {
		// url.Parse rejects a path with an invalid percent escape
		// ("/v1/100%"), which JavaScript keeps as it is. Such a path
		// is escaped here the way JavaScript escapes a path.
		escaped := absolutePath(path)
		return base.Scheme + "://" + base.Host + escaped + "?" + query, escaped + "?" + query
	}
	resolved := base.ResolveReference(ref)
	// The reference replaces the search with the defaults, so the
	// path's own query is dropped.
	resolved.RawQuery = ""
	resolved.ForceQuery = false
	escaped := resolved.EscapedPath()
	if escaped == "" {
		// A URL always has a path, at least a slash.
		escaped = "/"
	}
	target = resolved.Scheme + "://" + resolved.Host + escaped + "?" + query
	if resolved.Fragment != "" {
		target += "#" + resolved.EscapedFragment()
	}
	return target, escaped + "?" + query
}

// params is an ordered query parameter set: setting a key that is
// already there changes its value and keeps its place.
type params struct {
	keys []string
	vals map[string]string
}

func newParams() *params {
	return &params{vals: map[string]string{}}
}

func (p *params) set(k, v string) {
	if _, ok := p.vals[k]; !ok {
		p.keys = append(p.keys, k)
	}
	p.vals[k] = v
}

func (p *params) get(k string) string { return p.vals[k] }

func (p *params) encode() string {
	var b strings.Builder
	for i, k := range p.keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(formEncode(k))
		b.WriteByte('=')
		b.WriteString(formEncode(p.vals[k]))
	}
	return b.String()
}

// formEncode encodes one value the way URLSearchParams does.
func formEncode(s string) string {
	const kept = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789*-._"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case strings.IndexByte(kept, c) >= 0:
			b.WriteByte(c)
		case c == ' ':
			b.WriteByte('+')
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// encodeURIComponent encodes one value the way JavaScript's
// encodeURIComponent does.
func encodeURIComponent(s string) string {
	const kept = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.!~*'()"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if strings.IndexByte(kept, c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// absolutePath escapes a path the way JavaScript escapes one and makes
// it absolute.
func absolutePath(path string) string {
	escaped := jsPath(path)
	if !strings.HasPrefix(escaped, "/") {
		escaped = "/" + escaped
	}
	return escaped
}

// jsPath escapes a path the way JavaScript's URL parser escapes one:
// the C0 controls, everything above "~" and " # < > ? ` { }" are
// percent-encoded and the rest is left as it is. A "%" that is not a
// valid escape is left alone, as JavaScript leaves it.
func jsPath(s string) string {
	const escaped = "\"#<>?`{}"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c <= 0x20 || c >= 0x7F:
			fmt.Fprintf(&b, "%%%02X", c)
		case strings.IndexByte(escaped, c) >= 0:
			fmt.Fprintf(&b, "%%%02X", c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// jsTrim trims a string the way JavaScript's String.prototype.trim
// does: the WhiteSpace and LineTerminator characters.
//
// It is not quite strings.TrimSpace, which differs on two code points —
// JavaScript strips U+FEFF and keeps U+0085 — and the difference
// matters here, because the user id goes into the signed query.
func jsTrim(s string) string {
	return strings.TrimFunc(s, isJSSpace)
}

// isJSSpace says whether a rune is one of JavaScript's WhiteSpace and
// LineTerminator characters.
func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x00A0, 0xFEFF,
		0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000:
		return true
	}
	return r >= 0x2000 && r <= 0x200A
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}
