package sign

import (
	"strings"
	"testing"

	"mmxcode/internal/site"
)

// The vectors were produced by running the reference implementation
// (magpie-community/plugins, packages/minimax/index.mjs) under Node 24.
// They are the contract: if these pass, our signature is byte-identical
// to the one MiniMax Code sends.
func TestSignGoldenVectors(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		opts    Options
		wantURL string
		wantYY  string
		wantTS  string
		wantSig string
	}{
		{
			name: "identity GET",
			path: "/v1/api/user/info",
			opts: Options{
				Access: "tok_1", UserID: "1234567890",
				Now: 1763150000000, TZOffset: -480, OSName: "linux",
			},
			wantURL: "https://agent.minimax.io/v1/api/user/info?device_platform=mcode&biz_id=3&app_id=3001&version_code=22201&unix=1763150000000&timezone_offset=-480&sys_language=en&lang=en&device_id=0&os_name=linux&browser_name=mcode&user_id=1234567890&client=mcode",
			wantYY:  "40865acf7e929978824a2892b1cc0e7b",
			wantTS:  "1763150000",
			wantSig: "12f7ac9ac565a90bb1d7652ab46ac58c",
		},
		{
			name: "signin status GET",
			path: "/minimax-cloud/api/v1/signin/status",
			opts: Options{
				Access: "tok_2", UserID: "1234567890",
				Now: 1763150001000, TZOffset: -480, OSName: "linux",
				Query: []KV{
					{"device_platform", "web"},
					{"is_desktop", "1"},
					{"desktop_version", "0.6.12"},
				},
			},
			wantURL: "https://agent.minimax.io/minimax-cloud/api/v1/signin/status?device_platform=web&biz_id=3&app_id=3001&version_code=22201&is_desktop=1&desktop_version=0.6.12&unix=1763150001000&timezone_offset=-480&sys_language=en&lang=en&device_id=0&os_name=linux&browser_name=mcode&user_id=1234567890&client=mcode",
			wantYY:  "843dc3e76c9fb3ad20efd5e21ec0d694",
			wantTS:  "1763150001",
			wantSig: "76c77cb5d0d6379b5e3c327e1dcfc792",
		},
		{
			name: "signin claim POST",
			path: "/minimax-cloud/api/v1/signin/claim",
			opts: Options{
				Access: "tok_3", UserID: "1234567890",
				Body: []byte("{}"),
				Now:  1763150002000, TZOffset: -480, OSName: "linux",
				Query: []KV{
					{"device_platform", "web"},
					{"is_desktop", "1"},
					{"desktop_version", "0.6.12"},
				},
			},
			wantURL: "https://agent.minimax.io/minimax-cloud/api/v1/signin/claim?device_platform=web&biz_id=3&app_id=3001&version_code=22201&is_desktop=1&desktop_version=0.6.12&unix=1763150002000&timezone_offset=-480&sys_language=en&lang=en&device_id=0&os_name=linux&browser_name=mcode&user_id=1234567890&client=mcode",
			wantYY:  "6b25db8b3affd0acc6649a069504ae44",
			wantTS:  "1763150002",
			wantSig: "df32c8dde2f6a4ca656915a2b3d6cdb9",
		},
		{
			name: "credit details GET",
			path: "/minimax-cloud/api/v1/credit/details",
			opts: Options{
				Access: "tok_4", UserID: "1234567890",
				Now: 1763150003000, TZOffset: -480, OSName: "linux",
				Query: []KV{
					{"device_platform", "web"},
					{"is_desktop", "1"},
					{"desktop_version", "0.6.12"},
					{"page_number", "1"},
					{"page_size", "20"},
					{"include_expired", "true"},
				},
			},
			wantURL: "https://agent.minimax.io/minimax-cloud/api/v1/credit/details?device_platform=web&biz_id=3&app_id=3001&version_code=22201&is_desktop=1&desktop_version=0.6.12&page_number=1&page_size=20&include_expired=true&unix=1763150003000&timezone_offset=-480&sys_language=en&lang=en&device_id=0&os_name=linux&browser_name=mcode&user_id=1234567890&client=mcode",
			wantYY:  "08b8e40788854e52e454ba9368e06170",
			wantTS:  "1763150003",
			wantSig: "d85b51c8028ad38c89a641b2b7a02a6d",
		},
		{
			name: "membership POST",
			path: "/matrix/api/v1/commerce/get_membership_info",
			opts: Options{
				Access: "tok_5", UserID: "1234567890",
				Body: []byte("{}"),
				Now:  1763150004000, TZOffset: -480, OSName: "linux",
			},
			wantURL: "https://agent.minimax.io/matrix/api/v1/commerce/get_membership_info?device_platform=mcode&biz_id=3&app_id=3001&version_code=22201&unix=1763150004000&timezone_offset=-480&sys_language=en&lang=en&device_id=0&os_name=linux&browser_name=mcode&user_id=1234567890&client=mcode",
			wantYY:  "13ceb091e21212e52bb130743981aa78",
			wantTS:  "1763150004",
			wantSig: "3bb29287aeeb77d318ea9c5f3dc1e0a6",
		},
		{
			name: "workspace POST with empty user id",
			path: "/matrix/api/v1/user/get_user_extra_info",
			opts: Options{
				Access: "tok_6", UserID: "",
				Body: []byte("{}"),
				Now:  1763150005000, TZOffset: 0, OSName: "darwin",
			},
			wantURL: "https://agent.minimax.io/matrix/api/v1/user/get_user_extra_info?device_platform=mcode&biz_id=3&app_id=3001&version_code=22201&unix=1763150005000&timezone_offset=0&sys_language=en&lang=en&device_id=0&os_name=darwin&browser_name=mcode&user_id=0&client=mcode",
			wantYY:  "d654afea9c1c4f667f82ecf2c7e175da",
			wantTS:  "1763150005",
			wantSig: "b8aec2aec964042d1b477b48ecbf1324",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Sign("https://agent.minimax.io", c.path, c.opts)
			if got.URL != c.wantURL {
				t.Errorf("URL\n got %s\nwant %s", got.URL, c.wantURL)
			}
			if got.YY != c.wantYY {
				t.Errorf("yy = %s, want %s", got.YY, c.wantYY)
			}
			if got.Timestamp != c.wantTS {
				t.Errorf("timestamp = %s, want %s", got.Timestamp, c.wantTS)
			}
			if got.Signature != c.wantSig {
				t.Errorf("signature = %s, want %s", got.Signature, c.wantSig)
			}

			headers := got.Headers()
			want := map[string]string{
				"Authorization": "Bearer " + c.opts.Access,
				"Accept":        "application/json",
				"Content-Type":  "application/json",
				"User-Agent":    site.AccountUserAgent,
				"yy":            c.wantYY,
				"x-timestamp":   c.wantTS,
				"x-signature":   c.wantSig,
			}
			for name, value := range want {
				if headers[name] != value {
					t.Errorf("header %s = %q, want %q", name, headers[name], value)
				}
			}
		})
	}
}

