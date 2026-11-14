# 批量导入（邮箱|密码 → mcode 账号）实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 mmxcode 能读一份 `邮箱|密码` 的清单，自动走完「网页密码登录 → 批准设备码 → 换出 mcode 凭据」这条链，把账号灌进库；密码加密入库，账号掉线后由调度器自动重登。

**Architecture:** 新增三个包：`internal/webauth`（网页登录那一层：设备注册、邮箱探测、密码登录、批准设备码）、`internal/secret`（密码的加密）、`internal/provision`（批量任务：读文件、逐个账号跑链、分类失败、留痕）。`internal/sign` 加一个「网页味」的 flavor——签名算法一样，公共 query 不一样。CLI 和 WebUI 是同一个批量任务的两个前端。

**Tech Stack:** Go 1.26，`crypto/rsa` + `crypto/x509`（RSA-1024/PKCS#1 v1.5，与网页的 JSEncrypt 一致），`crypto/aes` + `crypto/cipher`（AES-256-GCM），`net/http/cookiejar`，`modernc.org/sqlite`，`cobra`。

**设计文档：** `docs/superpowers/specs/2026-11-14-mmxcode-batch-import-design.md`

---

## 动手之前要知道的几件事

1. **测试绝不打真站点。** `api.minimax.io` / `account.minimax.io` 都不许出现在测试里——那会真的用掉一个 refresh token，或者真的登一次号。网页登录这一层用 `httptest` 起一台假的服务端，按真站点的报文形状回话。
2. **参考实现是契约。** 上游行为以 `@magpie-community/opencode-minimax-auth` 的 `packages/minimax/index.mjs` 为准；网页登录那几步以 MiniMax 网页版前端（`account.minimax.io` 的 `prod-web-va-0.1.45` bundle）和用户自己的抓包笔记为准。spec §15 列了每个结论的出处。
3. **每个任务都以「写失败的测试 → 看它失败 → 写实现 → 看它过 → 提交」走。** 提交信息用中文，说清楚做了什么和为什么。
4. **计划里的整文件代码块跟仓库里的文件逐字节相同。** 改完代码要用 `.tmp-review/sync_plan.py` 把文件同步回计划（本计划在 `docs/superpowers/plans/2026-11-14-mmxcode-batch-import.md`）。
5. **所有测试都不能碰真站点。** 写测试时确认一下 `httptest.NewServer` 用的是本地地址。

---

### Task 1: 签名的「网页味」

**Files:**
- Modify: `internal/sign/sign.go`
- Modify: `internal/sign/sign_test.go`

- [ ] **Step 1: 写失败的测试**

在 `internal/sign/sign_test.go` 末尾加：

整个文件写成这样（下面是它的全文）：

```go
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
```

- [ ] **Step 2: 跑测试，确认它失败**

Run: `go test ./internal/sign/ -count=1 -run "WebFlavour|MCodeFlavour"`
Expected: FAIL（`FlavorWeb` 还没有，编译就过不去）

- [ ] **Step 3: 写实现**

在 `internal/sign/sign.go` 里加：

整个文件写成这样（下面是它的全文）：

```go
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

// Flavor is which client a signed request claims to be.
//
// The two clients sign the same way but send different things: the
// CLI claims to be MiniMax Code and the web claims to be a browser.
// The signature covers the query, so the query has to be the one that
// client really sends.
type Flavor int

const (
	// FlavorMCode is MiniMax Code, the CLI. It is the default.
	FlavorMCode Flavor = iota
	// FlavorWeb is the MiniMax account website.
	FlavorWeb
)

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
	// The CLI builds its query as an object literal with the caller's
	// query spread into the middle of it, and the two halves do not
	// treat a repeated key the same way:
	//
	//   - the four leading defaults — device_platform, biz_id, app_id
	//     and version_code — come before the spread, so a repeated key
	//     takes the caller's value and keeps the caller's place;
	//   - the trailing defaults — unix, timezone_offset, sys_language,
	//     lang, device_id, os_name, browser_name, user_id and client —
	//     come after it, so a repeated key keeps the caller's place but
	//     takes the reference's value. "lang" and "client" are the
	//     reference's, "user_id" is the option's.
	//
	// The web client builds its query the other way round. It fills a
	// URLSearchParams: the common parameters go in first and the
	// caller's own parameter is appended after them. A key that is in
	// both halves is therefore in the query twice, and the parsers the
	// site uses — url.Values.Get, URLSearchParams.get, Java's
	// getParameter — take the first one they see, so for the web
	// flavour it is the common block's value that counts.
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
	// OSName is the client's platform name. An empty one is "linux"
	// for the CLI and "Windows" for the web client.
	OSName string
	// Lang is the site's language. An empty one is the site's own.
	Lang string
	// Flavor says which client the request claims to be. The zero
	// value is the CLI, so the existing callers do not change.
	Flavor Flavor
	// UUID is the web client's long-lived device id. The browser
	// keeps one in localStorage and sends it with every request.
	UUID string
	// DeviceID is the number the device registration handed out.
	DeviceID string
	// ScreenWidth and ScreenHeight are the screen the web client
	// claims to have.
	ScreenWidth  int
	ScreenHeight int
	// DeviceMemory and CPUCores are the machine the web client claims.
	DeviceMemory int
	CPUCores     int
	// BrowserLanguage and BrowserPlatform are what the web client
	// claims about itself.
	BrowserLanguage string
	BrowserPlatform string
	// OpTicket is the web session's ticket. Before signing in the
	// browser sends the word "undefined", and so does this.
	OpTicket string
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

	p := newParams()
	switch o.Flavor {
	case FlavorWeb:
		// The web client fills a URLSearchParams: the common parameters
		// go in first and the caller's own parameter is appended after
		// them. A key that is in both halves is in the query twice, and
		// the usual parsers — url.Values.Get, URLSearchParams.get,
		// Java's getParameter — take the first one they see, so it is
		// the common block's value that counts here.
		//
		// The rest are what the web client claims about itself. The CLI
		// sends none of them.
		osName := o.OSName
		if osName == "" {
			osName = "Windows"
		}
		userID := jsTrim(o.UserID)
		if userID == "" {
			userID = "0"
		}
		// The browser language is the one the client claims to have:
		// the site says English and the browser behind it says English
		// too.
		browserLanguage := o.BrowserLanguage
		if browserLanguage == "" {
			browserLanguage = "en-US"
		}
		browserPlatform := o.BrowserPlatform
		if browserPlatform == "" {
			browserPlatform = "Win32"
		}
		deviceMemory := o.DeviceMemory
		if deviceMemory == 0 {
			deviceMemory = 16
		}
		cpuCores := o.CPUCores
		if cpuCores == 0 {
			cpuCores = 16
		}
		screenWidth := o.ScreenWidth
		if screenWidth == 0 {
			screenWidth = 1280
		}
		screenHeight := o.ScreenHeight
		if screenHeight == 0 {
			screenHeight = 800
		}
		opTicket := o.OpTicket
		if opTicket == "" {
			// The browser sends the word "undefined" before signing in:
			// the ticket is not in the session yet.
			opTicket = "undefined"
		}
		p.append("device_platform", "web")
		p.append("biz_id", "3")
		p.append("app_id", "3001")
		p.append("version_code", "22201")
		p.append("unix", strconv.FormatInt(o.Now, 10))
		p.append("timezone_offset", strconv.Itoa(o.TZOffset))
		p.append("sys_language", lang)
		p.append("lang", lang)
		p.append("uuid", o.UUID)
		p.append("device_id", o.DeviceID)
		p.append("os_name", osName)
		p.append("browser_name", "chrome")
		p.append("device_memory", strconv.Itoa(deviceMemory))
		p.append("cpu_core_num", strconv.Itoa(cpuCores))
		p.append("browser_language", browserLanguage)
		p.append("browser_platform", browserPlatform)
		p.append("user_id", userID)
		p.append("op_ticket", opTicket)
		p.append("screen_width", strconv.Itoa(screenWidth))
		p.append("screen_height", strconv.Itoa(screenHeight))
		p.append("client", "web")
		for _, kv := range o.Query {
			p.append(kv.K, kv.V)
		}
	default:
		// The CLI puts the caller's parameters in the middle and the
		// defaults around them. That is what the reference does, and
		// the order is part of the signature.
		osName := o.OSName
		if osName == "" {
			osName = "linux"
		}
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
	}

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
// already there changes its value and keeps its place, and appending a
// key puts it there a second time, further down.
type params struct {
	entries []KV
	// index is where a key's value is set, so that setting a key again
	// changes its value and keeps its place.
	index map[string]int
}

func newParams() *params {
	return &params{index: map[string]int{}}
}

func (p *params) set(k, v string) {
	if i, ok := p.index[k]; ok {
		p.entries[i].V = v
		return
	}
	p.index[k] = len(p.entries)
	p.entries = append(p.entries, KV{K: k, V: v})
}

// append puts the key at the end, even when it is already there.
//
// The web client builds its query by filling a URLSearchParams: the
// common parameters go in first and the caller's own parameter is
// appended after them, so a key that is in both halves is in the query
// twice.
//
// The key is registered in the index as well, so that get sees the most
// recent value and set changes the appended entry in place instead of
// appending a third copy of it.
func (p *params) append(k, v string) {
	p.entries = append(p.entries, KV{K: k, V: v})
	p.index[k] = len(p.entries) - 1
}

func (p *params) get(k string) string {
	if i, ok := p.index[k]; ok {
		return p.entries[i].V
	}
	return ""
}

func (p *params) encode() string {
	var b strings.Builder
	for i, e := range p.entries {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(formEncode(e.K))
		b.WriteByte('=')
		b.WriteString(formEncode(e.V))
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
```

- [ ] **Step 4: 跑测试，确认它过了**

Run: `go test ./internal/sign/ -count=1`
Expected: PASS（全部，包括原有的向量测试）

- [ ] **Step 5: 跑整个测试套件**

Run: `go build ./... && go vet ./... && gofmt -l . && go test ./... -count=1`
Expected: 全绿，`gofmt -l` 没有输出

- [ ] **Step 6: 提交**

```bash
git add internal/sign
git commit -m "feat(sign): 签名加网页那一味

签名算法两边一样，公共 query 不一样：网页把调用方的 query 放最前，
device_platform 是 web，浏览器名、设备号、屏幕这些都在；CLI 那套
不动。

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 2: 密码的加密

**Files:**
- Create: `internal/secret/secret.go`
- Create: `internal/secret/secret_test.go`

- [ ] **Step 1: 写失败的测试**

创建 `internal/secret/secret_test.go`：

```go
package secret

import (
	"encoding/base64"
	"strings"
	"testing"
)

func testKey() Key {
	return Key("0123456789abcdef0123456789abcdef")
}

func TestSealAndOpen(t *testing.T) {
	k := testKey()
	cipher, err := k.Seal("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cipher, "hunter2") {
		t.Fatalf("the password is in the cipher text: %s", cipher)
	}
	plain, err := k.Open(cipher)
	if err != nil {
		t.Fatal(err)
	}
	if plain != "hunter2" {
		t.Fatalf("plain = %q", plain)
	}
}

func TestTwoCiphersOfTheSamePasswordAreDifferent(t *testing.T) {
	k := testKey()
	a, _ := k.Seal("hunter2")
	b, _ := k.Seal("hunter2")
	if a == b {
		t.Fatal("the same password gave the same cipher text")
	}
}

func TestTheWrongKeyDoesNotOpenIt(t *testing.T) {
	cipher, err := testKey().Seal("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Key("fedcba9876543210fedcba9876543210").Open(cipher); err == nil {
		t.Fatal("the wrong key opened it")
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv("MMXCODE_TEST_KEY", base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	k, err := FromEnv("MMXCODE_TEST_KEY")
	if err != nil {
		t.Fatal(err)
	}
	if len(k) != 32 {
		t.Fatalf("key is %d bytes, want 32", len(k))
	}
}

func TestFromEnvWithoutTheVariable(t *testing.T) {
	t.Setenv("MMXCODE_TEST_KEY", "")
	if _, err := FromEnv("MMXCODE_TEST_KEY"); err == nil {
		t.Fatal("no error without the variable")
	}
}

func TestFromEnvWithAShortKey(t *testing.T) {
	t.Setenv("MMXCODE_TEST_KEY", base64.StdEncoding.EncodeToString([]byte("short")))
	if _, err := FromEnv("MMXCODE_TEST_KEY"); err == nil {
		t.Fatal("no error with a short key")
	}
}
```

- [ ] **Step 2: 跑测试，确认它失败**

Run: `go test ./internal/secret/ -count=1`
Expected: FAIL（包还不存在）

- [ ] **Step 3: 写实现**

创建 `internal/secret/secret.go`：

```go
// Package secret encrypts the passwords a batch import brings in.
//
// The passwords are kept in the database so that an account whose
// sign-in chain is broken can be signed in again without a human
// bringing the list back. The key is not in the database and not in
// the configuration file: it comes from the environment, so that a
// backup of the database does not carry the key with it.
//
// The cipher is AES-256-GCM. The text that is stored is
// base64(nonce ‖ ciphertext ‖ tag): the nonce is twelve random
// bytes, and the tag is GCM's own.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

// KeySize is how long a key is.
const KeySize = 32

// Key is a password-encryption key.
type Key []byte

// FromEnv reads the key from an environment variable.
//
// The variable holds the key in base64. A variable that is not there,
// or a key that is not KeySize bytes, is an error: a key that is the
// wrong size would encrypt things that can never be opened again.
func FromEnv(name string) (Key, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return nil, fmt.Errorf("%s is not set", name)
	}
	raw = strings.TrimSpace(raw)
	b, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("%s is not base64: %w", name, err)
	}
	if len(b) != KeySize {
		return nil, fmt.Errorf("%s is %d bytes, want %d", name, len(b), KeySize)
	}
	return Key(b), nil
}

