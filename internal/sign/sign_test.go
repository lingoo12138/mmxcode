package sign

import (
	"strings"
	"testing"

	"github.com/lingoo12138/mmxcode/internal/site"
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

// The web flavour puts the common parameters first and the caller's own
// parameter after them, the way the browser fills a URLSearchParams.
// A key that is in both halves is in the query twice, and the parsers
// take the first one they see, so the common block's value counts.
func TestTheWebFlavorSignsItsOwnQuery(t *testing.T) {
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
		"?device_platform=web&biz_id=3&app_id=3001&version_code=22201" +
		"&unix=1763000000000&timezone_offset=28800&sys_language=en&lang=en" +
		"&uuid=0f8fad5b-d9cb-469f-a165-70867728950e&device_id=574786429807779842" +
		"&os_name=Windows&browser_name=chrome&device_memory=16&cpu_core_num=16" +
		"&browser_language=en-US&browser_platform=Win32&user_id=0&op_ticket=undefined" +
		"&screen_width=1280&screen_height=800&client=web" +
		"&uuid=0f8fad5b-d9cb-469f-a165-70867728950e"
	if s.URL != want {
		t.Fatalf("url =\n%s\nwant\n%s", s.URL, want)
	}
}

// Every option the web flavour has, set to something other than its
// default, so that a field that is wired up wrong — the screen's width
// and height swapped, say — cannot hide behind a default.
func TestTheWebFlavorSignsEveryOption(t *testing.T) {
	now := int64(1763000000000)
	s := Sign("https://account.minimax.io", "/v1/api/user/device/register", Options{
		Flavor:          FlavorWeb,
		Now:             now,
		TZOffset:        -18000,
		Lang:            "zh",
		OSName:          "macOS",
		BrowserLanguage: "zh-CN",
		BrowserPlatform: "MacIntel",
		ScreenWidth:     2560,
		ScreenHeight:    1440,
		DeviceMemory:    32,
		CPUCores:        10,
		OpTicket:        "ticket-1",
		UUID:            "0f8fad5b-d9cb-469f-a165-70867728950e",
		DeviceID:        "574786429807779842",
		UserID:          "9876543210",
		Query:           []KV{{K: "uuid", V: "0f8fad5b-d9cb-469f-a165-70867728950e"}},
	})
	want := "https://account.minimax.io/v1/api/user/device/register" +
		"?device_platform=web&biz_id=3&app_id=3001&version_code=22201" +
		"&unix=1763000000000&timezone_offset=-18000&sys_language=zh&lang=zh" +
		"&uuid=0f8fad5b-d9cb-469f-a165-70867728950e&device_id=574786429807779842" +
		"&os_name=macOS&browser_name=chrome&device_memory=32&cpu_core_num=10" +
		"&browser_language=zh-CN&browser_platform=MacIntel&user_id=9876543210&op_ticket=ticket-1" +
		"&screen_width=2560&screen_height=1440&client=web" +
		"&uuid=0f8fad5b-d9cb-469f-a165-70867728950e"
	if s.URL != want {
		t.Fatalf("url =\n%s\nwant\n%s", s.URL, want)
	}
}

func TestTheWebFlavorSignsWhatItSends(t *testing.T) {
	// The signature is over what is sent: take the URL and work the two
	// hashes out of it again, and both have to come out the same.
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

// The CLI branch is the contract the six golden vectors depend on, so
// it has to stay exactly as it was: the same output as when the flavor
// is left unset, and none of the web's parameters.
func TestTheMCodeFlavorIsUnchanged(t *testing.T) {
	opts := Options{
		Access: "tok_1", UserID: "1234567890",
		Now: 1763000000000, TZOffset: -480, OSName: "linux",
		Query: []KV{{K: "group_id", V: "g1"}},
	}
	unset := Sign("https://account.minimax.io", "/v1/api/user/identity", opts)
	opts.Flavor = FlavorMCode
	explicit := Sign("https://account.minimax.io", "/v1/api/user/identity", opts)
	if explicit.URL != unset.URL {
		t.Errorf("url =\n%s\nwant\n%s", explicit.URL, unset.URL)
	}
	if explicit.YY != unset.YY {
		t.Errorf("yy = %s, want %s", explicit.YY, unset.YY)
	}
	if explicit.Signature != unset.Signature {
		t.Errorf("signature = %s, want %s", explicit.Signature, unset.Signature)
	}
	// The CLI sends none of the web client's own parameters.
	for _, key := range []string{
		"browser_language=", "browser_platform=", "screen_width=",
		"screen_height=", "device_memory=", "cpu_core_num=", "op_ticket=",
	} {
		if strings.Contains(explicit.URL, key) {
			t.Errorf("the CLI's query carries the web's %s: %s", key, explicit.URL)
		}
	}
}

// The parameter set is ordered: setting a key that is already there
// changes its value and keeps its place, and appending a key puts it
// there a second time, further down.
func TestParams(t *testing.T) {
	cases := []struct {
		name       string
		steps      []func(p *params)
		wantEncode string
		wantGet    map[string]string
	}{
		{
			name: "setting a key twice leaves one entry in its place",
			steps: []func(p *params){
				func(p *params) { p.set("a", "1") },
				func(p *params) { p.set("b", "2") },
				func(p *params) { p.set("a", "3") },
			},
			wantEncode: "a=3&b=2",
			wantGet:    map[string]string{"a": "3", "b": "2"},
		},
		{
			name: "appending a key twice leaves two entries",
			steps: []func(p *params){
				func(p *params) { p.append("a", "1") },
				func(p *params) { p.append("a", "2") },
			},
			wantEncode: "a=1&a=2",
			wantGet:    map[string]string{"a": "2"},
		},
		{
			name: "setting an appended key changes the appended one",
			steps: []func(p *params){
				func(p *params) { p.append("a", "1") },
				func(p *params) { p.append("b", "2") },
				func(p *params) { p.set("a", "3") },
			},
			wantEncode: "a=3&b=2",
			wantGet:    map[string]string{"a": "3"},
		},
		{
			name: "the entries come out in the order they went in",
			steps: []func(p *params){
				func(p *params) { p.set("z", "1") },
				func(p *params) { p.set("a", "2") },
				func(p *params) { p.append("m", "3") },
			},
			wantEncode: "z=1&a=2&m=3",
			wantGet:    map[string]string{"z": "1", "a": "2", "m": "3"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newParams()
			for _, step := range c.steps {
				step(p)
			}
			if got := p.encode(); got != c.wantEncode {
				t.Errorf("encode = %q, want %q", got, c.wantEncode)
			}
			for k, want := range c.wantGet {
				if got := p.get(k); got != want {
					t.Errorf("get(%q) = %q, want %q", k, got, want)
				}
			}
		})
	}
}

// A key that was never set has no value.
func TestParamsGetOfAMissingKey(t *testing.T) {
	p := newParams()
	if got := p.get("nothing"); got != "" {
		t.Errorf("get = %q, want an empty string", got)
	}
}