// The query the reference builds for a plain request, so that the cases
// below can spell out only what they change.
const plainQuery = "device_platform=mcode&biz_id=3&app_id=3001&version_code=22201" +
	"&unix=1763150000000&timezone_offset=-480&sys_language=en&lang=en" +
	"&device_id=0&os_name=linux&browser_name=mcode&user_id=1234567890&client=mcode"

// The reference resolves the path against the origin and signs the
// resolved, percent-encoded path, so these all sign something other
// than what the path looks like.
func TestSignResolvesThePath(t *testing.T) {
	cases := []struct {
		name    string
		origin  string
		path    string
		wantURL string
		wantYY  string
	}{
		{
			name:   "a path without a leading slash",
			origin: "https://agent.minimax.io",
			path:   "v1/api/user/info",
			// The reference resolves it against the origin, so it is
			// the same request as the golden identity GET.
			wantURL: "https://agent.minimax.io/v1/api/user/info?" + plainQuery,
			wantYY:  "40865acf7e929978824a2892b1cc0e7b",
		},
		{
			name:    "a path with a space in it",
			origin:  "https://agent.minimax.io",
			path:    "/v1/a b",
			wantURL: "https://agent.minimax.io/v1/a%20b?" + plainQuery,
			wantYY:  "5bf73d0b32d9919785be6e1cf3b76a84",
		},
		{
			name:   "a path with a query of its own",
			origin: "https://agent.minimax.io",
			path:   "/v1/x?a=1",
			// The reference assigns the defaults to url.search, which
			// replaces the path's own query.
			wantURL: "https://agent.minimax.io/v1/x?" + plainQuery,
			wantYY:  "390ae9a1f47d7d6bb3e1146f2ae666e5",
		},
		{
			name:   "an origin with a path of its own",
			origin: "https://agent.minimax.io/base/",
			path:   "/v1/x",
			// An absolute path replaces the origin's path.
			wantURL: "https://agent.minimax.io/v1/x?" + plainQuery,
			wantYY:  "390ae9a1f47d7d6bb3e1146f2ae666e5",
		},
		{
			name:   "a path with a percent that is not an escape",
			origin: "https://agent.minimax.io",
			path:   "/v1/100%",
			// JavaScript keeps a "%" that is not an escape as it is,
			// so the reference signs it as it is too.
			wantURL: "https://agent.minimax.io/v1/100%?" + plainQuery,
			wantYY:  "203556ac721d12836c9fa21a52201df3",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Sign(c.origin, c.path, Options{
				Access: "tok_1", UserID: "1234567890",
				Now: 1763150000000, TZOffset: -480, OSName: "linux",
			})
			if got.URL != c.wantURL {
				t.Errorf("URL\n got %s\nwant %s", got.URL, c.wantURL)
			}
			if got.YY != c.wantYY {
				t.Errorf("yy = %s, want %s", got.YY, c.wantYY)
			}
		})
	}
}