// Seal encrypts a password and gives back the text to store.
func (k Key) Seal(plain string) (string, error) {
	if len(k) != KeySize {
		return "", fmt.Errorf("the key is %d bytes, want %d", len(k), KeySize)
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plain), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Open decrypts a password that Seal made.
func (k Key) Open(text string) (string, error) {
	if len(k) != KeySize {
		return "", fmt.Errorf("the key is %d bytes, want %d", len(k), KeySize)
	}
	raw, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		return "", errors.New("the cipher text is not base64")
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("the cipher text is too short")
	}
	nonce, body := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, body, nil)
	if err != nil {
		return "", errors.New("the cipher text does not open with this key")
	}
	return string(plain), nil
}
```

（`strings` 已经在 import 里了。）

- [ ] **Step 4: 跑测试，确认它过了**

Run: `go test ./internal/secret/ -count=1`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/secret
git commit -m "feat(secret): 密码的加密

AES-256-GCM，密钥只从环境变量来（base64 的 32 字节）——配置文件和
数据库在同一个卷里，密钥跟密文放一处等于没加密。

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 3: 存储层：密码那一列与导入留痕

**Files:**
- Modify: `internal/store/schema.go`
- Modify: `internal/store/store.go`
- Modify: `internal/store/store_test.go`

- [ ] **Step 1: 写失败的测试**

在 `internal/store/store_test.go` 末尾加：

```go
func TestThePasswordGoesInAndComesBack(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	if err := s.CreateAccount(ctx, Account{ID: "acc_1", Name: "a", Status: StatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPassword(ctx, "acc_1", "Y2lwaGVy", now); err != nil {
		t.Fatal(err)
	}
	got, err := s.Password(ctx, "acc_1")
	if err != nil {
		t.Fatal(err)
	}
	if got != "Y2lwaGVy" {
		t.Fatalf("password = %q", got)
	}
}

func TestAnAccountWithoutAPassword(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	if err := s.CreateAccount(ctx, Account{ID: "acc_1", Name: "a", Status: StatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Password(ctx, "acc_1")
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("password = %q, want none", got)
	}
}

func TestTheImportRunIsKept(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	if err := s.CreateImportRun(ctx, ImportRun{ID: "run_1", Source: "cli", StartedAt: now, Total: 2}); err != nil {
		t.Fatal(err)
	}
	err := s.FinishImportRun(ctx, "run_1", now, 1, 1, `[{"email":"a@example.com","ok":true}]`)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := s.ListImportRuns(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("got %d runs", len(runs))
	}
	if runs[0].OK != 1 || runs[0].Failed != 1 || runs[0].Results == "" {
		t.Fatalf("run = %+v", runs[0])
	}
}

func TestAnImportRunCanBeAskedForByItsName(t *testing.T) {
	// 页面上要看得见某一次导入走到哪儿了：光有"最近的几次"不够，
	// 得能按名字把这一次导入单独取出来。
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	if err := s.CreateImportRun(ctx, ImportRun{ID: "run_1", Source: "web", StartedAt: now, Total: 2}); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishImportRun(ctx, "run_1", now, 1, 1, "[]"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetImportRun(ctx, "run_1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "run_1" || got.Source != "web" || got.OK != 1 || got.Failed != 1 {
		t.Fatalf("run = %+v", got)
	}
}
```

（`newTestStore` 是这个包里现成的helper；名字不一样的话看 `store_test.go` 里已有的建库函数叫什么，用那个。）

- [ ] **Step 2: 跑测试，确认它失败**

Run: `go test ./internal/store/ -count=1 -run "Password|ImportRun"`
Expected: FAIL（`SetPassword` / `ImportRun` 还没有）

- [ ] **Step 3: 写实现**

在 `internal/store/schema.go` 的 `accounts` 表里加一列：

```
  password    TEXT,
```

并在 `copy` 里加：

```go
			nullable("password"),
```

（`nullable` 已经在那个文件里有了。）

在 `tables` 里加一张新表（放在 `accounts` 后面，因为别的表都指着它，这张表不指谁，放哪儿都行）：

```go
	{
		name: "import_runs",
		body: `(
  id          TEXT PRIMARY KEY,
  source      TEXT NOT NULL,
  started_at  INTEGER NOT NULL,
  finished_at INTEGER,
  total       INTEGER NOT NULL DEFAULT 0,
  ok          INTEGER NOT NULL DEFAULT 0,
  failed      INTEGER NOT NULL DEFAULT 0,
  results     TEXT
)`,
		copy: []copyColumn{
			notNull("id", "''"),
			notNull("source", "''"),
			notNull("started_at", "0"),
			nullable("finished_at"),
			notNull("total", "0"),
			notNull("ok", "0"),
			notNull("failed", "0"),
			nullable("results"),
		},
	},
```

把 `internal/store/store.go` 里的 `schemaVersion` 从 3 改成 4。

在 `internal/store/store.go` 里加类型与方法：

```go
// ImportRun is one batch import and what came of it.
type ImportRun struct {
	ID         string
	Source     string
	StartedAt  time.Time
	FinishedAt time.Time
	Total      int
	OK         int
	Failed     int
	Results    string
}

// SetPassword writes an account's encrypted password.
//
// The cipher text is written on its own, so that the other setters —
// which write one column each — do not have to know about it.
func (s *Store) SetPassword(ctx context.Context, id, cipher string, updatedAt time.Time) error {
	return s.exec(ctx, `UPDATE accounts SET password = ?, updated_at = ? WHERE id = ?`, cipher, s.stamp(updatedAt).UnixMilli(), id)
}

// Password gives an account's encrypted password.
//
// An account that was not brought in by a batch import has none, and
// that is not an error: the text is empty.
func (s *Store) Password(ctx context.Context, id string) (string, error) {
	var cipher string
	err := s.db.QueryRowContext(ctx, `SELECT password FROM accounts WHERE id = ?`, id).Scan(&cipher)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return cipher, nil
}

// CreateImportRun records the start of a batch import.
func (s *Store) CreateImportRun(ctx context.Context, r ImportRun) error {
	return s.exec(ctx, `INSERT INTO import_runs (id, source, started_at, total, ok, failed) VALUES (?, ?, ?, ?, ?, ?)`,
		r.ID, r.Source, s.stamp(r.StartedAt).UnixMilli(), r.Total, r.OK, r.Failed)
}

// FinishImportRun records what a batch import came to.
func (s *Store) FinishImportRun(ctx context.Context, id string, finishedAt time.Time, ok, failed int, results string) error {
	return s.exec(ctx, `UPDATE import_runs SET finished_at = ?, ok = ?, failed = ?, results = ? WHERE id = ?`,
		s.stamp(finishedAt).UnixMilli(), ok, failed, results, id)
}

// GetImportRun gives one batch import by its name.
//
// A name that is not there is an error: the caller asked about one
// particular import, and being handed a different one would be worse
// than being told there is none.
func (s *Store) GetImportRun(ctx context.Context, id string) (ImportRun, error) {
	var r ImportRun
	var started, finished int64
	err := s.db.QueryRowContext(ctx, `SELECT id, source, started_at, finished_at, total, ok, failed, COALESCE(results, '')
		FROM import_runs WHERE id = ?`, id).
		Scan(&r.ID, &r.Source, &started, &finished, &r.Total, &r.OK, &r.Failed, &r.Results)
	if err != nil {
		return ImportRun{}, err
	}
	r.StartedAt = time.UnixMilli(started).UTC()
	if finished > 0 {
		r.FinishedAt = time.UnixMilli(finished).UTC()
	}
	return r, nil
}

// ListImportRuns gives the most recent batch imports.
func (s *Store) ListImportRuns(ctx context.Context, limit int) ([]ImportRun, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, source, started_at, finished_at, total, ok, failed, COALESCE(results, '')
		FROM import_runs ORDER BY started_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ImportRun
	for rows.Next() {
		var r ImportRun
		var started, finished int64
		if err := rows.Scan(&r.ID, &r.Source, &started, &finished, &r.Total, &r.OK, &r.Failed, &r.Results); err != nil {
			return nil, err
		}
		r.StartedAt = time.UnixMilli(started).UTC()
		if finished > 0 {
			r.FinishedAt = time.UnixMilli(finished).UTC()
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
```

（`s.exec` 是这个包里现成的写库helper；名字不一样就看 `store.go` 里别的 setter 怎么写的，照着来。`sql.ErrNoRows` 需要 `"database/sql"` 这个 import。）

- [ ] **Step 4: 跑测试，确认它过了**

Run: `go test ./internal/store/ -count=1`
Expected: PASS（全部，包括迁移的测试）

- [ ] **Step 5: 跑整个测试套件**

Run: `go build ./... && go vet ./... && gofmt -l . && go test ./... -count=1`
Expected: 全绿

- [ ] **Step 6: 提交**

```bash
git add internal/store
git commit -m "feat(store): 账号那一列加密的密码，另有一张导入留痕的表

schema 到 4：accounts 加 password，另有 import_runs 记每次批量导入
的来龙去脉。

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 4: 网页登录那一层

**Files:**
- Create: `internal/webauth/webauth.go`
- Create: `internal/webauth/webauth_test.go`

- [ ] **Step 1: 写失败的测试**

创建 `internal/webauth/webauth_test.go`：

整个文件写成这样（下面是它的全文）：

```go
package webauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeSite is a stand-in for the account site.
//
// It answers the four calls the login makes and writes down what it
// was asked, so a test can look at the request the client really made.
type fakeSite struct {
	t        *testing.T
	server   *httptest.Server
	logins   []map[string]any
	deviceID string
	groups   string
	approved map[string]any
}

func newFakeSite(t *testing.T) *fakeSite {
	f := &fakeSite{t: t, deviceID: "574786429807779842"}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/api/user/device/register":
			if r.Header.Get("yy") == "" || r.Header.Get("x-signature") == "" {
				t.Errorf("the device registration is not signed")
			}
			// The device registration is the one call whose own query
			// is the uuid. The web client puts the common parameters
			// first and the caller's own parameter after them, so the
			// query ends with the uuid — which is in the common block
			// as well, mid-list (design document 3.2).
			parts := strings.Split(r.URL.RawQuery, "&")
			last := parts[len(parts)-1]
			if !strings.HasPrefix(last, "uuid=") {
				t.Errorf("the device registration query = %q, want it to end with the uuid", r.URL.RawQuery)
			}
			if n := strings.Count(r.URL.RawQuery, "uuid="); n != 2 {
				t.Errorf("the device registration query has %d uuid parameters, want 2: %q", n, r.URL.RawQuery)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"deviceIDStr": f.deviceID},
			})
		case "/oauth2/login":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("the login body is not json: %v", err)
			}
			f.logins = append(f.logins, body)
			if r.Header.Get("yy") == "" {
				t.Errorf("the login is not signed")
			}
			// The web flavour's own query: the common one, web's copy.
			if r.URL.Query().Get("client") != "web" || r.URL.Query().Get("device_platform") != "web" {
				t.Errorf("the login query = %q, want the web one", r.URL.RawQuery)
			}
			if body["authToken"] == nil {
				// 探测：邮箱这一步。
				json.NewEncoder(w).Encode(map[string]any{
					"data": nil, "statusInfo": map[string]any{"code": 0},
				})
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "_sid", Value: "session-1", Path: "/"})
			json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{"login_redirect": "/oauth-authorize"},
			})
		case "/oauth2/device/authorize":
			// 这两步网页用的是原生 fetch，不过 axios 拦截器：不签名，
			// 也不带那套公共 query。
			if r.Header.Get("yy") != "" || r.Header.Get("x-signature") != "" {
				t.Errorf("the device authorization is signed, but the web page does not sign it")
			}
			if r.URL.Query().Get("client") != "" {
				t.Errorf("the device authorization carries the common query, but the web page does not")
			}
			if c, err := r.Cookie("_sid"); err != nil || c.Value != "session-1" {
				t.Errorf("the device authorization has no session cookie")
			}
			if r.Method == http.MethodGet {
				w.Write([]byte(f.groups))
				return
			}
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			f.approved = body
			json.NewEncoder(w).Encode(map[string]any{"code": 0})
		default:
			t.Errorf("path = %s", r.URL.Path)
			w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeSite) client(t *testing.T) *Client {
	return New(Options{BaseURL: f.server.URL, HTTP: f.server.Client()})
}

// login makes the client hold a session, the way Password does.
func (f *fakeSite) login(t *testing.T, c *Client) {
	t.Helper()
	if err := c.Password(context.Background(), "a@example.com", "hunter2", "dev-1", "/oauth-authorize"); err != nil {
		t.Fatal(err)
	}
}

func TestTheDeviceIsRegistered(t *testing.T) {
	f := newFakeSite(t)
	c := f.client(t)
	id, err := c.RegisterDevice(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if id != "574786429807779842" {
		t.Fatalf("device id = %q", id)
	}
}

func TestTheEmailIsAskedBeforeThePassword(t *testing.T) {
	f := newFakeSite(t)
	c := f.client(t)
	if err := c.Probe(context.Background(), "a@example.com", "dev-1", "/oauth-authorize"); err != nil {
		t.Fatal(err)
	}
	if len(f.logins) != 1 {
		t.Fatalf("got %d logins", len(f.logins))
	}
	got := f.logins[0]
	if got["loginType"] != "20" {
		t.Errorf("loginType = %v", got["loginType"])
	}
	if got["email"] != "a@example.com" {
		t.Errorf("email = %v", got["email"])
	}
	if got["authToken"] != nil {
		t.Errorf("authToken = %v, want nothing", got["authToken"])
	}
}

func TestThePasswordIsEncryptedWithTheSitesKey(t *testing.T) {
	f := newFakeSite(t)
	c := f.client(t)
	if err := c.Password(context.Background(), "a@example.com", "hunter2", "dev-1", "/oauth-authorize"); err != nil {
		t.Fatal(err)
	}
	if len(f.logins) != 1 {
		t.Fatalf("got %d logins", len(f.logins))
	}
	token, _ := f.logins[0]["authToken"].(string)
	if token == "" || token == "hunter2" {
		t.Fatalf("authToken = %q", token)
	}
	// RSA-1024 密文是 128 字节，base64 之后 172 个字符。
	if len(token) != 172 {
		t.Fatalf("the cipher text is %d characters, want 172", len(token))
	}
}

func TestTheCodeIsApprovedWithTheSession(t *testing.T) {
	f := newFakeSite(t)
	f.groups = `{"code":0,"data":{"groups":[{"id":"g1","name":"default","is_default":true},{"id":"g2","is_default":false}]}}`
	c := f.client(t)
	f.login(t, c)
	if err := c.Approve(context.Background(), "ABCD-1234"); err != nil {
		t.Fatal(err)
	}
	if f.approved == nil {
		t.Fatal("nothing was approved")
	}
	if f.approved["user_code"] != "ABCD-1234" {
		t.Errorf("user_code = %v", f.approved["user_code"])
	}
	if f.approved["decision"] != "approve" {
		t.Errorf("decision = %v", f.approved["decision"])
	}
	if f.approved["group_id"] != "g1" {
		t.Errorf("group_id = %v, want the default group", f.approved["group_id"])
	}
}

func TestAGroupThatIsNotThereIsAnError(t *testing.T) {
	f := newFakeSite(t)
	f.groups = `{"code":0,"data":{"groups":[{"id":"g1","is_default":false}]}}`
	c := f.client(t)
	f.login(t, c)
	err := c.Approve(context.Background(), "ABCD-1234")
	if err == nil {
		t.Fatal("no error without a default group")
	}
	if !strings.Contains(err.Error(), "group") {
		t.Fatalf("err = %v", err)
	}
}

func TestTheSitesCodeBecomesAReadableError(t *testing.T) {
	f := newFakeSite(t)
	f.server.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"statusInfo": map[string]any{"code": 1200056, "message": "need password"},
		})
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, HTTP: srv.Client()})
	err := c.Probe(context.Background(), "a@example.com", "dev-1", "/oauth-authorize")
	if err == nil {
		t.Fatal("no error")
	}
	var se *SiteError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want a site error", err)
	}
	if se.Code != 1200056 {
		t.Fatalf("code = %d", se.Code)
	}
	if !strings.Contains(err.Error(), "密码") {
		t.Fatalf("err = %v, want it to say the account has no password", err)
	}
}

// A site that answers with an error is not an approval. The caller
// would otherwise go on waiting for a token that is never coming.
func TestAServerErrorIsNotAnApproval(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Write([]byte(`{"code":0,"data":{"groups":[{"id":"g1","is_default":true}]}}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, HTTP: srv.Client()})
	err := c.Approve(context.Background(), "ABCD-1234")
	if err == nil {
		t.Fatal("no error from a server that said no")
	}
}

// A refusal without a number of its own still has to say something a
// person can act on. The site's own word is English, so the Chinese
// explanation comes from the word.
func TestARefusalWithoutACodeIsStillExplained(t *testing.T) {
	err := &SiteError{Reason: "login_required", Message: "account session is missing or expired"}
	if got := err.ExplainText(); got != "要先登录才能批准" {
		t.Errorf("ExplainText = %q", got)
	}
	text := err.Error()
	if !strings.Contains(text, "登录") {
		t.Errorf("err = %q, want it to say a login is needed", text)
	}
	// The site's own words come along, but they do not replace the
	// explanation.
	if !strings.Contains(text, "account session is missing or expired") {
		t.Errorf("err = %q, want the site's own words in it too", text)
	}
}

// Even a word nobody has heard of gets a Chinese sentence.
func TestAnUnknownRefusalWordIsStillExplained(t *testing.T) {
	err := &SiteError{Reason: "something_new"}
	text := err.Error()
	if text == "" {
		t.Fatal("no explanation at all")
	}
	if !strings.Contains(text, "站点") {
		t.Errorf("err = %q, want it to say the site refused", text)
	}
}

// The approve call answers with a word, not with a number, when the
// session is no good.
func TestTheApproveRefusalIsExplained(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Write([]byte(`{"code":0,"data":{"groups":[{"id":"g1","is_default":true}]}}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"error":             "login_required",
			"error_description": "account session is missing or expired",
		})
	}))
	defer srv.Close()

	c := New(Options{BaseURL: srv.URL, HTTP: srv.Client()})
	err := c.Approve(context.Background(), "ABCD-1234")
	if err == nil {
		t.Fatal("no error")
	}
	var se *SiteError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want a site error", err)
	}
	if se.Reason != "login_required" {
		t.Errorf("reason = %q", se.Reason)
	}
	if !strings.Contains(err.Error(), "登录") {
		t.Errorf("err = %v, want it to say a login is needed", err)
	}
}

// The session belongs to one account. Two clients must not share one,
// or one account's session would approve another account's code.
func TestEachClientHasItsOwnSession(t *testing.T) {
	// This one gets its own server, because the point is to see that a
	// client which never signed in sends no session cookie — and the
	// fake site above treats a missing cookie as a failure.
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("_sid")
		if err != nil {
			seen = append(seen, "")
		} else {
			seen = append(seen, c.Value)
		}
		if r.URL.Path == "/oauth2/login" {
			http.SetCookie(w, &http.Cookie{Name: "_sid", Value: "session-1", Path: "/"})
			json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{}})
			return
		}
		if r.Method == http.MethodGet {
			w.Write([]byte(`{"code":0,"data":{"groups":[{"id":"g1","is_default":true}]}}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"code": 0})
	}))
	defer srv.Close()

	a := New(Options{BaseURL: srv.URL, HTTP: srv.Client()})
	b := New(Options{BaseURL: srv.URL, HTTP: srv.Client()})
	if err := a.Password(context.Background(), "a@example.com", "hunter2", "dev-1", "/oauth-authorize"); err != nil {
		t.Fatal(err)
	}
	if err := a.Approve(context.Background(), "ABCD-1234"); err != nil {
		t.Fatal(err)
	}
	if err := b.Approve(context.Background(), "ABCD-1234"); err != nil {
		t.Fatal(err)
	}
	// a 的登录那一步还没有会话，批准那两步带着会话；b 从来没有登过，
	// 它的两步都不带。jar 是各自的。
	want := []string{"", "session-1", "session-1", "", ""}
	if len(seen) != len(want) {
		t.Fatalf("got %d requests, want %d", len(seen), len(want))
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("request %d had the session %q, want %q", i, seen[i], want[i])
		}
	}
}
```

- [ ] **Step 2: 跑测试，确认它失败**

Run: `go test ./internal/webauth/ -count=1`
Expected: FAIL（包还不存在）

- [ ] **Step 3: 写实现**

创建 `internal/webauth/webauth.go`：

整个文件写成这样（下面是它的全文）：

```go
// Package webauth signs in to MiniMax's account site the way the web
// page does.
//
// The device flow the CLI uses needs a person to approve the code in
// a browser. This package does the browser's part without a browser:
// it signs in with an email and a password, keeps the session cookie
// the site hands back, and uses that session to approve the code.
//
// The steps it takes are the ones the web page takes:
//   - register a device, so that there is a device number to send;
//   - ask the site which way this account signs in;
//   - sign in with the password, encrypted with the site's own key;
//   - approve the device code with the session that sign-in gave.
//
// The evidence for each step is in the design document.
package webauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"time"

	"mmxcode/internal/sign"
	"mmxcode/internal/site"
)