// The reference builds its query as an object literal with the
// caller's query spread into the middle of it, so a repeated key takes
// the caller's place but only the leading defaults take the caller's
// value.
func TestQueryDefaults(t *testing.T) {
	cases := []struct {
		name    string
		query   []KV
		wantURL string
		wantYY  string
	}{
		{
			name:  "a leading default is the caller's to override",
			query: []KV{{"device_platform", "web"}},
			wantURL: "https://agent.minimax.io/p?device_platform=web&biz_id=3&app_id=3001&version_code=22201" +
				"&unix=1763150000000&timezone_offset=-480&sys_language=en&lang=en" +
				"&device_id=0&os_name=linux&browser_name=mcode&user_id=1234567890&client=mcode",
			wantYY: "98e6fa51a036f804c2c65e80295a731b",
		},
		{
			name:  "lang keeps the reference's value",
			query: []KV{{"lang", "zh"}},
			wantURL: "https://agent.minimax.io/p?device_platform=mcode&biz_id=3&app_id=3001&version_code=22201" +
				"&lang=en&unix=1763150000000&timezone_offset=-480&sys_language=en" +
				"&device_id=0&os_name=linux&browser_name=mcode&user_id=1234567890&client=mcode",
			wantYY: "d2e7a1e8b377f6fa116a09b1d9825613",
		},
		{
			name:  "client keeps the reference's value",
			query: []KV{{"client", "x"}},
			wantURL: "https://agent.minimax.io/p?device_platform=mcode&biz_id=3&app_id=3001&version_code=22201" +
				"&client=mcode&unix=1763150000000&timezone_offset=-480&sys_language=en&lang=en" +
				"&device_id=0&os_name=linux&browser_name=mcode&user_id=1234567890",
			wantYY: "f73c2ba5320fdc2145e65d82d40b14b8",
		},
		{
			name:  "user_id keeps the option's value",
			query: []KV{{"user_id", "q"}},
			wantURL: "https://agent.minimax.io/p?device_platform=mcode&biz_id=3&app_id=3001&version_code=22201" +
				"&user_id=1234567890&unix=1763150000000&timezone_offset=-480&sys_language=en&lang=en" +
				"&device_id=0&os_name=linux&browser_name=mcode&client=mcode",
			wantYY: "84f842303887f8956faa37fad5226a13",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Sign("https://agent.minimax.io", "/p", Options{
				Access: "tok_1", UserID: "1234567890",
				Now: 1763150000000, TZOffset: -480, OSName: "linux",
				Query: c.query,
			})
			if got.URL != c.wantURL {
				t.Errorf("URL\n got %s\nwant %s", got.URL, c.wantURL)
			}
			if got.YY != c.wantYY {
				t.Errorf("yy = %s, want %s", got.YY, c.wantYY)
			}
		})
	}
}

func TestQueryOverrideKeepsPosition(t *testing.T) {
	// The reference spreads the caller's query over the defaults, so a
	// repeated key changes its value but keeps its place.
	got := Sign("https://agent.minimax.io", "/p", Options{
		Now: 1763150000000, TZOffset: -480, OSName: "linux",
		Query: []KV{{"device_platform", "web"}, {"is_desktop", "1"}},
	})
	want := "device_platform=web&biz_id=3&app_id=3001&version_code=22201&is_desktop=1"
	if !strings.Contains(got.URL, want) {
		t.Fatalf("URL %s does not contain %s", got.URL, want)
	}
}

// The options that fall back to a default, and the ones that are
// trimmed, are all part of the signed query.
func TestSignOptions(t *testing.T) {
	cases := []struct {
		name    string
		opts    Options
		wantURL string
		wantYY  string
	}{
		{
			name: "a language of its own",
			opts: Options{
				Access: "tok_1", UserID: "1234567890",
				Now: 1763150000000, TZOffset: -480, OSName: "linux",
				Lang: "zh",
			},
			wantURL: "https://agent.minimax.io/p?device_platform=mcode&biz_id=3&app_id=3001&version_code=22201" +
				"&unix=1763150000000&timezone_offset=-480&sys_language=zh&lang=zh" +
				"&device_id=0&os_name=linux&browser_name=mcode&user_id=1234567890&client=mcode",
			wantYY: "d6bbe2721fce479b89ca1b7c05de3591",
		},
		{
			name: "no platform name falls back to linux",
			opts: Options{
				Access: "tok_1", UserID: "1234567890",
				Now: 1763150000000, TZOffset: -480,
			},
			wantURL: "https://agent.minimax.io/p?" + plainQuery,
			wantYY:  "cf6860c1fac0feba1367dc97c06cd876",
		},
		{
			name: "a user id with whitespace around it",
			opts: Options{
				Access: "tok_1", UserID: "  1234567890  ",
				Now: 1763150000000, TZOffset: -480, OSName: "linux",
			},
			wantURL: "https://agent.minimax.io/p?" + plainQuery,
			wantYY:  "cf6860c1fac0feba1367dc97c06cd876",
		},
		{
			name: "a query value with a space and a non-ASCII character",
			opts: Options{
				Access: "tok_1", UserID: "1234567890",
				Now: 1763150000000, TZOffset: -480, OSName: "linux",
				Query: []KV{{"q", "a b 中文"}},
			},
			// URLSearchParams encodes a space as "+" and everything
			// else as UTF-8 percent escapes.
			wantURL: "https://agent.minimax.io/p?device_platform=mcode&biz_id=3&app_id=3001&version_code=22201" +
				"&q=a+b+%E4%B8%AD%E6%96%87&unix=1763150000000&timezone_offset=-480&sys_language=en&lang=en" +
				"&device_id=0&os_name=linux&browser_name=mcode&user_id=1234567890&client=mcode",
			wantYY: "79bfb3e01c70c286bc75523a8c43a2b2",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Sign("https://agent.minimax.io", "/p", c.opts)
			if got.URL != c.wantURL {
				t.Errorf("URL\n got %s\nwant %s", got.URL, c.wantURL)
			}
			if got.YY != c.wantYY {
				t.Errorf("yy = %s, want %s", got.YY, c.wantYY)
			}
		})
	}
}