// Paths of the calls the login makes.
const (
	// DeviceRegisterPath registers the device.
	DeviceRegisterPath = "/v1/api/user/device/register"
	// LoginPath signs in.
	LoginPath = "/oauth2/login"
	// DeviceAuthorizePath shows and approves a device code.
	DeviceAuthorizePath = "/oauth2/device/authorize"
)

// AppID is the application the device code belongs to.
const AppID = "3001"

// LoginTypePassword is the site's "email and password" way in.
const LoginTypePassword = "20"

// Options is how the client reaches the site.
type Options struct {
	// BaseURL is the account site's origin.
	BaseURL string
	// HTTP is the client to use. It carries the proxy. A nil one is
	// a client with a thirty second timeout.
	HTTP *http.Client
	// Now is the clock. A nil one is time.Now.
	Now func() time.Time
	// TZOffset is the client's timezone offset in seconds.
	TZOffset int
	// Lang is the site's language.
	Lang string
}

// Client talks to the account site as the web page does.
type Client struct {
	base string
	http *http.Client
	now  func() time.Time
	tz   int
	lang string

	// uuid is the long-lived device id the browser keeps in its
	// local storage, and deviceID is the number the site handed out.
	uuid     string
	deviceID string
}

// New makes a client.
//
// The client gets its own cookie jar: the session that signs in
// belongs to one account, and two accounts must never share one.
func New(o Options) *Client {
	c := &Client{
		base: strings.TrimRight(o.BaseURL, "/"),
		now:  o.Now,
		tz:   o.TZOffset,
		lang: o.Lang,
		uuid: newUUID(),
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.lang == "" {
		c.lang = site.Lang
	}
	hc := o.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		jar = nil
	}
	clone := *hc
	clone.Jar = jar
	c.http = &clone
	return c
}

// DeviceID is the number the site handed out for this device.
func (c *Client) DeviceID() string { return c.deviceID }