// An empty body is a GET: the reference has no way to send one, and a
// POST with an empty body would sign a body the reference never signs.
func TestSignEmptyBodyIsGet(t *testing.T) {
	opts := Options{
		Access: "tok_1", UserID: "1234567890",
		Now: 1763150000000, TZOffset: -480, OSName: "linux",
	}
	get := Sign("https://agent.minimax.io", "/p", opts)
	empty := Sign("https://agent.minimax.io", "/p", Options{
		Access: opts.Access, UserID: opts.UserID, Body: []byte{},
		Now: opts.Now, TZOffset: opts.TZOffset, OSName: opts.OSName,
	})
	if empty.YY != get.YY {
		t.Errorf("yy = %s, want the GET one %s", empty.YY, get.YY)
	}
	if empty.Signature != get.Signature {
		t.Errorf("signature = %s, want the GET one %s", empty.Signature, get.Signature)
	}
	if want := "cf6860c1fac0feba1367dc97c06cd876"; empty.YY != want {
		t.Errorf("yy = %s, want %s", empty.YY, want)
	}
	if want := "12f7ac9ac565a90bb1d7652ab46ac58c"; empty.Signature != want {
		t.Errorf("signature = %s, want %s", empty.Signature, want)
	}
}

// The reference floors the seconds and Go's division truncates towards
// zero.
func TestSignFloorsNegativeSeconds(t *testing.T) {
	got := Sign("https://agent.minimax.io", "/p", Options{
		Access: "tok_1", UserID: "1234567890",
		Now: -1500, TZOffset: -480, OSName: "linux",
	})
	if got.Timestamp != "-2" {
		t.Errorf("timestamp = %s, want -2", got.Timestamp)
	}
	if want := "f13f639021bd969f8a1dad1c171b5785"; got.YY != want {
		t.Errorf("yy = %s, want %s", got.YY, want)
	}
	if want := "c70bef52e842c40feaa0a33d76040802"; got.Signature != want {
		t.Errorf("signature = %s, want %s", got.Signature, want)
	}
}

// The user id is trimmed the way JavaScript trims a string, which is
// not quite strings.TrimSpace: JavaScript strips U+FEFF and keeps
// U+0085, and Go does the opposite.
func TestJSTrimMatchesJavaScript(t *testing.T) {
	bom := string(rune(0xFEFF))
	nel := string(rune(0x0085))
	cases := map[string]string{
		"  42  ":     "42",
		"\t\n42\r\n": "42",
		bom + "42":   "42",
		"42" + nel:   "42" + nel,
	}
	for in, want := range cases {
		if got := jsTrim(in); got != want {
			t.Errorf("jsTrim(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEncodeURIComponentMatchesJS(t *testing.T) {
	cases := map[string]string{
		"/v1/api/user/info?a=b&c=d": "%2Fv1%2Fapi%2Fuser%2Finfo%3Fa%3Db%26c%3Dd",
		"a b":                       "a%20b",
		"~":                         "~",
		"*":                         "*",
		// encodeURIComponent keeps !'()* — unlike URLSearchParams, which
		// escapes them. The two encoders differ exactly here.
		"!'()": "!'()",
	}
	for in, want := range cases {
		if got := encodeURIComponent(in); got != want {
			t.Errorf("encodeURIComponent(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFormEncodeMatchesJS(t *testing.T) {
	cases := map[string]string{
		"a b": "a+b",
		"~":   "%7E",
		"*":   "*",
		"-._": "-._",
		"/":   "%2F",
		// URLSearchParams escapes these; encodeURIComponent does not.
		"!'()": "%21%27%28%29",
	}
	for in, want := range cases {
		if got := formEncode(in); got != want {
			t.Errorf("formEncode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTheWebFlavourSignsItsOwnQuery(t *testing.T) {
	// 网页把公共 query 放在调用方的 query 后面（JS 的 {...params, ...common}
	// 是这样展开的），而 CLI 是放在中间。两边的键序不一样，签名就不一样。
	now := int64(1763000000000)
	s := Sign("https://account.minimax.io", "/oauth2/login", Options{
		Flavor:   FlavorWeb,
		Now:      now,
		TZOffset: 28800,
		Query:    []KV{{K: "uuid", V: "0f8fad5b-d9cb-469f-a165-70867728950e"}},
		UUID:     "0f8fad5b-d9cb-469f-a165-70867728950e",
		DeviceID: "574786429807779842",
	})
	want := "https://account.minimax.io/oauth2/login" +
		"?uuid=0f8fad5b-d9cb-469f-a165-70867728950e" +
		"&device_platform=web&biz_id=3&app_id=3001&version_code=22201" +
		"&unix=1763000000000&timezone_offset=28800&sys_language=en&lang=en" +
		"&uuid=0f8fad5b-d9cb-469f-a165-70867728950e&device_id=574786429807779842" +
		"&os_name=Windows&browser_name=chrome&device_memory=16&cpu_core_num=16" +
		"&browser_language=en-US&browser_platform=Win32&user_id=0&op_ticket=undefined" +
		"&screen_width=1280&screen_height=800&client=web"
	if s.URL != want {
		t.Fatalf("url =\n%s\nwant\n%s", s.URL, want)
	}
}

func TestTheWebFlavourSignsWhatItSends(t *testing.T) {
	// 签的就是发的：把 URL 拿来重算一遍 yy 和 x-signature，两个都要对得上。
	now := int64(1763000000000)
	body := []byte(`{"loginType":"20","email":"a@example.com"}`)
	s := Sign("https://account.minimax.io", "/oauth2/login", Options{
		Flavor:   FlavorWeb,
		Now:      now,
		TZOffset: 28800,
		Body:     body,
	})
	// yy = md5(encodeURIComponent(path?query) + "_" + body + md5(unix_ms) + "ooui")
	at := s.URL[len("https://account.minimax.io"):]
	yy := md5Hex(encodeURIComponent(at) + "_" + string(body) + md5Hex("1763000000000") + "ooui")
	if s.YY != yy {
		t.Errorf("yy = %q, want %q", s.YY, yy)
	}
	// x-signature = md5(x-timestamp + secret + body), and the timestamp
	// is in seconds — the same one the header carries.
	sig := md5Hex("1763000000" + Secret + string(body))
	if s.Signature != sig {
		t.Errorf("signature = %q, want %q", s.Signature, sig)
	}
}

func TestTheMCodeFlavourIsUnchanged(t *testing.T) {
	// CLI 那套不能动：调用方的 query 在中间，device_platform 是 mcode。
	now := int64(1763000000000)
	s := Sign("https://account.minimax.io", "/v1/api/user/identity", Options{
		Now:      now,
		TZOffset: 0,
		Query:    []KV{{K: "group_id", V: "g1"}},
	})
	if !strings.Contains(s.URL, "device_platform=mcode") ||
		!strings.Contains(s.URL, "browser_name=mcode") ||
		!strings.Contains(s.URL, "client=mcode") {
		t.Fatalf("url = %s", s.URL)
	}
	if strings.Contains(s.URL, "device_platform=web") {
		t.Fatalf("url = %s", s.URL)
	}
}