// RegisterDevice tells the site about this device.
//
// The browser does it once and keeps the number; the site puts it in
// the fingerprint it sends with every request afterwards.
func (c *Client) RegisterDevice(ctx context.Context) (string, error) {
	if c.uuid == "" {
		c.uuid = newUUID()
	}
	body, err := jsonBody(map[string]any{"uuid": c.uuid})
	if err != nil {
		return "", err
	}
	// The device registration is the one call whose own query is the
	// uuid (design document 3.2). The web client appends the common
	// parameters first and its own afterwards, so the uuid comes last
	// -- and it is in the query twice, because the common block has
	// one of its own.
	s := c.sign(DeviceRegisterPath, body, []sign.KV{{K: "uuid", V: c.uuid}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	c.setHeaders(req, s)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("device register: %w", err)
	}
	defer resp.Body.Close()
	var answer struct {
		Data struct {
			DeviceIDStr string `json:"deviceIDStr"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		return "", fmt.Errorf("device register: %w", err)
	}
	if answer.Data.DeviceIDStr == "" {
		return "", errors.New("device register: the site gave no device number")
	}
	c.deviceID = answer.Data.DeviceIDStr
	return c.deviceID, nil
}

// Probe asks the site which way this account signs in.
//
// The site answers with a code. Zero means the account has a password
// and can sign in with it; the other codes say what is missing.
func (c *Client) Probe(ctx context.Context, email, deviceID, redirect string) error {
	_, err := c.login(ctx, email, nil, deviceID, redirect)
	return err
}

// Password signs in with a password.
//
// The password is encrypted with the site's own public key, the way
// the web page encrypts it, and the site answers with a session
// cookie.
func (c *Client) Password(ctx context.Context, email, password, deviceID, redirect string) error {
	token, err := encryptPassword(password)
	if err != nil {
		return err
	}
	_, err = c.login(ctx, email, &token, deviceID, redirect)
	return err
}

// login posts to the site's login endpoint.
//
// A nil authToken is the probe: it asks the site how this account
// signs in. A token is the password, encrypted with the site's key.
func (c *Client) login(ctx context.Context, email string, authToken *string, deviceID, redirect string) (map[string]any, error) {
	body := map[string]any{
		"loginType":      LoginTypePassword,
		"email":          email,
		"authToken":      nil,
		"deviceID":       deviceID,
		"login_redirect": redirect,
	}
	if authToken != nil {
		body["authToken"] = *authToken
		body["countryCode"] = "+86"
	}
	raw, err := jsonBody(body)
	if err != nil {
		return nil, err
	}
	s := c.sign(LoginPath, raw, nil)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	c.setHeaders(req, s)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}
	defer resp.Body.Close()
	var answer struct {
		Code       int            `json:"code"`
		Data       map[string]any `json:"data"`
		StatusInfo *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"statusInfo"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}
	if answer.StatusInfo != nil {
		if answer.StatusInfo.Code != 0 {
			return nil, &SiteError{Code: answer.StatusInfo.Code, Message: answer.StatusInfo.Message}
		}
		return answer.Data, nil
	}
	// No statusInfo: the site answered with the top-level code, which
	// is how the mcode side refuses a sign-in (1000048).
	if answer.Code != 0 {
		return nil, &SiteError{Code: answer.Code}
	}
	return answer.Data, nil
}

// Approve lets the device code in.
//
// The session that Password got is the permission: without it the
// site answers 401 and says a login is needed.
//
// The approve call is not signed and carries no common query: the web
// page makes it with the browser's native fetch, which does not go
// through the interceptor that signs the rest.
func (c *Client) Approve(ctx context.Context, userCode string) error {
	group, err := c.defaultGroup(ctx, userCode)
	if err != nil {
		return err
	}
	body, err := jsonBody(map[string]any{
		"user_code": userCode,
		"decision":  "approve",
		"group_id":  group,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+DeviceAuthorizePath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("approve: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return errors.New("approve: the session is not good any more")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// A refusal that is not a refusal must not look like an
		// approval: the caller would go on waiting for a token that
		// is never going to come.
		return fmt.Errorf("approve: the site answered %s", resp.Status)
	}
	var answer struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
		Message          string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&answer); err == nil {
		if answer.Error != "" {
			msg := answer.ErrorDescription
			if msg == "" {
				msg = answer.Message
			}
			return &SiteError{Message: msg, Reason: answer.Error}
		}
	}
	return nil
}

// defaultGroup asks the site which group the code belongs to.
//
// This call is not signed either, for the same reason the approve is
// not.
func (c *Client) defaultGroup(ctx context.Context, userCode string) (string, error) {
	// The query is the one the web page sends, in its order. This call
	// is not signed, so the order does not matter to the signature.
	query := "user_code=" + url.QueryEscape(userCode) + "&app_id=" + AppID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.base+DeviceAuthorizePath+"?"+query, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("device authorize: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return "", errors.New("device authorize: the site wants a login")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("device authorize: the site answered %s", resp.Status)
	}
	var answer any
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		return "", fmt.Errorf("device authorize: %w", err)
	}
	groups := findGroups(answer)
	for _, g := range groups {
		if def, ok := g["is_default"].(bool); ok && def {
			if id, ok := g["id"].(string); ok && id != "" {
				return id, nil
			}
		}
	}
	return "", ErrNoDefaultGroup
}

// ErrNoDefaultGroup is the answer when the site has no default group
// to approve the code into.
//
// It is not worth trying again: the group is not there, and it will
// not be there in five seconds either.
var ErrNoDefaultGroup = errors.New("device authorize: there is no default group to approve")

// findGroups finds the groups in the site's answer.
//
// The shape of the answer is not documented, so the groups are looked
// for anywhere in it: any array whose entries have an "id" and an
// "is_default".
func findGroups(v any) []map[string]any {
	var out []map[string]any
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for _, child := range t {
				walk(child)
			}
		case []any:
			for _, child := range t {
				walk(child)
			}
			items := make([]map[string]any, 0, len(t))
			for _, item := range t {
				m, ok := item.(map[string]any)
				if !ok {
					return
				}
				if _, hasID := m["id"]; !hasID {
					return
				}
				items = append(items, m)
			}
			if len(items) > 0 {
				out = append(out, items...)
			}
		}
	}
	walk(v)
	return out
}

// sign makes a signed request for the web flavour.
//
// The query is the caller's own query. The web flavour puts the common
// parameters first and the caller's after them, so a key that is in
// both halves is in the query twice.
func (c *Client) sign(path string, body []byte, query []sign.KV) sign.Signed {
	return sign.Sign(c.base, path, sign.Options{
		Flavor:   sign.FlavorWeb,
		Now:      c.now().UnixMilli(),
		TZOffset: c.tz,
		Lang:     c.lang,
		Body:     body,
		UUID:     c.uuid,
		DeviceID: c.deviceID,
		Query:    query,
	})
}

// setHeaders puts the signed headers on a request.
func (c *Client) setHeaders(req *http.Request, s sign.Signed) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("yy", s.YY)
	req.Header.Set("x-timestamp", s.Timestamp)
	req.Header.Set("x-signature", s.Signature)
}

// SiteError is an answer the site refused with.
type SiteError struct {
	// Code is the site's own code.
	Code int
	// Message is what the site said.
	Message string
	// Reason is the site's own word for the refusal ("login_required"
	// and the like). A refusal that comes without a number of its own
	// carries the word, and that is what ExplainReason explains.
	Reason string
}

// ExplainText says what the refusal means, in a human's words.
//
// It is Explain for a code the site gave, and ExplainReason for a
// refusal that came without one. There is always a Chinese sentence
// here: the site's own words are English and say nothing a person can
// act on.
func (e *SiteError) ExplainText() string {
	if e.Code == 0 {
		return ExplainReason(e.Reason)
	}
	return Explain(e.Code)
}

func (e *SiteError) Error() string {
	// The site's own message is in English and says nothing a person
	// can act on; the code is what says what to do. The site's words
	// come along, but they do not replace the explanation.
	msg := e.ExplainText()
	if e.Message != "" {
		msg = fmt.Sprintf("%s（站点说：%s）", msg, e.Message)
	}
	if e.Code == 0 {
		return "the site refused the sign-in: " + msg
	}
	return fmt.Sprintf("the site refused the sign-in (%d): %s", e.Code, msg)
}

// ExplainReason says what the site's own word for a refusal means.
//
// Some refusals come without a number of their own — the site just
// says "login_required" or the like. That word is English and says
// nothing a person can act on, so the explanation comes from the word.
func ExplainReason(reason string) string {
	switch reason {
	case "login_required", "login_required_2":
		return "要先登录才能批准"
	case "invalid_grant":
		return "这个设备码已经不能用了"
	case "access_denied":
		return "站点没有让这个设备码进来"
	}
	return "站点拒绝了，没有说为什么"
}

// Explain says what a site code means in a human's words.
func Explain(code int) string {
	switch code {
	case 11:
		return "这个账号没有权限登录"
	case 1200007:
		return "登录状态无效"
	case 1200044:
		return "这个账号还没绑手机，去网站绑了再来"
	case 1200046:
		return "密码不对"
	case 1200056:
		return "这个账号还没设密码，去网站设了再来"
	case 1200057:
		return "这个账号的密码太旧了，去网站改过再来"
	case 1200062:
		return "子账号第一次登录要改密码"
	case 1000048:
		return "站点拒绝了这个登录"
	default:
		return "站点说了 " + strconv.Itoa(code)
	}
}

// jsonBody builds a body the way the browser's JSON.stringify does:
// compact, and with no HTML escaping.
//
// The bytes are what gets signed, so they have to be the bytes that
// go on the wire. A map's keys are sorted by Go's encoder, which is
// not what JSON.stringify does, but the site does not care about the
// order of a JSON object's keys, and the signature is over exactly
// these bytes — so the sorted order is fine.
func jsonBody(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// newUUID makes a version 4 UUID.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
```



再创建 `internal/webauth/rsa.go`：

```go
package webauth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
)

// publicKeyPEM is the key the web page encrypts the password with.
//
// The page hard-codes it (js/204, module 7160) and encrypts with
// RSA-1024 and PKCS#1 v1.5, which is what JSEncrypt does. The cipher
// text is 128 bytes, and every time it is different: the padding is
// random.
const publicKeyPEM = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDF5ndG2/UB4L5tbvQaNLHSoBTW
DKbrNBuOmUIP23eCmC2ELMx3kppEikxTp5cV8NxUZl6ii+KLwKugioAXApzypHXb
gXbq13kTKA7OCA1xtAoMdH9cltjBiFAUJlgmVjr0MuJCknhVAjWLjCVRHege+Atl
gkUBUeGa9O+cWcPEwQIDAQAB
-----END PUBLIC KEY-----`

// encryptPassword encrypts a password the way the web page does.
//
// The result is base64 of the RSA cipher text, which is what the site
// expects in the "authToken" field.
//
// The padding of PKCS#1 v1.5 is random, so the same password gives a
// different cipher text every time. The cipher text is not a password
// hash and must never be stored as one.
func encryptPassword(password string) (string, error) {
	block, _ := pem.Decode([]byte(publicKeyPEM))
	if block == nil {
		return "", errors.New("the site's key is not a key")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return "", err
	}
	key, ok := pub.(*rsa.PublicKey)
	if !ok {
		return "", errors.New("the site's key is not an RSA key")
	}
	cipher, err := rsa.EncryptPKCS1v15(rand.Reader, key, []byte(password))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(cipher), nil
}
```
- [ ] **Step 4: 跑测试，确认它过了**

Run: `go test ./internal/webauth/ -count=1`
Expected: PASS

- [ ] **Step 5: 跑整个测试套件**

Run: `go build ./... && go vet ./... && gofmt -l . && go test ./... -count=1`
Expected: 全绿

- [ ] **Step 6: 提交**

```bash
git add internal/webauth
git commit -m "feat(webauth): 用邮箱和密码登录账号站，再拿会话去批准设备码

网页那套：注册设备、用邮箱探测、密码走站点自己的 RSA 公钥加密、
拿到 _sid 会话，然后用会话批准 mcode 的设备码。

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 5: 批量任务

**Files:**
- Create: `internal/provision/provision.go`
- Create: `internal/provision/provision_test.go`

- [ ] **Step 1: 写失败的测试**

创建 `internal/provision/provision_test.go`：

```go
package provision

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestTheListIsRead(t *testing.T) {
	list := `
# 这是注释
a@example.com|password

b@example.com|pass|with|bar
`
	entries, bad := Parse(strings.NewReader(list))
	if len(entries) != 2 {
		t.Fatalf("got %d entries: %+v", len(entries), entries)
	}
	if entries[0].Email != "a@example.com" || entries[0].Password != "password" {
		t.Fatalf("entry = %+v", entries[0])
	}
	if entries[1].Password != "pass|with|bar" {
		t.Fatalf("the password kept only its first part: %q", entries[1].Password)
	}
	if len(bad) != 0 {
		t.Fatalf("got %d bad lines: %+v", len(bad), bad)
	}
}

func TestALineThatIsNotAnAccount(t *testing.T) {
	entries, bad := Parse(strings.NewReader("not-an-email\n"))
	if len(entries) != 0 {
		t.Fatalf("got %d entries", len(entries))
	}
	if len(bad) != 1 {
		t.Fatalf("got %d bad lines", len(bad))
	}
	if bad[0].Line != 1 {
		t.Errorf("the line is %d, want 1", bad[0].Line)
	}
}

func TestALineWithNoPassword(t *testing.T) {
	entries, bad := Parse(strings.NewReader("a@example.com\n"))
	if len(entries) != 0 || len(bad) != 1 {
		t.Fatalf("entries = %+v, bad = %+v", entries, bad)
	}
}

func TestTheSameEmailTwiceIsOnlyOneAccount(t *testing.T) {
	entries, bad := Parse(strings.NewReader("a@example.com|p\na@example.com|q\n"))
	if len(entries) != 1 {
		t.Fatalf("got %d entries: %+v", len(entries), entries)
	}
	if len(bad) != 1 {
		t.Fatalf("got %d bad lines: %+v", len(bad), bad)
	}
	if bad[0].Line != 2 {
		t.Errorf("the line is %d, want 2", bad[0].Line)
	}
}

func TestThePasswordIsNotInTheBadLine(t *testing.T) {
	entries, bad := Parse(strings.NewReader("not-an-email|secret\nb@example.com|\n"))
	if len(entries) != 0 {
		t.Fatalf("got %d entries: %+v", len(entries), entries)
	}
	if len(bad) != 2 {
		t.Fatalf("got %d bad lines: %+v", len(bad), bad)
	}
	for _, b := range bad {
		if strings.Contains(b.Text, "secret") {
			t.Fatalf("the password is in the bad line: %+v", b)
		}
	}
}

func TestTheRunnerTakesThemOneByOne(t *testing.T) {
	var seen []string
	r := &Runner{
		Importer: &Importer{
			Import: func(ctx context.Context, email, password string) (Result, error) {
				seen = append(seen, email)
				return Result{Email: email, OK: true, AccountID: "acc_" + email}, nil
			},
		},
		Interval: time.Millisecond,
		Sleep:    func(context.Context, time.Duration) error { return nil },
	}
	out, err := r.Run(context.Background(), []Entry{
		{Email: "a@example.com", Password: "p"},
		{Email: "b@example.com", Password: "p"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || !out[0].OK || !out[1].OK {
		t.Fatalf("out = %+v", out)
	}
	if len(seen) != 2 || seen[0] != "a@example.com" || seen[1] != "b@example.com" {
		t.Fatalf("seen = %v", seen)
	}
}

func TestOneBadAccountDoesNotStopTheRest(t *testing.T) {
	r := &Runner{
		Importer: &Importer{
			Import: func(ctx context.Context, email, password string) (Result, error) {
				if email == "bad@example.com" {
					return Result{Email: email}, &Error{Stage: StagePassword, Code: 1200046, Message: "wrong password"}
				}
				return Result{Email: email, OK: true}, nil
			},
		},
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	out, err := r.Run(context.Background(), []Entry{
		{Email: "bad@example.com", Password: "p"},
		{Email: "good@example.com", Password: "p"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("out = %+v", out)
	}
	if out[0].OK || out[0].Stage != StagePassword || out[0].Code != 1200046 {
		t.Fatalf("the first one = %+v", out[0])
	}
	if !out[1].OK {
		t.Fatalf("the second one = %+v", out[1])
	}
}

func TestANetworkErrorIsTriedAgain(t *testing.T) {
	var tries int
	r := &Runner{
		Importer: &Importer{
			Import: func(ctx context.Context, email, password string) (Result, error) {
				tries++
				if tries < 3 {
					return Result{Email: email}, &Error{Stage: StageToken, Message: "connection reset", Retry: true, Wait: time.Millisecond}
				}
				return Result{Email: email, OK: true}, nil
			},
		},
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	out, err := r.Run(context.Background(), []Entry{{Email: "a@example.com", Password: "p"}})
	if err != nil {
		t.Fatal(err)
	}
	if tries != 3 {
		t.Fatalf("tried %d times, want 3", tries)
	}
	if !out[0].OK {
		t.Fatalf("out = %+v", out)
	}
}

func TestAWrongPasswordIsNotTriedAgain(t *testing.T) {
	var tries int
	r := &Runner{
		Importer: &Importer{
			Import: func(ctx context.Context, email, password string) (Result, error) {
				tries++
				return Result{Email: email}, &Error{Stage: StagePassword, Code: 1200046}
			},
		},
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	if _, err := r.Run(context.Background(), []Entry{{Email: "a@example.com", Password: "p"}}); err != nil {
		t.Fatal(err)
	}
	if tries != 1 {
		t.Fatalf("tried %d times, want 1", tries)
	}
}

func TestTheTriesAreNotEndless(t *testing.T) {
	var tries int
	r := &Runner{
		Importer: &Importer{
			Import: func(ctx context.Context, email, password string) (Result, error) {
				tries++
				return Result{Email: email}, &Error{Stage: StageToken, Code: 1200007, Message: "not logged in", Retry: true}
			},
		},
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	out, err := r.Run(context.Background(), []Entry{{Email: "a@example.com", Password: "p"}})
	if err != nil {
		t.Fatal(err)
	}
	if tries != 3 {
		t.Fatalf("tried %d times, want 3", tries)
	}
	if out[0].OK {
		t.Fatalf("out = %+v", out[0])
	}
	if out[0].Code != 1200007 {
		t.Fatalf("the site's own code is gone: %+v", out[0])
	}
}

func TestTheRunStopsWhenTheContextIsDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &Runner{
		Importer: &Importer{
			Import: func(ctx context.Context, email, password string) (Result, error) {
				return Result{}, errors.New("should not be called")
			},
		},
	}
	if _, err := r.Run(ctx, []Entry{{Email: "a@example.com"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the cancellation", err)
	}
}

func TestTheRunStopsInTheMiddleOfAnAccount(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &Runner{
		Importer: &Importer{
			Import: func(ctx context.Context, email, password string) (Result, error) {
				cancel()
				return Result{Email: email}, &Error{Stage: StageToken, Message: "connection reset", Retry: true, Wait: time.Millisecond}
			},
		},
		Sleep: func(ctx context.Context, d time.Duration) error { return ctx.Err() },
	}
	if _, err := r.Run(ctx, []Entry{{Email: "a@example.com"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the cancellation", err)
	}
}

func TestTheResultsAreTheTextTheDatabaseKeeps(t *testing.T) {
	raw := ResultsJSON([]Result{{
		Email:   "a@example.com",
		OK:      false,
		Stage:   StagePassword,
		Code:    1200046,
		Message: "wrong password",
	}})
	for _, want := range []string{`"email":"a@example.com"`, `"ok":false`, `"stage":"password"`, `"code":1200046`} {
		if !strings.Contains(raw, want) {
			t.Fatalf("%s is not in %s", want, raw)
		}
	}
}
```

- [ ] **Step 2: 跑测试，确认它失败**

Run: `go test ./internal/provision/ -count=1`
Expected: FAIL（包还不存在）

- [ ] **Step 3: 写实现**

创建 `internal/provision/provision.go`：

```go
// Package provision brings accounts in.
//
// It reads a list of "email|password" lines and, for each of them,
// runs the whole chain: sign in to the account site, approve a device
// code with that session, and put the account in the database.
//
// One account at a time. The site is watching, and a hundred logins
// in a second is how a person gets noticed.
package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Stage is where an account's import stopped.
type Stage string

// The stages an import goes through.
const (
	// StageRead is reading the list.
	StageRead Stage = "read"
	// StageProbe is asking the site which way the account signs in.
	StageProbe Stage = "probe"
	// StagePassword is signing in with the password.
	StagePassword Stage = "password"
	// StageApprove is approving the device code.
	StageApprove Stage = "approve"
	// StageToken is turning the code into a token.
	StageToken Stage = "token"
	// StageStore is putting the account in the database.
	StageStore Stage = "store"
)

// Entry is one account from the list.
type Entry struct {
	// Email is the account's email.
	Email string
	// Password is the account's password.
	Password string
	// Line is the line it was on, for the messages.
	Line int
}

// BadLine is a line of the list that is not an account.
type BadLine struct {
	// Line is the line it was on.
	Line int
	// Text is the line itself, with the password taken off it.
	Text string
	// Reason says what is wrong with it.
	Reason string
}

// Parse reads the list.
//
// One account per line: "email|password". A line that starts with a
// "#" is a comment and an empty line is skipped. Everything after the
// first "|" is the password, so a password may contain a "|".
//
// An email that is already in the list is skipped: the second one is
// said to be a line that is not an account.
func Parse(r io.Reader) ([]Entry, []BadLine) {
	var entries []Entry
	var bad []BadLine
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, []BadLine{{Line: 0, Reason: err.Error()}}
	}
	seen := map[string]bool{}
	for i, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		n := i + 1
		text := strings.TrimSpace(line)
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		at := strings.Index(text, "|")
		if at < 0 {
			bad = append(bad, BadLine{Line: n, Text: mask(text), Reason: "没有密码"})
			continue
		}
		email := strings.TrimSpace(text[:at])
		password := text[at+1:]
		if !looksLikeEmail(email) {
			bad = append(bad, BadLine{Line: n, Text: mask(text), Reason: "不像邮箱"})
			continue
		}
		if password == "" {
			bad = append(bad, BadLine{Line: n, Text: mask(email), Reason: "没有密码"})
			continue
		}
		key := strings.ToLower(email)
		if seen[key] {
			bad = append(bad, BadLine{Line: n, Text: email, Reason: "已有这个账号"})
			continue
		}
		seen[key] = true
		entries = append(entries, Entry{Email: email, Password: password, Line: n})
	}
	return entries, bad
}

// Result is what happened to one account.
type Result struct {
	// Email is the account's email.
	Email string `json:"email"`
	// OK says whether the account is in the database now.
	OK bool `json:"ok"`
	// Stage is where it stopped.
	Stage Stage `json:"stage"`
	// Code is the site's own code, when the site gave one.
	Code int `json:"code"`
	// Message says what happened.
	Message string `json:"message"`
	// AccountID is the account's id, when it made it in.
	AccountID string `json:"account_id,omitempty"`
}

// Error is an import failure and what to do about it.
type Error struct {
	// Stage is where it happened.
	Stage Stage
	// Code is the site's own code, when the site gave one.
	Code int
	// Message says what happened.
	Message string
	// Retry says whether trying again could work.
	Retry bool
	// Wait is how long to wait before trying again.
	Wait time.Duration
}

func (e *Error) Error() string {
	msg := e.Message
	if msg == "" {
		msg = "这一步没成"
	}
	if e.Code != 0 {
		return fmt.Sprintf("%s：%s（%d）", e.Stage, msg, e.Code)
	}
	return fmt.Sprintf("%s：%s", e.Stage, msg)
}

// Importer brings one account in.
//
// The chain is in Import; the Runner drives it. Import is a field so
// that a test can bring an account in without a network.
type Importer struct {
	// Import signs the account in and puts it in the database.
	Import func(ctx context.Context, email, password string) (Result, error)
}

// Runner runs a batch.
type Runner struct {
	// Importer brings one account in.
	Importer *Importer
	// Interval is the wait between two accounts.
	Interval time.Duration
	// Sleep waits. A nil one waits with the context.
	Sleep func(ctx context.Context, d time.Duration) error
}

// tries is how many times one account is tried at most.
//
// The first try and two more: a network hiccup is worth another go, a
// password that is wrong is not, and three tries is enough to tell the
// two apart without hammering the site.
const tries = 3

// Run brings in every entry and gives back what happened to each.
//
// One account does not stop the rest: a wrong password says nothing
// about the next one.
//
// The error is only for the run itself stopping -- the context was
// cancelled. An account that failed is a result, not an error.
func (r *Runner) Run(ctx context.Context, entries []Entry) ([]Result, error) {
	if r.Importer == nil || r.Importer.Import == nil {
		return nil, errors.New("provision: 没有导入器")
	}
	var out []Result
	for i, e := range entries {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if i > 0 && r.Interval > 0 {
			if err := r.wait(ctx, r.Interval); err != nil {
				return out, err
			}
		}
		out = append(out, r.one(ctx, e))
		// The account may have been cut short half way through, and
		// the context is checked once more here: a run that was cut
		// short must say so, even when the account it was cut short
		// on was the last one.
		if err := ctx.Err(); err != nil {
			return out, err
		}
	}
	return out, nil
}

// one brings one account in, trying again when it is worth it.
func (r *Runner) one(ctx context.Context, e Entry) Result {
	var last *Error
	for attempt := 0; attempt < tries; attempt++ {
		res, err := r.Importer.Import(ctx, e.Email, e.Password)
		if err == nil {
			if res.Email == "" {
				res.Email = e.Email
			}
			return res
		}
		var pe *Error
		if !errors.As(err, &pe) || !pe.Retry {
			return resultOf(e, err)
		}
		last = pe
		if attempt == tries-1 {
			// The last try is the last one: waiting after it only
			// makes the answer come later.
			break
		}
		wait := pe.Wait
		if wait <= 0 {
			wait = time.Duration(1<<attempt) * time.Second
		}
		if err := r.wait(ctx, wait); err != nil {
			return resultOf(e, err)
		}
	}
	res := resultOf(e, last)
	res.Message = fmt.Sprintf("试了 %d 次都不成：%s", tries, res.Message)
	return res
}

// resultOf turns an error into a result.
func resultOf(e Entry, err error) Result {
	res := Result{Email: e.Email}
	var pe *Error
	if errors.As(err, &pe) {
		res.Stage = pe.Stage
		res.Code = pe.Code
		res.Message = pe.Message
	} else {
		res.Stage = StageToken
		res.Message = err.Error()
	}
	return res
}

// wait waits, or gives up when the context is done.
func (r *Runner) wait(ctx context.Context, d time.Duration) error {
	if r.Sleep != nil {
		return r.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// looksLikeEmail says whether a string could be an email.
//
// It is not a full check: the site is the one that says whether the
// account is there.
func looksLikeEmail(s string) bool {
	at := strings.Index(s, "@")
	if at <= 0 || at == len(s)-1 {
		return false
	}
	if strings.ContainsAny(s, " \t") {
		return false
	}
	return strings.Contains(s[at+1:], ".")
}

// mask hides the password of a line that could not be read.
//
// The text of a bad line goes into the database's import-run log and
// into the terminal, so a password must not be in it.
func mask(text string) string {
	at := strings.Index(text, "|")
	if at < 0 {
		return text
	}
	return text[:at] + "|***"
}

// ResultsJSON turns the results into the text the database keeps.
func ResultsJSON(results []Result) string {
	raw, err := json.Marshal(results)
	if err != nil {
		return "[]"
	}
	return string(raw)
}
```

（`Parse` 收的是 `io.Reader`，读全部用 `io.ReadAll`。`Runner.Now` 这个字段计划里写了但用不上，去掉。）

- [ ] **Step 4: 跑测试，确认它过了**

Run: `go test ./internal/provision/ -count=1`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/provision
git commit -m "feat(provision): 批量入号的骨架

读清单、逐个跑、分类失败、可重试的错误重试两次。失败的号不入库。

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 6: 把整条链接起来

**Files:**
- Create: `internal/provision/chain.go`
- Create: `internal/provision/chain_test.go`

- [ ] **Step 1: 写失败的测试**

创建 `internal/provision/chain_test.go`：

整个文件写成这样（下面是它的全文）：

```go
package provision

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"mmxcode/internal/account"
	"mmxcode/internal/oauth"
	"mmxcode/internal/store"
	"mmxcode/internal/webauth"
)

// fakeStore keeps what it is given.
type fakeStore struct {
	accounts []string
	tokens   []string
	password []string
	// failToken makes the token refuse to be saved.
	failToken bool
}

func (s *fakeStore) CreateAccount(ctx context.Context, a store.Account) error {
	s.accounts = append(s.accounts, a.ID+":"+a.Email)
	return nil
}

func (s *fakeStore) SaveToken(ctx context.Context, t store.Token) error {
	if s.failToken {
		return errors.New("the disk is full")
	}
	s.tokens = append(s.tokens, t.AccountID+":"+t.Refresh)
	return nil
}

func (s *fakeStore) SetPassword(ctx context.Context, id, cipher string, updatedAt time.Time) error {
	s.password = append(s.password, id+":"+cipher)
	return nil
}

// DeleteAccount takes an account back out.
func (s *fakeStore) DeleteAccount(ctx context.Context, id string) error {
	for i, a := range s.accounts {
		if strings.HasPrefix(a, id+":") {
			s.accounts = append(s.accounts[:i], s.accounts[i+1:]...)
			break
		}
	}
	return nil
}

// fakeWeb stands in for the account site.
type fakeWeb struct {
	deviceID   string
	probed     []string
	signed     []string
	approved   []string
	email      string
	password   string
	redirect   string
	approveErr error
}

func (w *fakeWeb) RegisterDevice(ctx context.Context) (string, error) {
	return w.deviceID, nil
}

func (w *fakeWeb) Probe(ctx context.Context, email, deviceID, redirect string) error {
	w.probed = append(w.probed, email)
	w.email = email
	w.redirect = redirect
	return nil
}

func (w *fakeWeb) Password(ctx context.Context, email, password, deviceID, redirect string) error {
	w.signed = append(w.signed, email)
	w.email = email
	w.password = password
	w.redirect = redirect
	return nil
}

func (w *fakeWeb) Approve(ctx context.Context, userCode string) error {
	w.approved = append(w.approved, userCode)
	return w.approveErr
}

// aChain is a chain that does everything but talk to the site.
func aChain(st *fakeStore) *Chain {
	return &Chain{
		Store: st,
		Start: func(ctx context.Context) (*oauth.Device, error) {
			return &oauth.Device{UserCode: "ABCD"}, nil
		},
		Poll: func(ctx context.Context, d *oauth.Device) (*oauth.Token, error) {
			return &oauth.Token{Access: "acc", Refresh: "ref", Subject: "sub"}, nil
		},
		Identity: func(ctx context.Context, access, subject string) (account.Identity, error) {
			return account.Identity{Email: "a@example.com", RealUserID: "u1"}, nil
		},
		Web:   &fakeWeb{deviceID: "574786429807779842"},
		Now:   func() time.Time { return time.Unix(1763000000, 0) },
		NewID: func() string { return "acc_1" },
	}
}

func TestTheChainSignsInAndStores(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)
	c.Keeper = func(password string) (string, error) { return "cipher", nil }

	res, err := c.Import(context.Background(), "a@example.com", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.AccountID != "acc_1" {
		t.Fatalf("res = %+v", res)
	}
	if len(st.accounts) != 1 || st.accounts[0] != "acc_1:a@example.com" {
		t.Fatalf("accounts = %v", st.accounts)
	}
	if len(st.tokens) != 1 || st.tokens[0] != "acc_1:ref" {
		t.Fatalf("tokens = %v", st.tokens)
	}
	if len(st.password) != 1 || st.password[0] != "acc_1:cipher" {
		t.Fatalf("passwords = %v", st.password)
	}
}

func TestTheChainRunsTheWebStepsInOrder(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)
	web := c.Web.(*fakeWeb)

	if _, err := c.Import(context.Background(), "a@example.com", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if len(web.probed) != 1 || web.probed[0] != "a@example.com" {
		t.Fatalf("probed = %v", web.probed)
	}
	if len(web.signed) != 1 || web.signed[0] != "a@example.com" {
		t.Fatalf("signed = %v", web.signed)
	}
	if web.password != "hunter2" {
		t.Fatalf("password = %q", web.password)
	}
	if len(web.approved) != 1 || web.approved[0] != "ABCD" {
		t.Fatalf("approved = %v", web.approved)
	}
	if !strings.Contains(web.redirect, "user_code=ABCD") {
		t.Fatalf("redirect = %q", web.redirect)
	}
}

func TestTheChainDoesNotKeepThePasswordWithoutAKey(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)

	if _, err := c.Import(context.Background(), "a@example.com", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if len(st.password) != 0 {
		t.Fatalf("the password was kept: %v", st.password)
	}
}

func TestAWrongPasswordSaysSoAndIsNotTriedAgain(t *testing.T) {
	c := &Chain{}
	err := c.webError(StagePassword, &webauth.SiteError{Code: 1200046})

	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
	if pe.Code != 1200046 || pe.Retry {
		t.Fatalf("err = %+v", pe)
	}
	if !strings.Contains(pe.Message, "密码") {
		t.Fatalf("message = %q", pe.Message)
	}
}

func TestARefusalWithoutACodeIsStillExplained(t *testing.T) {
	c := &Chain{}
	err := c.webError(StageApprove, &webauth.SiteError{Reason: "login_required"})

	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
	if pe.Message == "" || !strings.Contains(pe.Message, "登录") {
		t.Fatalf("message = %q", pe.Message)
	}
}

func TestAMissingDefaultGroupIsNotTriedAgain(t *testing.T) {
	c := &Chain{
		Start: func(ctx context.Context) (*oauth.Device, error) {
			return &oauth.Device{UserCode: "ABCD"}, nil
		},
		Web: &fakeWeb{deviceID: "d1", approveErr: webauth.ErrNoDefaultGroup},
	}
	_, err := c.SignIn(context.Background(), "a@example.com", "hunter2")
	if err == nil {
		t.Fatal("no error")
	}
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
	if pe.Stage != StageApprove || pe.Retry {
		t.Fatalf("err = %+v", pe)
	}
}

func TestAStaleLoginIsTriedAgain(t *testing.T) {
	c := &Chain{}
	err := c.webError(StagePassword, &webauth.SiteError{Code: 1200007})

	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
	if !pe.Retry {
		t.Fatalf("err = %+v", pe)
	}
}

func TestALostConnectionIsTriedAgain(t *testing.T) {
	c := &Chain{}
	err := c.webError(StageProbe, errors.New("connection reset"))

	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
	if !pe.Retry || pe.Wait <= 0 {
		t.Fatalf("err = %+v", pe)
	}
}

func TestAnAccountWithoutATokenIsTakenBackOut(t *testing.T) {
	st := &fakeStore{failToken: true}
	c := aChain(st)

	_, err := c.Import(context.Background(), "a@example.com", "hunter2")
	if err == nil {
		t.Fatal("no error")
	}
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
	if pe.Stage != StageStore {
		t.Fatalf("err = %+v", pe)
	}
	if len(st.accounts) != 0 {
		t.Fatalf("the broken account was left behind: %v", st.accounts)
	}
}
```

- [ ] **Step 2: 跑测试，确认它失败**

Run: `go test ./internal/provision/ -count=1 -run "Chain"`
Expected: FAIL（`Chain` 还没有）

- [ ] **Step 3: 写实现**

创建 `internal/provision/chain.go`：

整个文件写成这样（下面是它的全文）：

```go
package provision

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"time"

	"mmxcode/internal/account"
	"mmxcode/internal/oauth"
	"mmxcode/internal/site"
	"mmxcode/internal/store"
	"mmxcode/internal/webauth"
)

// Store is where an imported account goes.
type Store interface {
	CreateAccount(ctx context.Context, a store.Account) error
	SaveToken(ctx context.Context, t store.Token) error
	SetPassword(ctx context.Context, id, cipher string, updatedAt time.Time) error
}

// Web is the part of the account site the chain talks to.
//
// It is an interface so that a test can bring an account in without a
// network: the real one is a webauth.Client, and the session it keeps
// belongs to one account.
type Web interface {
	RegisterDevice(ctx context.Context) (string, error)
	Probe(ctx context.Context, email, deviceID, redirect string) error
	Password(ctx context.Context, email, password, deviceID, redirect string) error
	Approve(ctx context.Context, userCode string) error
}

// The real ones fit the interfaces, so that a caller can put them in
// without an adapter.
var (
	_ Web   = (*webauth.Client)(nil)
	_ Store = (*store.Store)(nil)
)

// Chain is the importer that does the real work.
//
// It signs in to the account site with the email and the password,
// uses that session to approve a device code, and puts the account in
// the database.
type Chain struct {
	// Store is where the account goes.
	Store Store
	// Web signs in to the account site. It is one per account: the
	// site keeps the session in a cookie, and two accounts sharing one
	// session would be signing in as each other.
	Web Web
	// Start begins a device authorization.
	Start func(ctx context.Context) (*oauth.Device, error)
	// Poll waits for the sign-in to finish.
	Poll func(ctx context.Context, d *oauth.Device) (*oauth.Token, error)
	// Identity reads who the account is. A nil one means the account's
	// email is the one from the list and its user id is the token's
	// subject.
	Identity func(ctx context.Context, access, subject string) (account.Identity, error)
	// Keeper encrypts the password so that it can be kept. A nil one
	// means the password is not kept: it is used once and thrown away.
	Keeper func(password string) (string, error)
	// Now is the clock.
	Now func() time.Time
	// NewID makes an account id.
	NewID func() string
}

// SignIn signs one account in and gives back the token.
//
// The steps are the ones the design document lists: a device code, a
// sign-in, an approval, and then the token.
//
// An account that is already in the database only needs the token; an
// account that is new also needs a row of its own. Import does both,
// SignIn only the first.
func (c *Chain) SignIn(ctx context.Context, email, password string) (*oauth.Token, error) {
	if c.Web == nil {
		return nil, &Error{Stage: StageProbe, Message: "没有站点可说话"}
	}
	if c.Start == nil {
		return nil, &Error{Stage: StageToken, Message: "没有设备码可等"}
	}
	d, err := c.Start(ctx)
	if err != nil {
		return nil, &Error{Stage: StageToken, Message: err.Error(), Retry: true, Wait: 5 * time.Second}
	}
	deviceID, err := c.Web.RegisterDevice(ctx)
	if err != nil {
		return nil, &Error{Stage: StageProbe, Message: err.Error(), Retry: true, Wait: 5 * time.Second}
	}
	// The page the sign-in sends the browser back to is the page that
	// shows the code. The site builds this path itself when it says a
	// login is needed.
	redirect := "/oauth-authorize?client_id=" + url.QueryEscape(site.ClientID) +
		"&user_code=" + url.QueryEscape(d.UserCode)

	if err := c.Web.Probe(ctx, email, deviceID, redirect); err != nil {
		return nil, c.webError(StageProbe, err)
	}
	if err := c.Web.Password(ctx, email, password, deviceID, redirect); err != nil {
		return nil, c.webError(StagePassword, err)
	}
	if err := c.Web.Approve(ctx, d.UserCode); err != nil {
		if errors.Is(err, webauth.ErrNoDefaultGroup) {
			// 站点没有默认组可批准，重试多少次都一样。
			return nil, &Error{Stage: StageApprove, Message: err.Error()}
		}
		return nil, c.webError(StageApprove, err)
	}
	if c.Poll == nil {
		return nil, &Error{Stage: StageToken, Message: "没有地方等这个令牌"}
	}
	tok, err := c.Poll(ctx, d)
	if err != nil {
		return nil, &Error{Stage: StageToken, Message: err.Error()}
	}
	return tok, nil
}

// Import signs one account in and puts it in the database.
func (c *Chain) Import(ctx context.Context, email, password string) (Result, error) {
	tok, err := c.SignIn(ctx, email, password)
	if err != nil {
		return Result{Email: email}, err
	}
	if c.Store == nil {
		return Result{Email: email}, &Error{Stage: StageStore, Message: "没有数据库可放"}
	}

	id := account.Identity{}
	if c.Identity != nil {
		got, err := c.Identity(ctx, tok.Access, tok.Subject)
		if err == nil {
			id = got
		}
	}
	now := c.now()
	name := id.Email
	if name == "" {
		name = email
	}
	userID := id.RealUserID
	if userID == "" {
		userID = tok.Subject
	}
	acc := store.Account{
		ID:        c.newID(),
		Name:      name,
		UserID:    userID,
		Email:     id.Email,
		Status:    store.StatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := c.Store.CreateAccount(ctx, acc); err != nil {
		return Result{Email: email}, &Error{Stage: StageStore, Message: err.Error()}
	}
	if err := c.Store.SaveToken(ctx, store.Token{
		AccountID: acc.ID,
		Access:    tok.Access,
		Refresh:   tok.Refresh,
		ExpiresAt: tok.Expires,
		Subject:   tok.Subject,
		UpdatedAt: now,
	}); err != nil {
		// 账号的行已经写进去了，令牌却没有：这样的账号是残的，
		// 把它拿掉，导入才能干净地再来一次。
		c.undo(ctx, acc.ID)
		return Result{Email: email}, &Error{Stage: StageStore, Message: err.Error()}
	}
	if c.Keeper != nil {
		cipher, err := c.Keeper(password)
		if err != nil {
			// The account is in; only the password is lost. Say so,
			// but do not call the import a failure.
			return Result{Email: email, OK: true, AccountID: acc.ID,
				Stage: StageStore, Message: "密码没能加密存下：" + err.Error()}, nil
		}
		if err := c.Store.SetPassword(ctx, acc.ID, cipher, now); err != nil {
			return Result{Email: email, OK: true, AccountID: acc.ID,
				Stage: StageStore, Message: "密码没能存下：" + err.Error()}, nil
		}
	}
	return Result{Email: email, OK: true, AccountID: acc.ID}, nil
}

// deleter is a store that can take an account back out.
//
// It is not part of the Store interface: taking the row back out is
// only possible with a store that has DeleteAccount, and the chain
// works without it.
type deleter interface {
	DeleteAccount(ctx context.Context, id string) error
}

// undo takes the account row back out.
//
// A store that cannot take the row out is left alone: the row stays
// where it is, broken but visible, and the message says what happened
// to it.
func (c *Chain) undo(ctx context.Context, id string) {
	d, ok := c.Store.(deleter)
	if !ok {
		return
	}
	_ = d.DeleteAccount(ctx, id)
}

// webError turns what the site said into what the runner needs to
// know: the stage, the site's own code, and whether trying again could
// work.
//
// A wrong password is not worth trying again. A dropped connection is.
func (c *Chain) webError(stage Stage, err error) error {
	var se *webauth.SiteError
	if errors.As(err, &se) {
		pe := &Error{
			Stage: stage,
			Code:  se.Code,
			// ExplainText 是一句话的说法：有错误码就用错误码，没有
			// 就用站点自己那个词（login_required 那种）。
			Message: se.ExplainText(),
		}
		if retryableCode(se.Code) {
			pe.Retry = true
			pe.Wait = 5 * time.Second
		}
		return pe
	}
	return &Error{Stage: stage, Message: err.Error(), Retry: true, Wait: 5 * time.Second}
}

// retryableCode says whether the site's own code is worth trying again.
//
// Most of them are not: a password that is wrong will be wrong the
// next time as well. The one that is different is the login that has
// gone stale -- a new sign-in may well work.
func retryableCode(code int) bool {
	switch code {
	case 1200007:
		// UNAUTHENTICATED: 登录状态无效，重试一次。
		return true
	}
	return false
}

// now is the clock.
func (c *Chain) now() time.Time {
	if c.Now == nil {
		return time.Now()
	}
	return c.Now()
}

// newID makes an account id.
//
// The time alone is not enough: two accounts that are brought in in the
// same millisecond get the same id, and the second one is refused by
// the primary key. So the id has a random part too.
func (c *Chain) newID() string {
	if c.NewID != nil {
		return c.NewID()
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// 没有随机数的时候只能用时间：撞车的机会很小，撞上了也
		// 只是这一个账号白做。
		return fmt.Sprintf("acc_%d", c.now().UnixMilli())
	}
	return "acc_" + hex.EncodeToString(b)
}
```

- [ ] **Step 4: 跑测试，确认它过了**

Run: `go test ./internal/provision/ -count=1`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/provision
git commit -m "feat(provision): 把登录、批准、换令牌、入库接成一条链

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 7: 命令行

**Files:**
- Create: `cmd/mmxcode/import.go`
- Create: `cmd/mmxcode/import_test.go`
- Modify: `cmd/mmxcode/root.go`

- [ ] **Step 1: 写失败的测试**

创建 `cmd/mmxcode/import_test.go`：

```go
package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mmxcode/internal/provision"
)

// aFakeEnv is an env whose batch does not touch the network.
func aFakeEnv(t *testing.T) *env {
	t.Helper()
	e := &env{}
	e.makeRunner = func(group string, interval time.Duration, keeper func(string) (string, error)) *provision.Runner {
		return &provision.Runner{
			Importer: &provision.Importer{
				Import: func(ctx context.Context, email, password string) (provision.Result, error) {
					if password != "pw" {
						return provision.Result{Email: email}, &provision.Error{
							Stage: provision.StagePassword, Code: 1200046, Message: "密码不对",
						}
					}
					return provision.Result{Email: email, OK: true, AccountID: "acc_1"}, nil
				},
			},
			Interval: interval,
			Sleep:    func(context.Context, time.Duration) error { return nil },
		}
	}
	return e
}

func TestImportReadsTheFileAndSaysWhatHappened(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.txt")
	if err := os.WriteFile(path, []byte("a@example.com|pw\nb@example.com|pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runImport(context.Background(), aFakeEnv(t), &out, path, "", time.Millisecond, false); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "a@example.com") || !strings.Contains(text, "b@example.com") {
		t.Fatalf("out = %q", text)
	}
	if !strings.Contains(text, "成功 2") {
		t.Fatalf("out = %q", text)
	}
}

func TestImportSaysALineItCouldNotRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.txt")
	// 清单里得有一个能用的号：光有坏行的话，整趟导入是空跑。
	if err := os.WriteFile(path, []byte("not-an-email\nb@example.com|pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runImport(context.Background(), aFakeEnv(t), &out, path, "", time.Millisecond, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "第 1 行") {
		t.Fatalf("out = %q", out.String())
	}
}

func TestImportSaysWhyAnAccountDidNotMakeIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.txt")
	if err := os.WriteFile(path, []byte("a@example.com|wrong\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runImport(context.Background(), aFakeEnv(t), &out, path, "", time.Millisecond, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "密码") {
		t.Fatalf("out = %q", out.String())
	}
}
```

- [ ] **Step 2: 跑测试，确认它失败**

Run: `go test ./cmd/mmxcode/ -count=1 -run Import`
Expected: FAIL

- [ ] **Step 3: 写实现**

在 `cmd/mmxcode/root.go` 的 `env` 结构里加一个口子，让测试不用真的上网：

```go
	// makeRunner makes the batch driver. It is a field so that a test
	// can run a batch without a network.
	makeRunner func(group string, interval time.Duration, keeper func(string) (string, error)) *provision.Runner
```

并把命令挂上（在 `newRoot` 里，`root.AddCommand(...)` 那一处）：

```go
	root.AddCommand(newImportCmd(e))
```

创建 `cmd/mmxcode/import.go`：

```go
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"mmxcode/internal/account"
	"mmxcode/internal/oauth"
	"mmxcode/internal/provision"
	"mmxcode/internal/secret"
	"mmxcode/internal/site"
	"mmxcode/internal/store"
	"mmxcode/internal/upstream"
	"mmxcode/internal/webauth"
)

// PasswordKeyEnv is the environment variable that holds the key the
// imported passwords are encrypted with.
//
// The key is not in the configuration file: the file and the database
// usually sit in the same volume, and a key next to the cipher text
// is no key at all.
const PasswordKeyEnv = "MMXCODE_PASSWORD_KEY"

func newImportCmd(e *env) *cobra.Command {
	var file string
	var group string
	var interval time.Duration
	var noStore bool
	cmd := &cobra.Command{
		Use:   "import",
		Short: "按清单批量登录账号",
		Long: "读一份每行「邮箱|密码」的清单，把这些账号自动登录进来。\n" +
			"密码登录之后默认加密存进库里（密钥在 " + PasswordKeyEnv + "），\n" +
			"没有这个环境变量就登录一次就丢。",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runImport(cmd.Context(), e, cmd.OutOrStdout(), file, group, interval, noStore)
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "账号清单（每行 邮箱|密码）")
	cmd.Flags().StringVar(&group, "group", "", "每个账号走哪个出口组")
	cmd.Flags().DurationVar(&interval, "interval", 3*time.Second, "每个账号之间的间隔")
	cmd.Flags().BoolVar(&noStore, "no-store-password", false, "登录成功就丢密码，不入库")
	_ = cmd.MarkFlagRequired("file")
	return cmd
}

// runImport brings in the accounts of a list.
func runImport(ctx context.Context, e *env, out io.Writer, file, group string, interval time.Duration, noStore bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	entries, bad := provision.Parse(strings.NewReader(string(raw)))
	for _, b := range bad {
		fmt.Fprintf(out, "第 %d 行：%s（%s）\n", b.Line, b.Reason, b.Text)
	}
	if len(entries) == 0 {
		return fmt.Errorf("清单里没有能用的账号")
	}

	keeper, err := passwordKeeper(noStore)
	if err != nil {
		return err
	}
	runner := e.runner(group, interval, keeper)

	runID := fmt.Sprintf("run_%d", time.Now().UnixMilli())
	if e.store != nil {
		if err := e.store.CreateImportRun(ctx, store.ImportRun{
			ID: runID, Source: "cli", StartedAt: time.Now(), Total: len(entries),
		}); err != nil {
			return err
		}
	}

	results, err := runner.Run(ctx, entries)
	if err != nil {
		return err
	}
	ok := 0
	for _, r := range results {
		if r.OK {
			ok++
			fmt.Fprintf(out, "  ✔ %s\t入库成功\n", r.Email)
			continue
		}
		fmt.Fprintf(out, "  ✘ %s\t%s：%s", r.Email, r.Stage, r.Message)
		if r.Code != 0 {
			fmt.Fprintf(out, "（%d）", r.Code)
		}
		fmt.Fprintln(out)
	}
	fmt.Fprintf(out, "\n共 %d 个，成功 %d，失败 %d。\n", len(results), ok, len(results)-ok)
	if keeper == nil {
		fmt.Fprintln(out, "密码没有存进库里（没有 "+PasswordKeyEnv+"）。")
	}
	if e.store != nil {
		if err := e.store.FinishImportRun(ctx, runID, time.Now(), ok, len(results)-ok, provision.ResultsJSON(results)); err != nil {
			return err
		}
	}
	return nil
}

// passwordKeeper gives the function that encrypts a password, or nil
// when the password is not to be kept.
//
// The key is not set: no key, no password kept. The key is set but
// wrong: that is a mistake, and the import stops on it — quietly
// throwing the password away would leave a database of accounts that
// nobody can sign in again.
func passwordKeeper(noStore bool) (func(string) (string, error), error) {
	if noStore {
		return nil, nil
	}
	if strings.TrimSpace(os.Getenv(PasswordKeyEnv)) == "" {
		return nil, nil
	}
	key, err := secret.FromEnv(PasswordKeyEnv)
	if err != nil {
		return nil, err
	}
	return key.Seal, nil
}

// runner makes the batch driver.
func (e *env) runner(group string, interval time.Duration, keeper func(string) (string, error)) *provision.Runner {
	if e.makeRunner != nil {
		return e.makeRunner(group, interval, keeper)
	}
	return &provision.Runner{
		Importer: &provision.Importer{
			Import: func(ctx context.Context, email, password string) (provision.Result, error) {
				chain := e.chain(group, keeper)
				return chain.Import(ctx, email, password)
			},
		},
		Interval: interval,
	}
}

// chain makes the thing that does the real work.
//
// The group says which way out the traffic goes: an empty group is the
// global default. The keeper encrypts the password so that it can be
// kept; a nil one throws the password away after the sign-in.
//
// Every call makes a new chain, and a new chain has a new web client:
// the site keeps the session in a cookie, and two accounts sharing one
// session would be signing in as each other.
func (e *env) chain(group string, keeper func(string) (string, error)) *provision.Chain {
	proxyURL := e.egr.Resolve(store.Account{ProxyGroup: group})
	up := e.up
	hc := &http.Client{Timeout: 30 * time.Second}
	if proxyURL != "" {
		if tr, err := e.egr.Transport(proxyURL); err == nil {
			up = upstream.New(upstream.Options{
				Timeout:   e.cfg.Upstream.Timeout.Duration(),
				Transport: tr,
			})
		}
		if c, err := e.egr.Client(proxyURL); err == nil {
			hc = c
		}
	}
	web := webauth.New(webauth.Options{
		BaseURL: site.AccountHost,
		HTTP:    hc,
		Lang:    site.Lang,
	})
	return &provision.Chain{
		Store: e.store,
		Web:   web,
		Start: func(ctx context.Context) (*oauth.Device, error) {
			return oauth.StartDevice(ctx, up, site.AccountURL(site.DeviceCodePath))
		},
		Poll: func(ctx context.Context, d *oauth.Device) (*oauth.Token, error) {
			return d.Poll(ctx, up, site.AccountURL(site.TokenPath))
		},
		Identity: func(ctx context.Context, access, subject string) (account.Identity, error) {
			return e.acc.Identity(ctx, access, subject)
		},
		Keeper: keeper,
		Now:    time.Now,
		NewID:  newBatchAccountID,
	}
}

// newBatchAccountID makes an account id for a batch import.
//
// The time alone is not enough: a batch signs in many accounts in a
// row, and two of them can land in the same millisecond.
func newBatchAccountID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("acc_%d", time.Now().UnixMilli())
	}
	return fmt.Sprintf("acc_%d_%s", time.Now().UnixMilli(), hex.EncodeToString(b))
}
```

- [ ] **Step 4: 跑测试，确认它过了**

Run: `go test ./cmd/mmxcode/ -count=1`
Expected: PASS

- [ ] **Step 5: 跑整个测试套件**

Run: `go build ./... && go vet ./... && gofmt -l . && go test ./... -count=1`
Expected: 全绿

- [ ] **Step 6: 提交**

```bash
git add cmd/mmxcode
git commit -m "feat(cli): import 子命令，按清单批量入号

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 8: 账号掉线了自动重登

**Files:**
- Modify: `internal/manager/manager.go`
- Modify: `internal/manager/manager_test.go`
- Modify: `internal/scheduler/scheduler.go`
- Modify: `internal/scheduler/scheduler_test.go`
- Modify: `cmd/mmxcode/import.go`
- Modify: `cmd/mmxcode/root.go`

- [ ] **Step 1: 写失败的测试**

在 `internal/manager/manager_test.go` 末尾加（并 import `"bytes"` 与 `"mmxcode/internal/secret"`）：

```go
func TestReloginSignsTheAccountInAgain(t *testing.T) {
	// 账号签出了，而且它有密码：拿密码重新登一次，状态回到 active。
	m := newTestManager(t, nil)
	ctx := context.Background()
	now := time.Now()
	key := secret.Key(bytes.Repeat([]byte{7}, secret.KeySize))
	cipher, err := key.Seal("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	m.Key = key
	if err := m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_1", Name: "a", Email: "a@example.com",
		Status: store.StatusSignedOut, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.SetPassword(ctx, "acc_1", cipher, now); err != nil {
		t.Fatal(err)
	}
	m.ReloginFn = func(ctx context.Context, acc store.Account, password string) error {
		if password != "hunter2" {
			t.Fatalf("password = %q", password)
		}
		return m.Store.SaveToken(ctx, store.Token{
			AccountID: "acc_1", Access: "new-access", Refresh: "new-refresh",
			ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
		})
	}
	if err := m.Relogin(ctx, "acc_1"); err != nil {
		t.Fatal(err)
	}
	acc, err := m.Store.GetAccount(ctx, "acc_1")
	if err != nil {
		t.Fatal(err)
	}
	if acc.Status != store.StatusActive {
		t.Fatalf("status = %q", acc.Status)
	}
}

func TestReloginWithoutAPasswordSaysSo(t *testing.T) {
	m := newTestManager(t, nil)
	ctx := context.Background()
	now := time.Now()
	if err := m.Store.CreateAccount(ctx, store.Account{
		ID: "acc_1", Name: "a", Status: store.StatusSignedOut, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Relogin(ctx, "acc_1"); err == nil {
		t.Fatal("no error for an account without a password")
	}
}

func TestReloginAllOnlyTouchesTheAccountsThatHaveAPassword(t *testing.T) {
	m := newTestManager(t, nil)
	ctx := context.Background()
	now := time.Now()
	key := secret.Key(bytes.Repeat([]byte{7}, secret.KeySize))
	m.Key = key
	cipher, err := key.Seal("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"acc_1", "acc_2"} {
		if err := m.Store.CreateAccount(ctx, store.Account{
			ID: id, Name: id, Status: store.StatusSignedOut, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Store.SetPassword(ctx, "acc_1", cipher, now); err != nil {
		t.Fatal(err)
	}
	m.ReloginFn = func(ctx context.Context, acc store.Account, password string) error {
		return m.Store.SaveToken(ctx, store.Token{
			AccountID: acc.ID, Access: "a", Refresh: "r",
			ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
		})
	}
	fixed, failed := m.ReloginAll(ctx, 10)
	if fixed != 1 || failed != 0 {
		t.Fatalf("fixed = %d, failed = %d", fixed, failed)
	}
	acc, err := m.Store.GetAccount(ctx, "acc_2")
	if err != nil {
		t.Fatal(err)
	}
	if acc.Status != store.StatusSignedOut {
		t.Fatalf("the account without a password was touched: %q", acc.Status)
	}
}
```

在 `internal/scheduler/scheduler_test.go` 末尾加（并 import `"bytes"` 与 `"mmxcode/internal/secret"`）：

```go
func TestTheReloginJobSignsTheSignedOutAccountsInAgain(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := s.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusSignedOut,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	key := secret.Key(bytes.Repeat([]byte{7}, secret.KeySize))
	cipher, err := key.Seal("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPassword(ctx, "acc_01", cipher, now); err != nil {
		t.Fatal(err)
	}
	up := upstream.New(upstream.Options{Timeout: 5 * time.Second})
	acc := account.New(up)
	mgr := manager.New(s, up, acc, egress.New(nil, ""), oauth.NewRenewer())
	mgr.Key = key
	mgr.ReloginFn = func(ctx context.Context, a store.Account, password string) error {
		return s.SaveToken(ctx, store.Token{
			AccountID: a.ID, Access: "a", Refresh: "r",
			ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
		})
	}

	sc := New(mgr, config.Default())
	sc.Relogin(ctx)

	got, err := s.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.StatusActive {
		t.Fatalf("status = %q", got.Status)
	}
}
```

- [ ] **Step 2: 跑测试，确认它失败**

Run: `go test ./internal/manager/ ./internal/scheduler/ -count=1 -run Relogin`
Expected: FAIL

- [ ] **Step 3: 写实现**

先在 `internal/config/config.go` 里加一节 `Batch`——调度器要读它，没有
这一节这一步编不过。（Task 10 只剩那些 yaml 文件了。）

```go
// Batch is how a batch import runs.
type Batch struct {
	// Interval is the wait between two accounts.
	Interval Duration `yaml:"interval" json:"interval"`
	// Group is the egress group the logins go through. An empty one
	// is the default proxy.
	Group string `yaml:"group" json:"group"`
	// Relogin is the automatic signing-in again.
	Relogin Relogin `yaml:"relogin" json:"relogin"`
}

// Relogin is the automatic signing-in again.
type Relogin struct {
	// Enabled says whether it runs at all. It is off by default:
	// signing in again is the site's business, and a person should
	// say so.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Cooldown is how long the scheduler waits between two rounds.
	Cooldown Duration `yaml:"cooldown" json:"cooldown"`
	// MaxAccounts is how many accounts one round touches.
	MaxAccounts int `yaml:"max_accounts" json:"max_accounts"`
}
```

在 `Config` 里加 `Batch Batch \`yaml:"batch" json:"batch"\``，在 `Default()` 里给它默认值：

```go
		Batch: Batch{
			Interval: Duration(3 * time.Second),
			Relogin: Relogin{
				Cooldown:    Duration(time.Hour),
				MaxAccounts: 20,
			},
		},
```

在 `internal/config/config_test.go` 末尾加一个测试（照着那个包里已有的测
试写，临时配置文件怎么建就照着建）：

```go
func TestTheBatchSection(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
batch:
  interval: 5s
  group: clash
  relogin:
    enabled: true
    cooldown: 2h
    max_accounts: 30
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Batch.Interval.Duration() != 5*time.Second {
		t.Errorf("interval = %v", cfg.Batch.Interval)
	}
	if cfg.Batch.Group != "clash" {
		t.Errorf("group = %q", cfg.Batch.Group)
	}
	if !cfg.Batch.Relogin.Enabled {
		t.Error("the relogin is not on")
	}
	if cfg.Batch.Relogin.Cooldown.Duration() != 2*time.Hour {
		t.Errorf("cooldown = %v", cfg.Batch.Relogin.Cooldown)
	}
	if cfg.Batch.Relogin.MaxAccounts != 30 {
		t.Errorf("max accounts = %d", cfg.Batch.Relogin.MaxAccounts)
	}
}
```

然后在 `internal/manager/manager.go` 的 `Manager` 结构里加两个字段：

```go
	// Key opens the passwords the accounts were brought in with.
	//
	// It is empty when there is no key: then an account cannot be
	// signed in again, and saying so is better than pretending.
	Key secret.Key
	// ReloginFn signs an account in again. It is a field so that a
	// test can stand in for the network.
	ReloginFn ReloginFunc
```

并在文件里加类型与两个方法：

```go
// ReloginFunc signs an account in again with the password it was
// brought in with.
type ReloginFunc func(ctx context.Context, acc store.Account, password string) error

// Relogin signs an account in again with the password it was brought
// in with.
//
// An account without a password cannot be signed in again: there is
// nothing to sign in with. That is an error, not a silent skip — the
// person who asked wants to know.
func (m *Manager) Relogin(ctx context.Context, accountID string) error {
	acc, err := m.Store.GetAccount(ctx, accountID)
	if err != nil {
		return err
	}
	cipher, err := m.Store.Password(ctx, accountID)
	if err != nil {
		return err
	}
	if cipher == "" {
		return errors.New("这个账号没有存密码，没法自己登回去")
	}
	password, err := m.Key.Open(cipher)
	if err != nil {
		return err
	}
	if m.ReloginFn == nil {
		return errors.New("没有地方能重新登录")
	}
	if err := m.ReloginFn(ctx, acc, password); err != nil {
		if mErr := m.markSignedOut(ctx, accountID, err.Error()); mErr != nil {
			return errors.Join(err, fmt.Errorf("the sign-out could not be saved: %w", mErr))
		}
		return err
	}
	return m.Store.UpdateAccountStatus(ctx, accountID, store.StatusActive, "", time.Now())
}

// ReloginAll signs in again every account that is signed out and has
// a password.
//
// It gives back how many it fixed and how many it did not. The limit
// says how many one round touches; zero means all of them.
func (m *Manager) ReloginAll(ctx context.Context, limit int) (fixed, failed int) {
	accounts, err := m.Store.ListAccounts(ctx)
	if err != nil {
		return 0, 0
	}
	for _, acc := range accounts {
		if acc.Status != store.StatusSignedOut {
			continue
		}
		if limit > 0 && fixed+failed >= limit {
			break
		}
		cipher, err := m.Store.Password(ctx, acc.ID)
		if err != nil || cipher == "" {
			continue
		}
		if err := m.Relogin(ctx, acc.ID); err != nil {
			failed++
			continue
		}
		fixed++
	}
	return fixed, failed
}
```

（`errors`、`fmt`、`time` 这几个包 `manager.go` 里多半已经有了；没有就 import。`secret` 要新 import。）

在 `internal/scheduler/scheduler.go` 里加：

```go
// Relogin signs in again the accounts that are signed out.
//
// It runs on its own clock: the cooldown is how long the scheduler
// waits between two rounds, so a sign-in that failed is not retried
// every minute.
func (s *Scheduler) Relogin(ctx context.Context) {
	fixed, failed := s.Mgr.ReloginAll(ctx, s.Cfg.Batch.Relogin.MaxAccounts)
	if fixed > 0 || failed > 0 {
		fmt.Printf("relogin: %d fixed, %d failed\n", fixed, failed)
	}
}
```

并在 `Run` 里加一条（`Batch` 这一节是 Task 10 加的；这一步先按 Task 10 的形状写好）：

```go
	if s.Cfg.Batch.Relogin.Enabled {
		every := s.Cfg.Batch.Relogin.Cooldown.Duration()
		if every <= 0 {
			every = time.Hour
		}
		go s.loop(ctx, every, s.Relogin)
	}
```

最后是接线：manager 得知道怎么重新登录。在 `cmd/mmxcode/import.go` 里加：

```go
// reloginFn makes the function that signs an account in again.
//
// The account is already in the database: only the token is new. The
// way out is the account's own proxy group, so that the sign-in comes
// from where the account usually comes from.
func (e *env) reloginFn() manager.ReloginFunc {
	return func(ctx context.Context, acc store.Account, password string) error {
		chain := e.chain(acc.ProxyGroup, nil)
		tok, err := chain.SignIn(ctx, acc.Email, password)
		if err != nil {
			return err
		}
		return e.store.SaveToken(ctx, store.Token{
			AccountID: acc.ID,
			Access:    tok.Access,
			Refresh:   tok.Refresh,
			ExpiresAt: tok.Expires,
			Subject:   tok.Subject,
			UpdatedAt: time.Now(),
		})
	}
}
```

在 `cmd/mmxcode/root.go` 的 `PersistentPreRunE` 里，manager 造好之后加：

```go
	// 重登用的密钥：没有它，账号掉线了就只能自己再登一次。
	if key, err := secret.FromEnv(PasswordKeyEnv); err == nil {
		e.mgr.Key = key
	}
	e.mgr.ReloginFn = e.reloginFn()
```

- [ ] **Step 4: 跑测试，确认它过了**

Run: `go test ./internal/manager/ ./internal/scheduler/ ./cmd/mmxcode/ -count=1`
Expected: PASS

- [ ] **Step 5: 跑整个测试套件**

Run: `go build ./... && go vet ./... && gofmt -l . && go test ./... -count=1`
Expected: 全绿

- [ ] **Step 6: 提交**

```bash
git add internal/manager internal/scheduler cmd/mmxcode
git commit -m "feat(manager): 账号签出了就用存着的密码自己登回去

重登放在调度器里，不动续期器那条热路径：续期器照旧把链断的账号
标成 signed_out，调度器过一阵子再把有密码的那些登回来。

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 9: WebUI：导入页与重登

**Files:**
- Modify: `internal/webui/webui.go`
- Modify: `internal/webui/webui_test.go`
- Create: `internal/webui/templates/import.html`
- Modify: `internal/webui/static/app.js`
- Modify: `cmd/mmxcode/serve.go`

- [ ] **Step 1: 写失败的测试**

在 `internal/webui/webui_test.go` 里加一个能换配置的 helper（把现有的 `newTestWebUI` 改成调它），并加测试：

```go
// newTestWebUIWithConfig makes a WebUI with a given configuration.
//
// The import page is only there when no API key is set, so a test that
// wants to see it there needs a configuration of its own.
func newTestWebUIWithConfig(t *testing.T, cfg config.Config) (*WebUI, *store.Store) {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	now := time.Now()
	s.CreateAccount(context.Background(), store.Account{
		ID: "acc_01", Name: "主号", Email: "a@example.com", Status: store.StatusActive,
		CreatedAt: now, UpdatedAt: now,
	})
	up := upstream.New(upstream.Options{Timeout: 5 * time.Second})
	groups := map[string]string{"warp": "socks5h://warp:4000"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no upstream in the test", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	acc := account.New(up)
	acc.SetBase(srv.URL, srv.URL)
	mgr := manager.New(s, up, acc, egress.New(groups, ""), oauth.NewRenewer())
	cfg.Proxy.Groups = groups
	w := New(mgr, cfg, s)
	w.Import = func(ctx context.Context, entries []provision.Entry) ([]provision.Result, error) {
		out := make([]provision.Result, 0, len(entries))
		for _, e := range entries {
			out = append(out, provision.Result{Email: e.Email, OK: true, AccountID: "acc_02"})
		}
		return out, nil
	}
	w.Relogin = func(ctx context.Context) (int, int) { return 1, 0 }
	return w, s
}

func newTestWebUI(t *testing.T) (*WebUI, *store.Store) {
	t.Helper()
	return newTestWebUIWithConfig(t, config.Default())
}
```

（把原来那个 `newTestWebUI` 的身子换成上面这个。`provision` 要 import。）

然后加测试：

```go
func TestTheImportPageIsThereWhenNoAPIKeysAreSet(t *testing.T) {
	w, _ := newTestWebUIWithConfig(t, config.Default())
	req := httptest.NewRequest(http.MethodGet, "/import", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "批量导入") {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestTheImportPageIsNotThereWhenAPIKeysAreSet(t *testing.T) {
	cfg := config.Default()
	cfg.Server.APIKeys = []string{"sk-x"}
	w, _ := newTestWebUIWithConfig(t, cfg)
	req := httptest.NewRequest(http.MethodGet, "/import", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatal("the import page is open with API keys set")
	}
}

func TestUploadingAFileAnswersWithAJobID(t *testing.T) {
	w, _ := newTestWebUIWithConfig(t, config.Default())
	body := &bytes.Buffer{}
	part := multipart.NewWriter(body)
	fw, err := part.CreateFormFile("file", "accounts.txt")
	if err != nil {
		t.Fatal(err)
	}
	fw.Write([]byte("a@example.com|pw\n"))
	part.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/import", body)
	req.Header.Set("Content-Type", part.FormDataContentType())
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d, body = %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "\"id\"") {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestTheReloginIsAskedFor(t *testing.T) {
	w, _ := newTestWebUIWithConfig(t, config.Default())
	req := httptest.NewRequest(http.MethodPost, "/api/relogin", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "fixed") {
		t.Fatalf("body = %q", rec.Body.String())
	}
}
```

（`bytes`、`mime/multipart` 要 import。）

- [ ] **Step 2: 跑测试，确认它失败**

Run: `go test ./internal/webui/ -count=1 -run "Import|Relogin"`
Expected: FAIL

- [ ] **Step 3: 写实现**

在 `internal/webui/webui.go` 里：

1. `WebUI` 结构加两个字段：

```go
	// Import brings a batch of accounts in. It is a field so that the
	// page and the command share one task, and so that a test can
	// stand in for the network.
	Import func(ctx context.Context, entries []provision.Entry) ([]provision.Result, error)
	// Relogin signs the accounts that are signed out in again.
	Relogin func(ctx context.Context) (fixed, failed int)
```

2. 路由。在 `ServeHTTP` 的页面那一段里加：

```go
	case "/import":
		// 只有没配 key 的网关才开放上传：配了 key 就说明别的机器能
		// 进来，明文密码不该从那儿过。
		if len(w.Cfg.Server.APIKeys) > 0 {
			http.NotFound(rw, r)
			return
		}
		w.page(rw, r, "import.html", w.importData)
```

在 API 那一段里加：

```go
	case "/api/import":
		w.apiImport(rw, r)
		return
	case "/api/relogin":
		w.apiRelogin(rw, r)
		return
```

3. 三个处理函数：

```go
// importData is the import page's data.
func (w *WebUI) importData(ctx context.Context) (map[string]any, error) {
	runs, err := w.Store.ListImportRuns(ctx, 10)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"UploadOpen": len(w.Cfg.Server.APIKeys) == 0,
		"Runs":       runs,
	}, nil
}

// apiImport takes a list of accounts and brings them in.
//
// The list is a file: every line is an account. The work goes on in
// the background, and the answer says which run to watch.
func (w *WebUI) apiImport(rw http.ResponseWriter, r *http.Request) {
	if len(w.Cfg.Server.APIKeys) > 0 {
		http.Error(rw, "这个网关配了 API key，不开放上传", http.StatusForbidden)
		return
	}
	if w.Import == nil {
		http.Error(rw, "这个网关没有批量导入", http.StatusNotImplemented)
		return
	}
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		http.Error(rw, "这个请求不是文件上传", http.StatusBadRequest)
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		http.Error(rw, "没有文件", http.StatusBadRequest)
		return
	}
	defer file.Close()
	entries, bad := provision.Parse(file)
	for _, b := range bad {
		log.Printf("import: 第 %d 行：%s", b.Line, b.Reason)
	}
	if len(entries) == 0 {
		http.Error(rw, "清单里没有能用的账号", http.StatusBadRequest)
		return
	}
	runID := fmt.Sprintf("run_%d", time.Now().UnixMilli())
	now := time.Now()
	if err := w.Store.CreateImportRun(r.Context(), store.ImportRun{
		ID: runID, Source: "web", StartedAt: now, Total: len(entries),
	}); err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	go w.runImport(runID, entries)
	// writeJSON 在这个包里只有两个参数，自己不写状态码：要 202 就先写。
	rw.WriteHeader(http.StatusAccepted)
	writeJSON(rw, map[string]any{"id": runID})
}

// runImport brings a batch in and writes down what came of it.
func (w *WebUI) runImport(runID string, entries []provision.Entry) {
	ctx := context.Background()
	results, err := w.Import(ctx, entries)
	ok := 0
	for _, r := range results {
		if r.OK {
			ok++
		}
	}
	failed := len(results) - ok
	if err != nil {
		failed++
	}
	if err := w.Store.FinishImportRun(ctx, runID, time.Now(), ok, failed, provision.ResultsJSON(results)); err != nil {
		log.Printf("import %s: %v", runID, err)
	}
}

// apiRelogin signs the accounts that are signed out in again.
func (w *WebUI) apiRelogin(rw http.ResponseWriter, r *http.Request) {
	if w.Relogin == nil {
		http.Error(rw, "这个网关没有重登", http.StatusNotImplemented)
		return
	}
	fixed, failed := w.Relogin(r.Context())
	writeJSON(rw, map[string]any{"fixed": fixed, "failed": failed})
}
```

（`writeJSON` 这个包里多半已经有了；没有就照着别的 API 处理器的样子写一个。`log` 要 import `"log"`。）

4. 页面的模板 `internal/webui/templates/import.html`：

```html
{{define "content"}}
<section>
  <h1>批量导入</h1>
  {{if .UploadOpen}}
    <p class="muted">清单每行一个账号，写成 <code>邮箱|密码</code>；<code>#</code> 开头的是注释。</p>
    <form id="import-form" enctype="multipart/form-data">
      <input type="file" name="file" accept=".txt" required>
      <button type="submit">开始</button>
    </form>
    <p class="muted" id="import-status"></p>
    <table id="import-results" hidden>
      <thead><tr><th>账号</th><th>结果</th><th>说明</th></tr></thead>
      <tbody></tbody>
    </table>
  {{else}}
    <p class="muted">已经配了 API key，出于安全考虑这里不开放上传。请在服务器上跑
      <code>mmxcode import --file accounts.txt</code>。</p>
  {{end}}
  {{if .Runs}}
  <h2>最近的导入</h2>
  <table>
    <thead><tr><th>时间</th><th>来源</th><th>成功</th><th>失败</th></tr></thead>
    <tbody>
      {{range .Runs}}
      <tr>
        <td>{{.StartedAt.Format "2006-01-02 15:04:05"}}</td>
        <td>{{.Source}}</td>
        <td class="good">{{.OK}}</td>
        <td class="bad">{{.Failed}}</td>
      </tr>
      {{end}}
    </tbody>
  </table>
  {{end}}
</section>
{{end}}
```

5. `internal/webui/static/app.js` 里加一段（放在 `poll` 那个函数后面）：

```js
// 导入：把文件交上去，然后一直问那一次导入走到哪儿了。
//
// 上传回来的只有一次导入的编号，进度得自己问。
document.addEventListener('submit', async (ev) => {
  const form = ev.target.closest('#import-form');
  if (!form) return;
  ev.preventDefault();
  const status = document.getElementById('import-status');
  const input = form.querySelector('input[type=file]');
  if (!input || !input.files || !input.files[0]) return;
  status.textContent = '上传中……';
  const data = new FormData();
  data.append('file', input.files[0]);
  try {
    const res = await fetch('/api/import', { method: 'POST', body: data });
    const answer = await readAnswer(res);
    if (!answer.ok) throw new Error(messageOf(answer));
    status.textContent = '导入中……';
    watchImport(answer.json.id);
  } catch (e) {
    status.textContent = '失败：' + e.message;
  }
});

function watchImport(id) {
  const timer = setInterval(async () => {
    try {
      const res = await fetch('/api/import/' + encodeURIComponent(id));
      const answer = await readAnswer(res);
      if (!answer.ok) throw new Error(messageOf(answer));
      const run = answer.json;
      const status = document.getElementById('import-status');
      status.textContent = '导入中……成功 ' + run.ok + '，失败 ' + run.failed;
      if (run.finished_at) {
        clearInterval(timer);
        showImportResults(run);
      }
    } catch (e) {
      clearInterval(timer);
      const status = document.getElementById('import-status');
      if (status) status.textContent = '失败：' + e.message;
    }
  }, 3000);
}

function showImportResults(run) {
  const table = document.getElementById('import-results');
  if (!table) return;
  const body = table.querySelector('tbody');
  body.textContent = '';
  let results = [];
  try { results = JSON.parse(run.results || '[]'); } catch (e) { results = []; }
  for (const r of results) {
    const tr = document.createElement('tr');
    const cells = [r.email, r.ok ? '成功' : '失败', r.message || ''];
    for (const c of cells) {
      const td = document.createElement('td');
      td.textContent = c;
      tr.appendChild(td);
    }
    body.appendChild(tr);
  }
  table.hidden = false;
}
```

6. `GET /api/import/{id}` 的路由（在 `ServeHTTP` 里，`/api/accounts/` 那一段的旁边）：

```go
	if strings.HasPrefix(r.URL.Path, "/api/import/") {
		w.apiImportRun(rw, r)
		return
	}
```

```go
// apiImportRun says how one batch import is going.
func (w *WebUI) apiImportRun(rw http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/import/")
	run, err := w.Store.GetImportRun(r.Context(), id)
	if err != nil {
		http.Error(rw, "没有这一次导入", http.StatusNotFound)
		return
	}
	writeJSON(rw, http.StatusOK, map[string]any{
		"id":          run.ID,
		"source":      run.Source,
		"total":       run.Total,
		"ok":          run.OK,
		"failed":      run.Failed,
		"results":     run.Results,
		"finished_at": !run.FinishedAt.IsZero(),
	})
}
```

7. `cmd/mmxcode/serve.go` 里接线：

```go
			ui := webui.New(e.mgr, e.cfg, e.store)
			keeper, _ := passwordKeeper(false)
			ui.Import = func(ctx context.Context, entries []provision.Entry) ([]provision.Result, error) {
				runner := e.runner(e.cfg.Batch.Group, e.cfg.Batch.Interval.Duration(), keeper)
				return runner.Run(ctx, entries)
			}
			ui.Relogin = func(ctx context.Context) (int, int) {
				return e.mgr.ReloginAll(ctx, e.cfg.Batch.Relogin.MaxAccounts)
			}
```

（`provision` 要 import。）

- [ ] **Step 4: 跑测试，确认它过了**

Run: `go test ./internal/webui/ ./cmd/mmxcode/ -count=1`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/webui cmd/mmxcode
git commit -m "feat(webui): 批量导入页和重登页

上传只在没有配 API key 的时候开放：配了 key 就说明别的机器能进来，
明文密码不该从那儿过。

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 10: 配置与部署

**Files:**
- Modify: `config.example.yaml`
- Modify: `config.docker.yaml`
- Modify: `docker-compose.yml`
- Create: `.env.example`

（`internal/config/config.go` 里的 `Batch` 一节已经在 Task 8 加了——调
度器要读它，那一步没有它编不过。这一步只管那些 yaml 文件。）

- [ ] **Step 1: 看看现在是什么样**

Run: `cat config.example.yaml config.docker.yaml docker-compose.yml`
Expected: 看得见现在的样子。仓库里还没有 `.env.example`，这一步把它建出来。

- [ ] **Step 2: 写实现**

在 `internal/config/config.go` 里加：

```go
// Batch is how a batch import runs.
type Batch struct {
	// Interval is the wait between two accounts.
	Interval Duration `yaml:"interval" json:"interval"`
	// Group is the egress group the logins go through. An empty one
	// is the default proxy.
	Group string `yaml:"group" json:"group"`
	// Relogin is the automatic signing-in again.
	Relogin Relogin `yaml:"relogin" json:"relogin"`
}

// Relogin is the automatic signing-in again.
type Relogin struct {
	// Enabled says whether it runs at all. It is off by default:
	// signing in again is the site's business, and a person should
	// say so.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Cooldown is how long the scheduler waits between two rounds.
	Cooldown Duration `yaml:"cooldown" json:"cooldown"`
	// MaxAccounts is how many accounts one round touches.
	MaxAccounts int `yaml:"max_accounts" json:"max_accounts"`
}
```

在 `Config` 里加 `Batch Batch \`yaml:"batch" json:"batch"\``，在 `Default()` 里给它默认值：

```go
		Batch: Batch{
			Interval: Duration(3 * time.Second),
			Relogin: Relogin{
				Cooldown:    Duration(time.Hour),
				MaxAccounts: 20,
			},
		},
```

在 `config.example.yaml` 里加：

```yaml
batch:
  # 批量导入时每个账号之间的间隔
  interval: 3s
  # 批量导入走哪个出口组（空 = 全局默认出口）
  group: ""
  relogin:
    # 账号掉线了自动用存着的密码登回去（默认关）
    enabled: false
    # 调度器两趟之间隔多久
    cooldown: 1h
    # 一轮最多处理多少个账号
    max_accounts: 20
```

在 `docker-compose.yml` 的 `environment` 里加：

```yaml
      # 导入用的密码要加密存库，密钥在这里。不设 = 登录一次就丢。
      MMXCODE_PASSWORD_KEY: ${MMXCODE_PASSWORD_KEY:-}
```

在 `.env.example` 里加一行：

```
# 导入用的密码加密的密钥（base64 的 32 字节）。不设就不存密码。
MMXCODE_PASSWORD_KEY=
```

- [ ] **Step 4: 跑测试，确认它过了**

Run: `go test ./internal/config/ -count=1`
Expected: PASS

- [ ] **Step 5: 跑整个测试套件**

Run: `go build ./... && go vet ./... && gofmt -l . && go test ./... -count=1`
Expected: 全绿

- [ ] **Step 6: 提交**

```bash
git add internal/config config.example.yaml config.docker.yaml docker-compose.yml .env.example
git commit -m "feat(config): 批量导入与自动重登的配置

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 11: 收口

**Files:**
- Modify: `README.md`
- Modify: `docs/superpowers/specs/2026-11-14-mmxcode-batch-import-design.md`
- Modify: `docs/superpowers/plans/2026-11-14-mmxcode-batch-import.md`

- [ ] **Step 1: README 补上这一节**

在 README 的「命令」一节里加上：

```markdown
### 批量导入

```bash
# 清单每行一个账号：邮箱|密码，# 开头是注释
mmxcode import --file accounts.txt

# 每个账号走哪个出口组，每个账号之间隔多久
mmxcode import --file accounts.txt --group clash --interval 5s
```

密码登录之后会加密存进库里，密钥在 `MMXCODE_PASSWORD_KEY`（base64 的 32 字节）。
不设这个变量，密码登录一次就丢，账号掉线了就得自己再登一次。

账号掉线了可以自动登回去（默认关）：

```yaml
batch:
  relogin:
    enabled: true
```
```

- [ ] **Step 2: spec 补上「做完了什么样」**

在设计文档的末尾加一节，记下实现时和设计不一样的地方（如果有）。没有就不加。

- [ ] **Step 3: 把计划跟代码对起来**

改过代码之后，计划的整文件代码块要跟仓库里的文件逐字节相同。用：

```bash
python -I .tmp-review/sync_plan.py docs/superpowers/plans/2026-11-14-mmxcode-batch-import.md internal/webauth/webauth.go internal/webauth/rsa.go internal/provision/provision.go internal/provision/chain.go internal/secret/secret.go
python -I .tmp-review/check_plan_files.py docs/superpowers/plans/2026-11-14-mmxcode-batch-import.md
```

（`.tmp-review/` 是这个项目里用来对计划的工具目录，不进仓库。）

- [ ] **Step 4: 最后的检查**

```bash
go build ./... && go vet ./... && gofmt -l . && go test ./... -count=1
```

全绿，`gofmt -l` 没有输出。

- [ ] **Step 5: 提交**

```bash
git add README.md docs/
git commit -m "docs: 批量导入怎么用

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

## 待实测（只有人能做）

1. **拿真账号跑通整条链**：`mmxcode import --file` 一个真的账号，看它能不能登录成功、能不能入库
2. `statusInfo.code` 在「密码错」这件事上的**确切**码值（抓包笔记里只有常量名，没有对照表）
3. `group_id` 的默认组规则：真实账号上 `is_default` 的那组是不是唯一的
4. 大陆出口下 `account.minimax.io` 是否也被 307 到 `.cn`（笔记里只记了 `agent.minimax.io`）
5. Akamai 的 `_abck` / `bm_sz` 在批量登录时会不会真的拦
6. 批量到多少个号会触发风控
7. **抓一个真的网页请求存成金标准**：整条 URL 加 `yy` 加 `x-signature`。
   命令行那边的六个向量是照着参考实现重算出来的，网页这一边目前只是照
   着笔记里的散文推的——两边只有一边有金标准，这个不对称才是最大的风险。
   抓包笔记里已经有 `x-signature` 的那半个向量（`abd7da0095474425f28458b7b56a6626`
   能用 `md5(秒 + 密钥 + body)` 复现），`yy` 那一半缺的是完整 URL。
8. `op_ticket` 到底在不在：抓包笔记 §4 的公共 query 里没有它，是设计文档从
   js/204 里读出来的。它在 query 里就改了签名的字节
9. `uuid` 出现两次的时候，站点读的是哪一个。网页那边公共的那一份在前、自己
   的那一份在后，常见的解析器取第一个（`url.Values.Get`、`URLSearchParams.get`、
   Java 的 `getParameter`）——但这是猜的
10. 同一个账号导两次会不会在库里留下两行。`Chain.Import` 每回都新建一行，
    库里没有按邮箱找账号的法子
