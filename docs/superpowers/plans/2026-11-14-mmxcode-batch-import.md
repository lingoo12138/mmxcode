# 批量导入（邮箱|密码 → mcode 账号）实施计划

> **这份计划已经做完了。** 十一个任务都落地了（`git log` 里从 `86db5e1` 到
> `feature/batch-import` 的 HEAD），代码块跟仓库里的代码逐字对齐：
> `docs/superpowers/tools/` 里的四个脚本管这件事，`check_plan_files.py` 报
> `stale declarations: 0` 就是对齐了。设计文档末尾的 §16 记着实现时跟设计
> 不一样的地方。

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
4. **计划里的代码跟仓库里的代码逐字对齐。** 这个功能自己写的那些文件
   （`sign`、`secret`、`webauth`、`provision`、`import` 及其测试）在计划里是
   **整文件**带过来的，逐字节相同；只是改过的那些（`store`、`webui`、`serve`、
   `config`、`manager`）在计划里是**改的那一部分**，一个声明一个声明地对。
   改完代码用 `docs/superpowers/tools/` 里的脚本对一遍：
   `refresh_plan_blocks.py`（片段）、`sync_plan.py`（整文件）、
   `check_plan_files.py`（查）、`check_plan_fences.py`（查代码块里有没有混进
   散文）。
5. **所有测试都不能碰真站点。** 写测试时确认一下 `httptest.NewServer` 用的是本地地址。

---

### Task 1: 签名的「网页味」

**Files:**
- Modify: `internal/sign/sign.go`
- Modify: `internal/sign/sign_test.go`

- [x] **Step 1: 写失败的测试**

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

- [x] **Step 2: 跑测试，确认它失败**

Run: `go test ./internal/sign/ -count=1 -run "WebFlavour|MCodeFlavour"`
Expected: FAIL（`FlavorWeb` 还没有，编译就过不去）

- [x] **Step 3: 写实现**

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

- [x] **Step 4: 跑测试，确认它过了**

Run: `go test ./internal/sign/ -count=1`
Expected: PASS（全部，包括原有的向量测试）

- [x] **Step 5: 跑整个测试套件**

Run: `go build ./... && go vet ./... && gofmt -l . && go test ./... -count=1`
Expected: 全绿，`gofmt -l` 没有输出

- [x] **Step 6: 提交**

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

- [x] **Step 1: 写失败的测试**

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

- [x] **Step 2: 跑测试，确认它失败**

Run: `go test ./internal/secret/ -count=1`
Expected: FAIL（包还不存在）

- [x] **Step 3: 写实现**

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

- [x] **Step 4: 跑测试，确认它过了**

Run: `go test ./internal/secret/ -count=1`
Expected: PASS

- [x] **Step 5: 提交**

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

- [x] **Step 1: 写失败的测试**

在 `internal/store/store_test.go` 末尾加：

```go
// TestThePasswordGoesInAndComesBack says the encrypted password can be
// written and read back.
func TestThePasswordGoesInAndComesBack(t *testing.T) {
	s := openTest(t)
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

// TestAnAccountWithoutAPassword says an account that was not brought
// in by a batch import has no password, and that is not an error.
func TestAnAccountWithoutAPassword(t *testing.T) {
	s := openTest(t)
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

// TestTheImportRunIsKept says a batch import is written down with what
// came of it.
func TestTheImportRunIsKept(t *testing.T) {
	s := openTest(t)
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

// TestAnImportRunCanBeAskedForByItsName says one batch import can be
// taken out on its own.
//
// 页面上要看得见某一次导入走到哪儿了：光有"最近的几次"不够，
// 得能按名字把这一次导入单独取出来。
func TestAnImportRunCanBeAskedForByItsName(t *testing.T) {
	s := openTest(t)
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

- [x] **Step 2: 跑测试，确认它失败**

Run: `go test ./internal/store/ -count=1 -run "Password|ImportRun"`
Expected: FAIL（`SetPassword` / `ImportRun` 还没有）

- [x] **Step 3: 写实现**

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
// The cipher text is written on its own, so that the other setters --
// which write one column each -- do not have to know about it.
//
// An account that is not there is ErrNotFound: the caller believes it
// wrote a password, and writing nothing is not writing one.
func (s *Store) SetPassword(ctx context.Context, id, cipher string, updatedAt time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE accounts SET password=?, updated_at=? WHERE id=?`,
		cipher, ms(s.stamp(updatedAt)), id)
	if err != nil {
		return fmt.Errorf("set the password of the account %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("set the password of the account %s: %w", id, ErrNotFound)
	}
	return nil
}

// Password gives an account's encrypted password.
//
// An account that was not brought in by a batch import has none, and
// that is not an error: the text is empty.
//
// An account that is not there at all has no password either, and that
// is not an error either: the caller asked about a password, and there
// is none. A caller that cares whether the account is there asks for
// the account.
func (s *Store) Password(ctx context.Context, id string) (string, error) {
	var cipher sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT password FROM accounts WHERE id=?`, id).Scan(&cipher)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get the password of the account %s: %w", id, err)
	}
	return cipher.String, nil
}

// CreateImportRun records the start of a batch import.
//
// The run is written down before the accounts are made, so that an
// import that is still going can be seen.
func (s *Store) CreateImportRun(ctx context.Context, r ImportRun) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO import_runs (id, source, started_at, total, ok, failed)
		VALUES (?,?,?,?,?,?)`,
		r.ID, r.Source, ms(s.stamp(r.StartedAt)), r.Total, r.OK, r.Failed)
	if err != nil {
		return fmt.Errorf("create import run %s: %w", r.ID, err)
	}
	return nil
}

// FinishImportRun records what a batch import came to.
//
// The results are kept as they were given: they are the reader's to
// read, and the store does not have to know what is in them.
func (s *Store) FinishImportRun(ctx context.Context, id string, finishedAt time.Time, ok, failed int, results string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE import_runs SET finished_at=?, ok=?, failed=?, results=? WHERE id=?`,
		ms(s.stamp(finishedAt)), ok, failed, results, id)
	if err != nil {
		return fmt.Errorf("finish import run %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("finish import run %s: %w", id, ErrNotFound)
	}
	return nil
}

// GetImportRun gives one batch import by its name.
//
// A name that is not there is ErrNotFound: the caller asked about one
// particular import, and being handed a different one would be worse
// than being told there is none.
func (s *Store) GetImportRun(ctx context.Context, id string) (ImportRun, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, source, started_at, finished_at, total, ok, failed,
			COALESCE(results, '')
		FROM import_runs WHERE id=?`, id)
	r, err := scanImportRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ImportRun{}, fmt.Errorf("get import run %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return ImportRun{}, fmt.Errorf("get import run %s: %w", id, err)
	}
	return r, nil
}

// ListImportRuns gives the most recent batch imports.
//
// A limit of zero or less means the usual twenty: a limit is a limit,
// and a limit of zero is no limit at all, which is not what a reader
// of the page wants.
func (s *Store) ListImportRuns(ctx context.Context, limit int) ([]ImportRun, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, source, started_at, finished_at, total, ok, failed,
			COALESCE(results, '')
		FROM import_runs ORDER BY started_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list import runs: %w", err)
	}
	defer rows.Close()
	var out []ImportRun
	for rows.Next() {
		r, err := scanImportRun(rows)
		if err != nil {
			return nil, fmt.Errorf("list import runs: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list import runs: %w", err)
	}
	return out, nil
}

```

（`s.exec` 是这个包里现成的写库helper；名字不一样就看 `store.go` 里别的 setter 怎么写的，照着来。`sql.ErrNoRows` 需要 `"database/sql"` 这个 import。）

另外还要一个按邮箱找账号的方法：重跑一次导入的时候，靠它把原来那一
行找出来更新，而不是给同一个站点账号再来一行。不改表的结构，也不加
索引——表才几百行。

还要一个按令牌里的那个 sub 找账号的方法：那个 sub 是站点账号自己的
id，一个站点账号一个，重登多少次都是它。清单上写的地址可能是别名，站
点那边报的地址又可能一时问不出来，只有它是一直在的——去重靠它，才不会
在地址对不上的时候给同一个站点账号建出第二行。它顺着令牌那一行找。

```go
// AccountByEmail gives the account with that email.
//
// 邮箱是站点那边认的东西：同一个邮箱只该有一行。重跑一次导入是常事
// （上一批有一部分失败了，重来一遍是补救的办法），再来一行就是同一个
// 站点账号有两个活着的行，两个都在轮转池里，页面把它列两遍。
//
// 表不大——几百行——一次全表扫就够了，不值得为它改表的结构或者加个
// 索引。邮箱比的时候两边都折成小写：清单上写的是哪个样子，库里存的
// 是哪个样子，人看着是同一个就是同一个。
//
// 没有这一行是 ErrNotFound。
func (s *Store) AccountByEmail(ctx context.Context, email string) (Account, error) {
	return s.accountByEmail(ctx, s.db, email)
}

```

还要一个写事务：按令牌里的那个 sub 找一行、按邮箱找一行、没有就建一
行，这种「查了再做」的事要在一个事务里做完，不然两个并发的导入会各建一
行（同一个站点账号两行都是活的）。数据源名字里已经有
`_txlock=immediate`：事务一开始就拿着写锁，第二个进来的那个看见的是第
一个建好的那一行。不改表的结构，也不加索引。

```go
// Tx is what one transaction can do.
//
// It is the same set of writes the store has, but inside one
// transaction: they all land, or none of them does.
type Tx interface {
	// AccountByEmail gives the account with that email, when there is
	// one. It says ErrNotFound when there is none.
	AccountByEmail(ctx context.Context, email string) (Account, error)
	// AccountBySubject gives the account whose token carries that
	// subject, when there is one. It says ErrNotFound when there is
	// none.
	AccountBySubject(ctx context.Context, subject string) (Account, error)
	// AccountByUserID gives the account with that site user id, when
	// there is one. It says ErrNotFound when there is none.
	AccountByUserID(ctx context.Context, userID string) (Account, error)
	// CreateAccount inserts an account.
	CreateAccount(ctx context.Context, a Account) error
	// SaveToken writes an account's token.
	SaveToken(ctx context.Context, t Token) error
	// SetAccountIdentity writes an account's address and user id.
	SetAccountIdentity(ctx context.Context, id, email, userID string, updatedAt time.Time) error
	// SetAccountName writes the account's name.
	SetAccountName(ctx context.Context, id, name string, updatedAt time.Time) error
	// SetAccountProxy writes the account's outlet.
	SetAccountProxy(ctx context.Context, id, group, url string, updatedAt time.Time) error
	// UpdateAccountStatus writes the account's state and its last
	// error.
	UpdateAccountStatus(ctx context.Context, id, status, lastError string, updatedAt time.Time) error
}

// Tx runs fn inside one write transaction.
//
// The write lock is taken when the transaction starts (the data source
// name says _txlock=immediate), so "look the email up, and make a row
// when there is none" cannot be done by two imports at the same time.
// Without that, two concurrent imports of one email leave two live rows
// of one site account: both of them are in the rotation pool, the page
// lists the account twice, and the token of one of them is bound to be
// thrown away by the other one's re-login.
//
// An error from fn rolls the transaction back: a reader never sees a
// row whose token was not written.
//
// The rollback does not need the caller's context: the caller may have
// been cut short, and the half of the transaction that did land has to
// go back all the same.
func (s *Store) Tx(ctx context.Context, fn func(Tx) error) error {
	return s.tx(ctx, func(tx *sqlTx) error { return fn(tx) })
}

```

（事务里跑的语句和事务外的是同一批：能跑语句的就连接池和事务两样，
所以语句只写一遍，`*sql.DB` 和 `*sql.Tx` 都当同一个接口使。）

还要两个跟「签出」有关的方法。签出不是把状态改一下就完了：令牌要收走，
而且**只在手里那份令牌还是库里那一份的时候**才写。另一个进程可能刚把这
个账号登回去了（库里换成了新令牌），这时候把签出写下去，就是把别人刚登
好的账号作废掉，还把那份新令牌收走——页面说这个账号登出了，轮转池也不
用它，而它手里握着一份能用的凭据。

写状态和收走令牌是一个事务里一起写的：中间插进来一个写，账号就会停在
「状态是签出、令牌还在」的样子（状态说它登出了，令牌却还在，每次请求都
要去续一次，续不回来）。

令牌行里的 `subject` 留着：那是站点那边账号自己的 id，不是凭据。它是「这
个站点账号在库里是哪一行」的键——签出的账号恰恰是最要紧的那一个（人会
重新导入它），键要是被抹掉，那一次导入就会当成新账号再插一行。

```go
// SignOutIfTheTokenIsStillThere signs the account out and takes its
// token away, but only while the token row still holds that access
// token.
//
// 签出是**有条件**地写的：另一个进程可能刚把这个账号登回去了。手里这
// 份令牌就是这次失败说的那份；库里换成了别的，就是有人替它登好了，这
// 时候把它写回签出，就是把别人刚换好的令牌作废掉，还让这个账号显示成
// 登出的状态——手里却握着一份能用的凭据，轮转池不用它，下一轮又要登
// 一次。
//
// 它说有没有写下去。没有写，说明库里那一份令牌已经不是手里这一份了：
// 有人在这中间把账号救回来了，这次的失败说的已经不是它了。
//
// 写状态和收走令牌是一起写的。中间插进来一个写，账号就会停在「状态是
// 签出、令牌还在」的样子：状态说它登出了，令牌却还在，每次请求都要
// 去续一次，续不回来。
//
// 令牌行里的 subject 留着。那是站点那边账号自己的 id，不是凭据——它是
// 「这个站点账号在库里是哪一行」的键。抹掉它，下次导入这个账号就会当
// 成新账号再插一行，而那一行是活的。
func (s *Store) SignOutIfTheTokenIsStillThere(ctx context.Context, id, access, reason string, updatedAt time.Time) (bool, error) {
	written := false
	err := s.tx(ctx, func(tx *sqlTx) error {
		n, err := tx.setStatusIfTokenUnchanged(ctx, id, access, StatusSignedOut, reason, updatedAt)
		if err != nil || !n {
			return err
		}
		written = true
		return tx.blankToken(ctx, id, updatedAt)
	})
	if err != nil {
		return false, err
	}
	return written, nil
}

```

```go
// MarkErrorIfTheTokenIsStillThere writes the error state down, but only
// while the token row still holds that access token.
//
// 它和 SignOutIfTheTokenIsStillThere 是同一个条件、同一件事：这个错误
// 说的是手里这份令牌的事，库里换成了别的令牌，说的就不是这个账号了。
// 把别人刚换好的状态改回出错，页面就会说一个好好在用的账号出错了。
func (s *Store) MarkErrorIfTheTokenIsStillThere(ctx context.Context, id, access, reason string, updatedAt time.Time) (bool, error) {
	return s.setStatusIfTokenUnchanged(ctx, s.db, id, access, StatusError, reason, updatedAt)
}

```

还有登录那一条路要按同一套规矩走：`mmxcode login` 和页面上那个登录，以前
都是直接建一行——登录一个批量导入进来的账号，库里就多一个活着的行。同一
个站点账号在库里只能有一行：按令牌里的那个 `sub` 找那一行，找着就改它
（令牌、状态、地址、用户 id），找不到才新建。

```go
// SignIn records a sign-in: the account and its token.
//
// 一个站点账号在库里只能有一行。库里已经有这一行的时候改那一行，不另
// 起一行——同一个站点账号两行都是活的，轮转会挑到两次，页面把它列两遍，
// 而且其中一个的令牌迟早被另一个的重登作废。
//
// 找的是令牌里带的那个 sub：那是站点账号自己的 id，一个站点账号一个，
// 重登多少次都是它。清单上的邮箱可能是别名，站点那边报的邮箱又可能一
// 时问不出来，只有它是一直在的。邮箱是第二条路：有的登录那条路上，令牌
// 里的 sub 是空的。
//
// a 是这个账号该有的样子：它的 id 只在没有那一行的时候才用得上。a.Email
// 是站点那边说的地址：它是空的，说明这一次没问出身份来——那就不动那一
// 行上的地址和用户 id，把空的写上去等于把知道的东西换成不知道的。
//
// 名字是人自己写的标签：给了就写上去，没给就把原来那一行上的留着。
func (s *Store) SignIn(ctx context.Context, a Account, tok Token) (Account, error) {
	var out Account
	err := s.Tx(ctx, func(tx Tx) error {
		row, err := findForSignIn(ctx, tx, a, tok)
		if err != nil {
			return err
		}
		now := a.UpdatedAt
		if row.ID == "" {
			// 没有那一行：这就是一个新账号。名字没有给定的时候，用知道
			// 的东西起：地址、用户 id、令牌里带的那个站点账号 id。
			acc := a
			acc.Name = firstNonEmpty(a.Name, a.Email, a.UserID, tok.Subject)
			if err := tx.CreateAccount(ctx, acc); err != nil {
				return err
			}
			row = acc
		} else {
			// 已经有这一行了：改它，不另起一行。
			if a.Email != "" && (a.Email != row.Email || a.UserID != row.UserID) {
				if err := tx.SetAccountIdentity(ctx, row.ID, a.Email, a.UserID, now); err != nil {
					return err
				}
				row.Email = a.Email
				row.UserID = a.UserID
			}
			if a.Name != "" && a.Name != row.Name {
				if err := tx.SetAccountName(ctx, row.ID, a.Name, now); err != nil {
					return err
				}
				row.Name = a.Name
			}
			if row.Status != StatusActive {
				// 它刚登进来：状态回到 active。
				if err := tx.UpdateAccountStatus(ctx, row.ID, StatusActive, "", now); err != nil {
					return err
				}
				row.Status = StatusActive
			}
		}
		if err := tx.SaveToken(ctx, Token{
			AccountID: row.ID, Access: tok.Access, Refresh: tok.Refresh,
			ExpiresAt: tok.ExpiresAt, Subject: tok.Subject, UpdatedAt: now,
		}); err != nil {
			return err
		}
		out = row
		return nil
	})
	if err != nil {
		return Account{}, err
	}
	return out, nil
}

```

- [x] **Step 4: 跑测试，确认它过了**

Run: `go test ./internal/store/ -count=1`
Expected: PASS（全部，包括迁移的测试）

- [x] **Step 5: 跑整个测试套件**

Run: `go build ./... && go vet ./... && gofmt -l . && go test ./... -count=1`
Expected: 全绿

- [x] **Step 6: 提交**

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

- [x] **Step 1: 写失败的测试**

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

// TestTheDefaultGroupIsFoundEvenWhenItIsNotARealBool says a default
// group that is written as a number or as a string is found too.
//
// is_default 到底回的是不是真的布尔，没人验过（设计文档「待实测」第
// 3 条）。认错一个组和认不出一个组，后果一样：账号批不了设备码。
func TestTheDefaultGroupIsFoundEvenWhenItIsNotARealBool(t *testing.T) {
	for _, written := range []string{`1`, `"true"`, `true`} {
		f := newFakeSite(t)
		f.groups = `{"code":0,"data":{"groups":[{"id":"g1","is_default":` + written + `}]}}`
		c := f.client(t)
		f.login(t, c)
		if err := c.Approve(context.Background(), "ABCD-1234"); err != nil {
			t.Fatalf("is_default = %s: %v", written, err)
		}
		if f.approved["group_id"] != "g1" {
			t.Fatalf("is_default = %s: group_id = %v", written, f.approved["group_id"])
		}
	}
}

// TestTheOnlyGroupIsApprovedWhenNoneIsTheDefault says a group is used
// even when none of them is marked the default, as long as there is
// only one.
//
// 没有默认组的时候，站点的前端是拒绝的（它自己会写「A default group
// is required to approve authorization.」）。但只有一个组的时候没得选：
// 站点后头收不收是站点的事，让它答，比我们先拒了好。
func TestTheOnlyGroupIsApprovedWhenNoneIsTheDefault(t *testing.T) {
	f := newFakeSite(t)
	f.groups = `{"code":0,"data":{"groups":[{"id":"g1","is_default":false}]}}`
	c := f.client(t)
	f.login(t, c)
	if err := c.Approve(context.Background(), "ABCD-1234"); err != nil {
		t.Fatal(err)
	}
	if f.approved["group_id"] != "g1" {
		t.Fatalf("group_id = %v, want the only group", f.approved["group_id"])
	}
}

// TestSeveralGroupsWithoutADefaultAreAnError says several groups and
// no default among them is an error.
//
// 有得选的时候不猜：猜错了就是把账号批进别的组里去了。站点答的那个
// 形状得在错里：默认组是真的没有，还是我们没认出来，看一眼就知道
// （设计文档「待实测」第 3 条）。
func TestSeveralGroupsWithoutADefaultAreAnError(t *testing.T) {
	f := newFakeSite(t)
	f.groups = `{"code":0,"data":{"groups":[{"id":"g1"},{"id":"g2"}]}}`
	c := f.client(t)
	f.login(t, c)
	err := c.Approve(context.Background(), "ABCD-1234")
	if err == nil {
		t.Fatal("no error with several groups and no default")
	}
	if !errors.Is(err, ErrNoDefaultGroup) {
		t.Fatalf("err = %v, want the no-default-group error", err)
	}
	if !strings.Contains(err.Error(), `"id":"g1"`) {
		t.Fatalf("the site's answer is not in the error: %v", err)
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

- [x] **Step 2: 跑测试，确认它失败**

Run: `go test ./internal/webauth/ -count=1`
Expected: FAIL（包还不存在）

- [x] **Step 3: 写实现**

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
		if !isDefaultGroup(g) {
			continue
		}
		if id, ok := g["id"].(string); ok && id != "" {
			return id, nil
		}
	}
	// 没有默认组的时候，只有一个组的话就用它：没得选的时候不猜。站点
	// 收不收是站点的事，让它答 —— 我们先拒了，就把人挡在门外了。
	if len(groups) == 1 {
		if id, ok := groups[0]["id"].(string); ok && id != "" {
			return id, nil
		}
	}
	return "", noDefaultGroupError{raw: rawAnswer(answer)}
}

// isDefaultGroup says whether a group is the site's default one.
//
// 真假有三种写法：真的布尔、1、"true"。站点到底回哪一种，没人验过
// （设计文档「待实测」第 3 条）。认错一个组和认不出一个组，后果一样：
// 账号批不了设备码。
func isDefaultGroup(g map[string]any) bool {
	switch v := g["is_default"].(type) {
	case bool:
		return v
	case float64:
		return v != 0
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		return s == "true" || s == "1"
	}
	return false
}

// rawAnswer gives the site's answer as one compact line.
//
// 它进错里：默认组是真的没有，还是我们没认出来，看一眼那个形状就知道
// （设计文档「待实测」第 3 条）。
func rawAnswer(answer any) string {
	b, err := json.Marshal(answer)
	if err != nil {
		return ""
	}
	s := string(b)
	if len(s) > 800 {
		s = s[:800] + "…"
	}
	return s
}

// ErrNoDefaultGroup is the answer when the site has no default group
// to approve the code into.
//
// It is not worth trying again: the group is not there, and it will
// not be there in five seconds either.
var ErrNoDefaultGroup = errors.New("device authorize: there is no default group to approve")

// noDefaultGroupError is that answer, with the site's own words in it.
//
// 它带上站点答的那个形状：默认组是真的没有，还是我们没认出来，看一眼
// 那个形状就知道（设计文档「待实测」第 3 条）。
type noDefaultGroupError struct {
	raw string
}

func (e noDefaultGroupError) Error() string {
	return "这个账号没有默认组，设备码批不了（去站点把一个组设成默认的再来）。站点答的是：" + e.raw
}

// Is makes errors.Is(err, ErrNoDefaultGroup) true for it.
func (e noDefaultGroupError) Is(target error) bool { return target == ErrNoDefaultGroup }

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
- [x] **Step 4: 跑测试，确认它过了**

Run: `go test ./internal/webauth/ -count=1`
Expected: PASS

- [x] **Step 5: 跑整个测试套件**

Run: `go build ./... && go vet ./... && gofmt -l . && go test ./... -count=1`
Expected: 全绿

- [x] **Step 6: 提交**

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

- [x] **Step 1: 写失败的测试**

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

func TestTheWaitsAreTheOnesTheDesignSays(t *testing.T) {
	// 设计文档 §5.3：网络错误、5xx、超时退避重试 2 次（1s、4s）。
	var waits []time.Duration
	r := &Runner{
		Importer: &Importer{
			Import: func(ctx context.Context, email, password string) (Result, error) {
				return Result{Email: email}, &Error{Stage: StageToken, Message: "connection reset", Retry: true}
			},
		},
		Sleep: func(ctx context.Context, d time.Duration) error {
			waits = append(waits, d)
			return nil
		},
	}
	if _, err := r.Run(context.Background(), []Entry{{Email: "a@example.com", Password: "p"}}); err != nil {
		t.Fatal(err)
	}
	if len(waits) != 2 || waits[0] != time.Second || waits[1] != 4*time.Second {
		t.Fatalf("waits = %v, want 1s then 4s", waits)
	}
}

func TestASiteThatIsLimitingIsTriedOnlyOnceMore(t *testing.T) {
	// 429：退避 30s 重试 1 次。
	var tries int
	r := &Runner{
		Importer: &Importer{
			Import: func(ctx context.Context, email, password string) (Result, error) {
				tries++
				return Result{Email: email}, &Error{Stage: StageToken, Message: "too many requests",
					Retry: true, Wait: time.Millisecond, Tries: 2}
			},
		},
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	out, err := r.Run(context.Background(), []Entry{{Email: "a@example.com", Password: "p"}})
	if err != nil {
		t.Fatal(err)
	}
	if tries != 2 {
		t.Fatalf("tried %d times, want 2", tries)
	}
	if out[0].OK {
		t.Fatalf("out = %+v", out[0])
	}
}

func TestTheResultsAreAnArrayEvenWhenThereAreNone(t *testing.T) {
	// 一整趟导入失败的时候没有结果。存下来的那一串得是 "[]"：页面上
	// 那个脚本 JSON.parse('null') 解析出来是 null，遍历它就炸了。
	if got := ResultsJSON(nil); got != "[]" {
		t.Fatalf("results = %q, want an empty array", got)
	}
	if got := ResultsJSON([]Result{}); got != "[]" {
		t.Fatalf("results = %q, want an empty array", got)
	}
}
```

- [x] **Step 2: 跑测试，确认它失败**

Run: `go test ./internal/provision/ -count=1`
Expected: FAIL（包还不存在）

- [x] **Step 3: 写实现**

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
	// Whole says this row is not one account but the run itself.
	//
	// 整趟导入没能完成的时候，页面上要有一行说这件事。那一行不是账号，
	// 所以它不算进「失败了几个账号」里——一个账号都没处理的这一趟，失败
	// 的账号数就是零。
	Whole bool `json:"whole,omitempty"`
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
	//
	// Zero means the standard backoff: one second, then four.
	Wait time.Duration
	// Tries is how many tries at most.
	//
	// Zero means the standard number of tries. A site that is limiting
	// how much it answers is not worth three goes: the design says one.
	Tries int
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

// backoff is how long to wait before the second and the third try.
//
// 设计文档 §5.3：网络错误、5xx、超时退避重试 2 次（1s、4s）。
var backoff = []time.Duration{time.Second, 4 * time.Second}

// backoffWait gives how long to wait before the next try.
//
// The standard backoff: one second before the second try, four before
// the third. A failure that says what it wants to wait for says so, and
// this is what the others get.
func backoffWait(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if attempt >= len(backoff) {
		return backoff[len(backoff)-1]
	}
	return backoff[attempt]
}

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
	attempts := 0
	for attempt := 0; attempt < tries; attempt++ {
		attempts++
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
		if pe.Tries > 0 && pe.Tries <= attempt+1 {
			// 有些错值不值得试三次：站点在限流的时候，试一次就够，
			// 再去问只是让它更烦。
			break
		}
		wait := pe.Wait
		if wait <= 0 {
			wait = backoffWait(attempt)
		}
		if err := r.wait(ctx, wait); err != nil {
			return resultOf(e, err)
		}
	}
	res := resultOf(e, last)
	res.Message = fmt.Sprintf("试了 %d 次都不成：%s", attempts, res.Message)
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
//
// 没有结果的时候给的是 `[]`，不是 `null`：页面上那个脚本
// `JSON.parse(run.results || '[]')` 解析 `null` 是成的，出来的是
// `null`，接着 `for (const r of results)` 就炸了——一整趟导入失败的
// 时候，页面上那张结果表反而打不开。
func ResultsJSON(results []Result) string {
	if len(results) == 0 {
		return "[]"
	}
	raw, err := json.Marshal(results)
	if err != nil {
		return "[]"
	}
	return string(raw)
}
```

（`Parse` 收的是 `io.Reader`，读全部用 `io.ReadAll`。`Runner.Now` 这个字段计划里写了但用不上，去掉。）

- [x] **Step 4: 跑测试，确认它过了**

Run: `go test ./internal/provision/ -count=1`
Expected: PASS

- [x] **Step 5: 提交**

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

- [x] **Step 1: 写失败的测试**

创建 `internal/provision/chain_test.go`：

整个文件写成这样（下面是它的全文）：

```go
package provision

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mmxcode/internal/account"
	"mmxcode/internal/oauth"
	"mmxcode/internal/store"
	"mmxcode/internal/webauth"
)

// fakeStore keeps what it is given.
//
// 它也照着真库的样子来：事务期间别人进不来（真库是一开始就把写锁拿
// 着），写错了要退回去。
type fakeStore struct {
	// mu is the write lock.
	mu       sync.Mutex
	accounts []string
	tokens   []string
	// subjects are the token subjects that were written.
	subjects []string
	password []string
	// failToken makes the token refuse to be saved.
	failToken bool
	// rows are the accounts themselves.
	rows []store.Account
	// names are the names that were written.
	names []string
	// statuses are the states that were written.
	statuses []string
	// groups are the outlets that were written.
	groups []string
	// make is where the rows are made, when the test wants two imports
	// to meet there.
	make *barrier
}

// barrier lets a fixed number of callers meet.
//
// 真库里两个并发的导入是排队的：等不到对方就放行，这里就是这个意思。
type barrier struct {
	mu   sync.Mutex
	n    int
	want int
	ch   chan struct{}
}

func newBarrier(want int) *barrier {
	return &barrier{want: want, ch: make(chan struct{})}
}

// wait blocks until the wanted number of callers have arrived, or until
// the wait is over.
//
// 等不到也要放行：真库里的事务是排队的，不是卡死的。
func (b *barrier) wait() {
	b.mu.Lock()
	b.n++
	if b.n >= b.want {
		select {
		case <-b.ch:
		default:
			close(b.ch)
		}
		b.mu.Unlock()
		return
	}
	b.mu.Unlock()
	select {
	case <-b.ch:
	case <-time.After(200 * time.Millisecond):
	}
}

// snapshot is what the fake holds right now.
type snapshot struct {
	accounts []string
	tokens   []string
	subjects []string
	password []string
	rows     []store.Account
	names    []string
	statuses []string
	groups   []string
}

// snapshot takes what the fake holds, so that it can be put back.
func (s *fakeStore) snapshot() snapshot {
	return snapshot{
		accounts: append([]string(nil), s.accounts...),
		tokens:   append([]string(nil), s.tokens...),
		subjects: append([]string(nil), s.subjects...),
		password: append([]string(nil), s.password...),
		rows:     append([]store.Account(nil), s.rows...),
		names:    append([]string(nil), s.names...),
		statuses: append([]string(nil), s.statuses...),
		groups:   append([]string(nil), s.groups...),
	}
}

// restore puts back what the fake held before.
//
// 事务里写下去的东西，一眼吞掉就要退回去。
func (s *fakeStore) restore(before snapshot) {
	s.accounts = before.accounts
	s.tokens = before.tokens
	s.subjects = before.subjects
	s.password = before.password
	s.rows = before.rows
	s.names = before.names
	s.statuses = before.statuses
	s.groups = before.groups
}

// Tx runs fn inside a transaction.
//
// 真库的事务一开始就把写锁拿着：两个并发的导入是排队进来的，不是同时
// 进来的。写错了就退回去。
func (s *fakeStore) Tx(ctx context.Context, fn func(store.Tx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	before := s.snapshot()
	if err := fn(fakeTx{s}); err != nil {
		s.restore(before)
		return err
	}
	return nil
}

// fakeTx is one transaction of the fake store.
type fakeTx struct {
	s *fakeStore
}

func (t fakeTx) AccountByEmail(ctx context.Context, email string) (store.Account, error) {
	return t.s.lookup(email)
}

func (t fakeTx) AccountBySubject(ctx context.Context, subject string) (store.Account, error) {
	return t.s.lookupSubject(subject)
}

func (t fakeTx) AccountByUserID(ctx context.Context, userID string) (store.Account, error) {
	return t.s.lookupUserID(userID)
}

// lookupUserID gives the account with that site user id.
//
// 真的库里是按 accounts.user_id 找的：站点给的那个真的那个 id。一个站
// 点账号一个，重登多少次都是它。
func (s *fakeStore) lookupUserID(userID string) (store.Account, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return store.Account{}, fmt.Errorf("get the account by user id: %w", store.ErrNotFound)
	}
	var found store.Account
	have := false
	for _, r := range s.rows {
		if strings.TrimSpace(r.UserID) != userID {
			continue
		}
		if !have || r.CreatedAt.Before(found.CreatedAt) ||
			(r.CreatedAt.Equal(found.CreatedAt) && r.ID < found.ID) {
			found, have = r, true
		}
	}
	if !have {
		return store.Account{}, fmt.Errorf("get the account by user id %s: %w", userID, store.ErrNotFound)
	}
	return found, nil
}

func (t fakeTx) CreateAccount(ctx context.Context, a store.Account) error {
	t.s.waitMake()
	return t.s.createAccount(a)
}

func (t fakeTx) SaveToken(ctx context.Context, tok store.Token) error {
	return t.s.saveToken(tok)
}

func (t fakeTx) SetAccountIdentity(ctx context.Context, id, email, userID string, updatedAt time.Time) error {
	return t.s.setAccountIdentity(id, email, userID)
}

func (t fakeTx) SetAccountName(ctx context.Context, id, name string, updatedAt time.Time) error {
	return t.s.setAccountName(id, name)
}

func (t fakeTx) SetAccountProxy(ctx context.Context, id, group, url string, updatedAt time.Time) error {
	return t.s.setAccountProxy(id, group, url)
}

func (t fakeTx) UpdateAccountStatus(ctx context.Context, id, status, lastError string, updatedAt time.Time) error {
	return t.s.updateAccountStatus(id, status)
}

// waitMake lets two imports meet before they make a row.
//
// 两个并发的导入都要按邮箱找这一行，都找到没有，都要建一行——这就是要
// 防的那件事。让它们在门口碰头，这事就一定会发生，而不是碰运气。
//
// 等的时候不拿着锁：不然另一个导入连看都看不了，就成了排队，不是并发。
func (s *fakeStore) waitMake() {
	if s.make != nil {
		s.make.wait()
	}
}

// createAccount makes a row.
func (s *fakeStore) createAccount(a store.Account) error {
	s.accounts = append(s.accounts, a.ID+":"+a.Email)
	s.rows = append(s.rows, a)
	return nil
}

// saveToken writes a token.
func (s *fakeStore) saveToken(tok store.Token) error {
	if s.failToken {
		return errors.New("the disk is full")
	}
	s.tokens = append(s.tokens, tok.AccountID+":"+tok.Refresh)
	s.subjects = append(s.subjects, tok.AccountID+":"+tok.Subject)
	return nil
}

// setAccountIdentity writes the address and the user id.
func (s *fakeStore) setAccountIdentity(id, email, userID string) error {
	for i := range s.rows {
		if s.rows[i].ID == id {
			s.rows[i].Email = email
			s.rows[i].UserID = userID
		}
	}
	return nil
}

// setAccountName writes a name.
func (s *fakeStore) setAccountName(id, name string) error {
	s.names = append(s.names, id+":"+name)
	for i := range s.rows {
		if s.rows[i].ID == id {
			s.rows[i].Name = name
		}
	}
	return nil
}

// setAccountProxy writes an outlet.
func (s *fakeStore) setAccountProxy(id, group, url string) error {
	s.groups = append(s.groups, id+":"+group)
	for i := range s.rows {
		if s.rows[i].ID == id {
			s.rows[i].ProxyGroup = group
			s.rows[i].ProxyURL = url
		}
	}
	return nil
}

// updateAccountStatus writes a state.
func (s *fakeStore) updateAccountStatus(id, status string) error {
	s.statuses = append(s.statuses, id+":"+status)
	for i := range s.rows {
		if s.rows[i].ID == id {
			s.rows[i].Status = status
		}
	}
	return nil
}

// lookup gives the account with that email.
//
// 真的库里比的时候两边都折成小写、去空格：清单上写的是哪个样子，库里
// 存的是哪个样子，人看着是同一个就是同一个。
func (s *fakeStore) lookup(email string) (store.Account, error) {
	for _, r := range s.rows {
		if strings.EqualFold(strings.TrimSpace(r.Email), strings.TrimSpace(email)) {
			return r, nil
		}
	}
	return store.Account{}, fmt.Errorf("get the account by email %s: %w", email, store.ErrNotFound)
}

// lookupSubject gives the account whose token carries that subject.
//
// 真的库里是顺着令牌那一行找的：令牌里的那个 sub 是站点账号自己的 id。
// 找到好几行的时候取最早的那一行（先建的先算），跟真的库一个样子。
func (s *fakeStore) lookupSubject(subject string) (store.Account, error) {
	var found store.Account
	have := false
	for _, t := range s.subjects {
		at := strings.LastIndex(t, ":")
		if at < 0 || t[at+1:] != subject {
			continue
		}
		id := t[:at]
		for _, r := range s.rows {
			if r.ID != id {
				continue
			}
			if !have || r.CreatedAt.Before(found.CreatedAt) ||
				(r.CreatedAt.Equal(found.CreatedAt) && r.ID < found.ID) {
				found, have = r, true
			}
		}
	}
	if !have {
		return store.Account{}, fmt.Errorf("get the account by subject %s: %w", subject, store.ErrNotFound)
	}
	return found, nil
}

func (s *fakeStore) CreateAccount(ctx context.Context, a store.Account) error {
	s.waitMake()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.createAccount(a)
}

// AccountByEmail gives the account with that email.
func (s *fakeStore) AccountByEmail(ctx context.Context, email string) (store.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lookup(email)
}

// SetAccountName writes the account's name.
func (s *fakeStore) SetAccountName(ctx context.Context, id, name string, updatedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setAccountName(id, name)
}

// UpdateAccountStatus writes the account's state.
func (s *fakeStore) UpdateAccountStatus(ctx context.Context, id, status, lastError string, updatedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updateAccountStatus(id, status)
}

// SetAccountProxy writes the account's outlet.
func (s *fakeStore) SetAccountProxy(ctx context.Context, id, group, url string, updatedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setAccountProxy(id, group, url)
}

func (s *fakeStore) SaveToken(ctx context.Context, t store.Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveToken(t)
}

func (s *fakeStore) SetPassword(ctx context.Context, id, cipher string, updatedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.password = append(s.password, id+":"+cipher)
	return nil
}

// DeleteAccount takes an account back out.
//
// A context that is already done is refused, the way a real database
// would refuse the delete: the test wants to see that the chain does
// not need the caller's context to be alive.
func (s *fakeStore) DeleteAccount(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
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
//
// It takes the Store interface: the fake one and the real library both fit,
// so a test can run the same chain against either.
func aChain(st Store) *Chain {
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
	err := c.webError(context.Background(), StagePassword, &webauth.SiteError{Code: 1200046})

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
	err := c.webError(context.Background(), StageApprove, &webauth.SiteError{Reason: "login_required"})

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
	err := c.webError(context.Background(), StagePassword, &webauth.SiteError{Code: 1200007})

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
	err := c.webError(context.Background(), StageProbe, errors.New("connection reset"))

	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
	if !pe.Retry {
		t.Fatalf("err = %+v", pe)
	}
	// 等多久是标准退避的事（1s、4s），不是这一处说了算。
	if pe.Wait != 0 {
		t.Fatalf("wait = %v, want the standard backoff", pe.Wait)
	}
}

func TestALostConnectionWhilePollingIsTriedAgain(t *testing.T) {
	// 轮询的时候连接断了，跟前面几步的连接断了是一回事：再来一次。
	c := &Chain{}
	err := c.pollError(context.Background(), errors.New("device token: connection reset"))

	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
	if !pe.Retry {
		t.Fatalf("err = %+v", pe)
	}
}

func TestARefusedDeviceCodeIsNotTriedAgain(t *testing.T) {
	// 设备码过期了，或者站点不让这个码进来：再来一次也是这个答案。
	for _, code := range []string{"expired_token", "access_denied"} {
		c := &Chain{}
		err := c.pollError(context.Background(), &oauth.Error{Code: code, What: oauth.WhatSignIn})

		var pe *Error
		if !errors.As(err, &pe) {
			t.Fatalf("err = %v", err)
		}
		if pe.Retry {
			t.Fatalf("%s: err = %+v, want it not to be tried again", code, pe)
		}
	}
}

func TestASiteThatIsLimitingIsWaitedFor(t *testing.T) {
	// 429：等久一点再来一次，试一次就够。
	c := &Chain{}
	err := c.pollError(context.Background(), &oauth.Error{Code: "too_many_requests", HTTPStatus: 429, What: oauth.WhatSignIn})

	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
	if !pe.Retry || pe.Wait != 30*time.Second || pe.Tries != 2 {
		t.Fatalf("err = %+v", pe)
	}
}

func TestACancelledPollIsNotTriedAgain(t *testing.T) {
	// 上下文没了：再试也是白试。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &Chain{}
	err := c.pollError(ctx, context.Canceled)

	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
	if pe.Retry {
		t.Fatalf("err = %+v", pe)
	}
}

func TestAClientSideTimeoutWhilePollingIsTriedAgain(t *testing.T) {
	// 客户端自己的超时（http.Client.Timeout）报出来的错也是
	// context.DeadlineExceeded，但那是连接那边的事：调用方的上下文还
	// 好好地活着，这种要再试一次，跟前面几步的连接断了是一回事。
	//
	// 真的一次超时长什么样：拿真的客户端去问一个不吭声的服务。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
	}))
	defer srv.Close()
	hc := &http.Client{Timeout: 30 * time.Millisecond}
	_, err := hc.Get(srv.URL)
	if err == nil {
		t.Fatal("the request did not time out")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want it to be a deadline", err)
	}

	c := &Chain{}
	pe := pollErrorOf(t, c.pollError(context.Background(), err))
	if !pe.Retry {
		t.Fatalf("err = %+v, want it to be tried again", pe)
	}
}

func TestAPollTimeoutIsNotTriedAgainWhenTheRunIsOver(t *testing.T) {
	// 上下文没了：再试也是白试。这时候超时就是超时，不再试。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := &Chain{}
	pe := pollErrorOf(t, c.pollError(ctx, context.DeadlineExceeded))
	if pe.Retry {
		t.Fatalf("err = %+v, want it not to be tried again", pe)
	}
}

// pollErrorOf says what the error the chain made was.
func pollErrorOf(t *testing.T, err error) *Error {
	t.Helper()
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
	return pe
}

func TestTheChainIsTriedAgainWhenThePollLosesTheConnection(t *testing.T) {
	// 轮询时的一次连接断了，整个链要重来一次：这个账号最后是进来了的。
	st := &fakeStore{}
	c := aChain(st)
	polls := 0
	c.Poll = func(ctx context.Context, d *oauth.Device) (*oauth.Token, error) {
		polls++
		if polls == 1 {
			return nil, errors.New("device token: connection reset")
		}
		return &oauth.Token{Access: "acc", Refresh: "ref", Subject: "sub"}, nil
	}
	r := &Runner{
		Importer: &Importer{
			Import: func(ctx context.Context, email, password string) (Result, error) {
				return c.Import(ctx, email, password)
			},
		},
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	out, err := r.Run(context.Background(), []Entry{{Email: "a@example.com", Password: "pw"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || !out[0].OK {
		t.Fatalf("out = %+v", out)
	}
	if polls != 2 {
		t.Fatalf("polled %d times, want 2", polls)
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

func TestAFailedIdentityLookupKeepsTheEmailOfTheList(t *testing.T) {
	// 站点那边问不出身份的时候，邮箱得从清单里兜底：邮箱空着的账号
	// 以后落线了是登不回来的——有密码也不知道该登谁。
	st := &fakeStore{}
	c := aChain(st)
	c.Identity = func(ctx context.Context, access, subject string) (account.Identity, error) {
		return account.Identity{}, errors.New("the account API answered 503")
	}

	res, err := c.Import(context.Background(), "a@example.com", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("res = %+v", res)
	}
	if len(st.accounts) != 1 || st.accounts[0] != "acc_1:a@example.com" {
		t.Fatalf("accounts = %v", st.accounts)
	}
	if !strings.Contains(res.Message, "身份") {
		t.Fatalf("message = %q, want it to say the identity was not confirmed", res.Message)
	}
	if strings.Contains(res.Message, "hunter2") {
		t.Fatalf("message = %q, want no password in it", res.Message)
	}
}

func TestTheSameAccountTwiceIsOneRow(t *testing.T) {
	// 重跑一次导入是常事：同一个站点账号只能有一行活着。
	st := &fakeStore{}
	c := aChain(st)

	first, err := c.Import(context.Background(), "a@example.com", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.Import(context.Background(), "a@example.com", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.accounts) != 1 {
		t.Fatalf("accounts = %v, want one row", st.accounts)
	}
	if second.AccountID != first.AccountID {
		t.Fatalf("account id = %q, want %q", second.AccountID, first.AccountID)
	}
	if !second.OK {
		t.Fatalf("res = %+v", second)
	}
	if !strings.Contains(second.Message, "已经在库里") {
		t.Fatalf("message = %q, want it to say it was an update", second.Message)
	}
	// 令牌是换新的：这一行现在用的是刚登进去的那条链。
	if len(st.tokens) != 2 {
		t.Fatalf("tokens = %v, want the token written twice", st.tokens)
	}
	if len(st.statuses) != 1 || st.statuses[0] != "acc_1:"+store.StatusActive {
		t.Fatalf("statuses = %v", st.statuses)
	}
}

func TestAFailedUpdateKeepsTheRowThatIsThere(t *testing.T) {
	// 令牌没写下去的时候，库里原来那一行不能动：它还带着一条能用的
	// 链，把它删掉就是毁掉一个还能用的账号。
	//
	// 而且动的得是原来那一行，不是新来的一行：新来的一行号不一样，
	// 原来那条链也没有被换掉。这两样都要看得见——「还剩一行」是看不
	// 出来的（新来的一行建了又删，看着也像没动过）。
	st := &fakeStore{}
	c := aChain(st)
	// 两次导入给两个不同的号：新来的一行一眼就看得出来。
	ids := 0
	c.NewID = func() string {
		ids++
		return fmt.Sprintf("acc_%d", ids)
	}
	// 两次登进去的令牌不一样：换了令牌一眼就看得出来。
	polls := 0
	c.Poll = func(ctx context.Context, d *oauth.Device) (*oauth.Token, error) {
		polls++
		return &oauth.Token{Access: "acc", Refresh: fmt.Sprintf("ref%d", polls), Subject: "sub"}, nil
	}

	res, err := c.Import(context.Background(), "a@example.com", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	first := res.AccountID
	firstToken := st.tokens[0]

	st.failToken = true
	res, err = c.Import(context.Background(), "a@example.com", "hunter2")
	if err == nil {
		t.Fatal("no error for a token that could not be saved")
	}
	if res.OK {
		t.Fatalf("res = %+v", res)
	}
	if len(st.accounts) != 1 || st.accounts[0] != first+":a@example.com" {
		t.Fatalf("accounts = %v, want the original row %q", st.accounts, first)
	}
	if len(st.rows) != 1 || st.rows[0].ID != first {
		t.Fatalf("rows = %v, want the original row %q", st.rows, first)
	}
	// 令牌没有换：写失败的时候不能换。
	if len(st.tokens) != 1 || st.tokens[0] != firstToken {
		t.Fatalf("tokens = %v, want the token that is there", st.tokens)
	}
	// 第二次导入没有另起一行：它用的就是原来那一行。
	if ids != 1 {
		t.Fatalf("the import made %d ids, want 1", ids)
	}
}

// TestTwoConcurrentImportsOfOneEmailAreOneRow says two imports of one
// email do not make two rows.
//
// 两个并发的导入（网页上传两趟，或者两个进程各跑一趟）：都要按邮箱找
// 这一行，都找到没有，都建一行——同一个站点账号两行都是活的，页面把
// 它列两遍，其中一个的令牌迟早被另一个的重登作废。
//
// 找和建要在一个事务里：事务一开始就把写锁拿着，第二个进来的那个看见
// 的是第一个建好的那一行。
func TestTwoConcurrentImportsOfOneEmailAreOneRow(t *testing.T) {
	st := &fakeStore{make: newBarrier(2)}
	c := aChain(st)
	// 两次导入给两个不同的号：两行就看得出来是两行。
	var mu sync.Mutex
	n := 0
	c.NewID = func() string {
		mu.Lock()
		defer mu.Unlock()
		n++
		return fmt.Sprintf("acc_%d", n)
	}

	var wg sync.WaitGroup
	results := make([]Result, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = c.Import(context.Background(), "a@example.com", "hunter2")
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("import %d: %v", i, err)
		}
	}
	if len(st.accounts) != 1 {
		t.Fatalf("accounts = %v, want one row", st.accounts)
	}
	if results[0].AccountID != results[1].AccountID {
		t.Fatalf("account ids = %q and %q, want the same row",
			results[0].AccountID, results[1].AccountID)
	}
}

func TestTheAccountGoesOutOfTheGroupItWasImportedWith(t *testing.T) {
	// 账号从哪个口登进去的，以后它的活就从哪个口出：站点看到的应该是
	// 同一个账号从同一个地方来，而不是两个地方。
	st := &fakeStore{}
	c := aChain(st)
	c.ProxyGroup = "hk"

	if _, err := c.Import(context.Background(), "a@example.com", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if len(st.rows) != 1 {
		t.Fatalf("rows = %v", st.rows)
	}
	if st.rows[0].ProxyGroup != "hk" {
		t.Fatalf("group = %q, want hk", st.rows[0].ProxyGroup)
	}
}

// TestAnAccountWithItsOwnAddressGoesOutOfTheGroupItWasImportedWith
// says the group the import carries wins over the address on the
// account.
//
// 出口是按「先看地址、再看组」找的：地址留着，账号还是从那个地方出去，
// 组写着是 hk 也没用。导入是人明确做的一件事，它带的组说了算，所以那个
// 地址清掉，而且要说出来——地址没了，人得知道。
func TestAnAccountWithItsOwnAddressGoesOutOfTheGroupItWasImportedWith(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)
	c.ProxyGroup = "hk"

	res, err := c.Import(context.Background(), "a@example.com", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	// 人给这个账号手设了一个地址。
	if err := st.SetAccountProxy(context.Background(), res.AccountID, "hk",
		"socks5h://127.0.0.1:1080", time.Now()); err != nil {
		t.Fatal(err)
	}
	if st.rows[0].ProxyURL == "" {
		t.Fatalf("the address was not set: %+v", st.rows[0])
	}

	res, err = c.Import(context.Background(), "a@example.com", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if st.rows[0].ProxyURL != "" {
		t.Fatalf("the address is still there: %+v", st.rows[0])
	}
	if st.rows[0].ProxyGroup != "hk" {
		t.Fatalf("group = %q, want hk", st.rows[0].ProxyGroup)
	}
	if !strings.Contains(res.Message, "地址") {
		t.Fatalf("message = %q, want it to say the address was cleared", res.Message)
	}
	// 被清掉的是哪个地址，得写出来：那是一个人自己设的地址，没了就得能
	// 照着填回去。
	if !strings.Contains(res.Message, "socks5h://127.0.0.1:1080") {
		t.Fatalf("message = %q, want it to name the address that was cleared", res.Message)
	}
}

// TestAnImportWithoutAGroupKeepsTheGroupThatIsThere says an import
// without a group does not wipe the group the account has.
//
// 组是人自己设的：一次没带组的导入不能把它抹掉——抹掉了账号的出口就变
// 了，而导入的人根本没有说要变。
func TestAnImportWithoutAGroupKeepsTheGroupThatIsThere(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)
	c.ProxyGroup = "hk"

	if _, err := c.Import(context.Background(), "a@example.com", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if st.rows[0].ProxyGroup != "hk" {
		t.Fatalf("group = %q, want hk", st.rows[0].ProxyGroup)
	}
	// 第二趟没带组。
	c.ProxyGroup = ""
	if _, err := c.Import(context.Background(), "a@example.com", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if st.rows[0].ProxyGroup != "hk" {
		t.Fatalf("group = %q, want hk: an import without a group must not wipe it",
			st.rows[0].ProxyGroup)
	}
	// 而且出口那一行根本没动过。
	if len(st.groups) != 0 {
		t.Fatalf("the outlet was written %d times, want none", len(st.groups))
	}
}

func TestAnAccountWithoutAGroupGoesOutOfTheDefault(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)

	if _, err := c.Import(context.Background(), "a@example.com", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if st.rows[0].ProxyGroup != "" {
		t.Fatalf("group = %q, want the default one", st.rows[0].ProxyGroup)
	}
}

func TestTheBrokenAccountIsTakenBackOutWhenTheRunWasCutShort(t *testing.T) {
	// 导入被掐断的时候，令牌写不进去：那个半成的行要拿走。删它用的
	// 上下文不能是调用方那个——那时候它已经没了。
	st := &fakeStore{failToken: true}
	c := aChain(st)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := c.Import(ctx, "a@example.com", "hunter2"); err == nil {
		t.Fatal("no error")
	}
	if len(st.accounts) != 0 {
		t.Fatalf("the broken account was left behind: %v", st.accounts)
	}
}

// TestAnAccountIsFoundByItsSubjectEvenWhenTheAddressIsDifferent says
// the token's subject is the key, not the address.
//
// 清单上写的地址可能是别名，站点那边报的地址又可能一时问不出来（站点
// 503 了）。靠地址去重就会漏：同一个站点账号在库里会有两行都是活的，
// 两个都在轮转池里，其中一个的令牌迟早被另一个的重登作废。
//
// 令牌里带的那个 sub 是站点账号自己的 id：一个站点账号一个，重登多少
// 次都是它。
func TestAnAccountIsFoundByItsSubjectEvenWhenTheAddressIsDifferent(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)

	if _, err := c.Import(context.Background(), "a@example.com", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if len(st.rows) != 1 {
		t.Fatalf("rows = %v", st.rows)
	}

	// 第二趟：站点那边问不出身份了（503），清单上的地址也换了。
	c.Identity = func(ctx context.Context, access, subject string) (account.Identity, error) {
		return account.Identity{}, errors.New("the site is busy")
	}
	res, err := c.Import(context.Background(), "other@example.com", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.rows) != 1 {
		t.Fatalf("rows = %v, want one row for one site account", st.rows)
	}
	if res.AccountID != "acc_1" {
		t.Fatalf("account id = %q, want the row that was already there", res.AccountID)
	}
}

// TestTheAddressIsTrimmed says the address is written without the
// spaces around it.
//
// 地址是去重的键之一：同一个地址写成两种样子（一边带空格一边不带），
// 库里就会看成两个账号。
func TestTheAddressIsTrimmed(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)
	// 站点那边报的地址两头带着空格。
	c.Identity = func(ctx context.Context, access, subject string) (account.Identity, error) {
		return account.Identity{Email: "  real@example.com  "}, nil
	}
	if _, err := c.Import(context.Background(), "alias@example.com", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if st.rows[0].Email != "real@example.com" {
		t.Fatalf("email = %q, want it trimmed", st.rows[0].Email)
	}

	// 清单上的地址两头带着空格，站点那边又问不出身份。
	st2 := &fakeStore{}
	c2 := aChain(st2)
	c2.Identity = nil
	if _, err := c2.Import(context.Background(), "  alias@example.com  ", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if st2.rows[0].Email != "alias@example.com" {
		t.Fatalf("email = %q, want it trimmed", st2.rows[0].Email)
	}
}

// TestTheAddressIsNotTakenOverFromAnotherAccount says an address that
// another account already has is not written onto this one.
//
// 两个账号共用一个地址不是好事：掉线以后重登，分不清该拿哪个密码去登
// ——密码是对着其中一个的，登错了就是把这个账号也登坏了。
//
// 这一行是按令牌里那个 id 找到的（那是站点账号自己的 id），站点那边报
// 出来的地址却是另一个账号的：这时候地址不能换过去。
func TestTheAddressIsNotTakenOverFromAnotherAccount(t *testing.T) {
	st := &fakeStore{
		rows: []store.Account{
			{ID: "acc_a", Name: "a", Email: "taken@example.com", Status: store.StatusActive},
			{ID: "acc_b", Name: "b", Email: "b@example.com", Status: store.StatusActive},
		},
		subjects: []string{"acc_b:sub-b"},
	}
	c := aChain(st)
	c.NewID = func() string { return "acc_new" }
	c.Poll = func(ctx context.Context, d *oauth.Device) (*oauth.Token, error) {
		return &oauth.Token{Access: "acc", Refresh: "ref", Subject: "sub-b"}, nil
	}
	c.Identity = func(ctx context.Context, access, subject string) (account.Identity, error) {
		return account.Identity{Email: "taken@example.com", RealUserID: "u9"}, nil
	}

	res, err := c.Import(context.Background(), "b@example.com", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if res.AccountID != "acc_b" {
		t.Fatalf("account id = %q, want acc_b", res.AccountID)
	}
	if st.rows[1].Email != "b@example.com" {
		t.Fatalf("email = %q, want the address the account already had", st.rows[1].Email)
	}
	if !strings.Contains(res.Message, "acc_a") {
		t.Fatalf("message = %q, want it to name the account that has the address", res.Message)
	}
}

// TestTheRealLibraryHasOneRowForOneSiteAccount says the real library
// keeps one row for one site account, even when the address on the list
// is not the one the site knows.
//
// 假的库和真的库不是一回事：真的库按令牌里那个 sub 顺着令牌那一行找，假
// 的那套是自己的一套规矩。这一条拿真的库走一遍：清单上的地址换了，站点
// 那边一时又问不出身份，库里还是只有一行。
func TestTheRealLibraryHasOneRowForOneSiteAccount(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	c := aChain(s)
	// 第一趟：站点那边报了地址，那一行上的地址是站点认的那个。
	if _, err := c.Import(ctx, "alias@example.com", "hunter2"); err != nil {
		t.Fatal(err)
	}
	// 第二趟：清单上的地址换了，站点那边问不出身份了。
	c.Identity = nil
	if _, err := c.Import(ctx, "other@example.com", "hunter2"); err != nil {
		t.Fatal(err)
	}

	rows, err := s.ListAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("accounts = %v, want one row for one site account", rows)
	}
	if rows[0].Email != "a@example.com" {
		t.Fatalf("email = %q, want the one the site says", rows[0].Email)
	}
}

// TestTheRowIsBroughtInLineWithWhatTheSiteSays says the address and
// the user id on the row are the ones the site says.
//
// 去重的时候邮箱是键之一：键停在旧的地方，下一次导入就会把这个账号
// 当成新账号，再插一行。站点那边报出来的真地址知道了，就得写上去。
func TestTheRowIsBroughtInLineWithWhatTheSiteSays(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)
	// 第一趟：身份没问，邮箱只能拿清单上的。
	c.Identity = nil
	if _, err := c.Import(context.Background(), "alias@example.com", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if st.rows[0].Email != "alias@example.com" {
		t.Fatalf("email = %q, want the one from the list", st.rows[0].Email)
	}

	// 第二趟：站点那边说了它是谁。
	c.Identity = func(ctx context.Context, access, subject string) (account.Identity, error) {
		return account.Identity{Email: "real@example.com", RealUserID: "u9"}, nil
	}
	res, err := c.Import(context.Background(), "alias@example.com", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.rows) != 1 {
		t.Fatalf("rows = %v, want one row", st.rows)
	}
	if st.rows[0].Email != "real@example.com" {
		t.Fatalf("email = %q, want the one the site says", st.rows[0].Email)
	}
	if st.rows[0].UserID != "u9" {
		t.Fatalf("user id = %q, want the one the site says", st.rows[0].UserID)
	}
	// 换了地址要说：掉线以后重登用的是这一栏。
	if !strings.Contains(res.Message, "real@example.com") {
		t.Fatalf("message = %q, want it to say the address changed", res.Message)
	}
}

// TestTheAddressOnTheRowIsNotThrownAwayWhenTheSiteSaysNothing says a
// row that is already there keeps its address when the site does not
// say one.
//
// 站点那边一时问不出身份，手里的地址就只剩原来那一行的：那是站点自己
// 认过的，比清单上的更靠得住。
//
// 这一条要能分清两件事：地址留在原来那一行，和地址换成清单上那个。这两
// 件事在清单上的地址跟原来那一行一样的时候是一样的事——所以这里清单上
// 的地址跟那一行的不一样，换成清单上那个是看得出来的。
func TestTheAddressOnTheRowIsNotThrownAwayWhenTheSiteSaysNothing(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)
	// 第一趟：站点那边说了它是谁，那一行上的地址是站点认的那个。
	if _, err := c.Import(context.Background(), "alias@example.com", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if st.rows[0].Email != "a@example.com" {
		t.Fatalf("email = %q, want the one the site says", st.rows[0].Email)
	}

	// 第二趟：站点那边问不出身份了，清单上的地址是另一个。
	c.Identity = nil
	if _, err := c.Import(context.Background(), "alias@example.com", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if st.rows[0].Email != "a@example.com" {
		t.Fatalf("email = %q, want the one that was already there", st.rows[0].Email)
	}
}

// TestTwoRowsForOneSiteAccountAreSaidOut says the import says so when
// the library already has two rows for one site account.
//
// 那不是这次导入造成的，但两行都在轮转池里，页面把它列两遍，重登一次
// 会把另一个的令牌作废。人得知道这件事。
func TestTwoRowsForOneSiteAccountAreSaidOut(t *testing.T) {
	st := &fakeStore{}
	// 库里已经有同一个站点账号的两行（老数据）。
	st.rows = append(st.rows,
		store.Account{ID: "acc_1", Email: "a@example.com", UserID: "u1", Status: store.StatusActive},
		store.Account{ID: "acc_2", Email: "b@example.com", UserID: "u2", Status: store.StatusActive})
	st.subjects = append(st.subjects, "acc_1:sub", "acc_2:sub")
	c := aChain(st)

	res, err := c.Import(context.Background(), "b@example.com", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Message, "两行") {
		t.Fatalf("message = %q, want it to say the library has two rows", res.Message)
	}
}

// TestAFailureIsNotWorthTryingAgainWhenTheRunIsOver says a finished
// run does not get a failure that says "try again".
//
// 这一趟已经结束了：再试也是白试，等的那一下也是白等。说了「值得再试」
// 只是让结果表上的那一行写着它做不到的事。
func TestAFailureIsNotWorthTryingAgainWhenTheRunIsOver(t *testing.T) {
	c := &Chain{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := c.webError(ctx, StageProbe, errors.New("connection reset"))
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
	if pe.Retry {
		t.Fatalf("err = %+v, want it not to be tried again", pe)
	}
	if pe.Message != "connection reset" {
		t.Fatalf("message = %q, want what went wrong", pe.Message)
	}
}

// TestAnAccountIsFoundByItsUserIDEvenWhenTheAddressIsDifferent says the
// site user id finds the account.
//
// 有的账号站点那边报不出邮箱：清单上写的地址是别名，令牌里也没有 sub。
// 只有站点给的那个真的 user id 认得出是哪一个站点账号——少了这一步，再
// 导一次就多一行活的，同一个站点账号两行都在轮转池里。
func TestAnAccountIsFoundByItsUserIDEvenWhenTheAddressIsDifferent(t *testing.T) {
	st := &fakeStore{}
	c := aChain(st)
	// 站点那边报不出邮箱，也没有别的线索，只有那个 user id。
	c.Identity = func(ctx context.Context, access, subject string) (account.Identity, error) {
		return account.Identity{RealUserID: "u1"}, nil
	}
	c.Poll = func(ctx context.Context, d *oauth.Device) (*oauth.Token, error) {
		return &oauth.Token{Access: "acc", Refresh: "ref"}, nil
	}
	if _, err := c.Import(context.Background(), "a@example.com", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if len(st.rows) != 1 {
		t.Fatalf("rows = %v", st.rows)
	}

	// 第二趟：清单上的地址换了，站点那边报的还是同一个 user id。
	res, err := c.Import(context.Background(), "other@example.com", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.rows) != 1 {
		t.Fatalf("rows = %v, want one row for one site account", st.rows)
	}
	if res.AccountID != "acc_1" {
		t.Fatalf("account id = %q, want the row that was already there", res.AccountID)
	}
}
```

- [x] **Step 2: 跑测试，确认它失败**

Run: `go test ./internal/provision/ -count=1 -run "Chain"`
Expected: FAIL（`Chain` 还没有）

- [x] **Step 3: 写实现**

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
	"net/http"
	"net/url"
	"strings"
	"time"

	"mmxcode/internal/account"
	"mmxcode/internal/oauth"
	"mmxcode/internal/site"
	"mmxcode/internal/store"
	"mmxcode/internal/webauth"
)

// Store is where an imported account goes.
//
// 找和建要在一个事务里：不然两个并发的导入会各自看见「还没有」这一
// 行，然后各建一行——同一个站点账号两行都是活的。
type Store interface {
	// Tx runs fn inside one write transaction.
	Tx(ctx context.Context, fn func(store.Tx) error) error
	// SetPassword writes an account's encrypted password.
	SetPassword(ctx context.Context, id, cipher string, updatedAt time.Time) error
	// UpdateAccountStatus writes the account's state and its last
	// error.
	UpdateAccountStatus(ctx context.Context, id, status, lastError string, updatedAt time.Time) error
	// SetAccountName writes the account's name.
	SetAccountName(ctx context.Context, id, name string, updatedAt time.Time) error
	// SetAccountProxy writes the account's outlet.
	SetAccountProxy(ctx context.Context, id, group, url string, updatedAt time.Time) error
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
	// ProxyGroup is the outlet group the accounts go out through.
	//
	// 账号从哪个口登进去的，以后它的活就从哪个口出：站点看到的是同一个
	// 账号从同一个地方来。空的是全局默认出口。
	ProxyGroup string
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
		return nil, &Error{Stage: StageToken, Message: err.Error(), Retry: true}
	}
	deviceID, err := c.Web.RegisterDevice(ctx)
	if err != nil {
		return nil, &Error{Stage: StageProbe, Message: err.Error(), Retry: true}
	}
	// The page the sign-in sends the browser back to is the page that
	// shows the code. The site builds this path itself when it says a
	// login is needed.
	redirect := "/oauth-authorize?client_id=" + url.QueryEscape(site.ClientID) +
		"&user_code=" + url.QueryEscape(d.UserCode)

	if err := c.Web.Probe(ctx, email, deviceID, redirect); err != nil {
		return nil, c.webError(ctx, StageProbe, err)
	}
	if err := c.Web.Password(ctx, email, password, deviceID, redirect); err != nil {
		return nil, c.webError(ctx, StagePassword, err)
	}
	if err := c.Web.Approve(ctx, d.UserCode); err != nil {
		if errors.Is(err, webauth.ErrNoDefaultGroup) {
			// 有好几个组、没有一个是默认的：重试多少次都一样，而且这
			// 一条得把站点答的原文带着 —— 人得看见站点到底答了什么，
			// 才知道是去站点上设默认组，还是我们没认出来。
			return nil, &Error{Stage: StageApprove, Message: err.Error()}
		}
		return nil, c.webError(ctx, StageApprove, err)
	}
	if c.Poll == nil {
		return nil, &Error{Stage: StageToken, Message: "没有地方等这个令牌"}
	}
	tok, err := c.Poll(ctx, d)
	if err != nil {
		return nil, c.pollError(ctx, err)
	}
	return tok, nil
}

// Import signs one account in and puts it in the database.
//
// An account that is already in the database is signed in again and its
// row is updated: one site account must not have two live rows.
func (c *Chain) Import(ctx context.Context, email, password string) (Result, error) {
	// 地址前后带的空格是要去掉的：地址是去重的键之一，同一个地址写成
	// 两种样子，库里就会看成两个账号。
	email = strings.TrimSpace(email)
	tok, err := c.SignIn(ctx, email, password)
	if err != nil {
		return Result{Email: email}, err
	}
	if c.Store == nil {
		return Result{Email: email}, &Error{Stage: StageStore, Message: "没有数据库可放"}
	}

	id := account.Identity{}
	var notes []string
	if c.Identity != nil {
		got, err := c.Identity(ctx, tok.Access, tok.Subject)
		if err == nil {
			got.Email = strings.TrimSpace(got.Email)
			id = got
		} else {
			// 身份没问出来，账号还是在的，只是不知道站点那边是不是就认
			// 这一份。这件事得说出来：不然人以为每一条都对，而邮箱要是
			// 也空了，这个号以后就再也登不回来了。
			notes = append(notes, "没能确认账号身份："+err.Error())
		}
	}
	now := c.now()
	// 邮箱从清单里兜底：站点那边一时问不出来身份，账号的邮箱也不能
	// 就这么空着。没有邮箱的账号落线以后是登不回来的——手里有密码
	// 也用不上，因为不知道该拿它去登谁。
	mail := id.Email
	if mail == "" {
		mail = email
	}

	// 这个账号是不是已经在库里了？重跑一次导入是常事：上一批有一部分
	// 失败了，重来一遍是补救的办法。库里已经有这一行的时候再建一行，
	// 同一个站点账号就有两个活着的行——两个都在轮转池里，页面把它列
	// 两遍，而且其中一个的令牌迟早被另一个的重登作废。
	//
	// 找和写在一个事务里：查了再做的事，两个并发的导入一起做就会做两
	// 遍（网页上传两趟，或者两个进程各跑一趟）。令牌跟这一行一起写：
	// 读者不会看见一行没有令牌的账号。
	var existed bool
	var row store.Account
	err = c.Store.Tx(ctx, func(tx store.Tx) error {
		existing, found, err := findIn(ctx, tx, mail, email, tok.Subject, id.RealUserID)
		if err != nil {
			return err
		}
		notes = append(notes, found...)
		// 这一行该有的地址和用户 id：站点那边报出来的最准，问不出来就
		// 拿手里有的那个顶上。已经在库里那一行原来就有的，比清单上的
		// 地址更靠得住：那个地址是站点自己认的。
		wantMail := id.Email
		if wantMail == "" {
			wantMail = email
			if existing.ID != "" {
				wantMail = existing.Email
			}
		}
		wantUserID := id.RealUserID
		if wantUserID == "" {
			wantUserID = tok.Subject
			if existing.ID != "" && existing.UserID != "" {
				wantUserID = existing.UserID
			}
		}
		if existing.ID != "" {
			existed = true
			row = existing
			// 站点那边报出来的真邮箱和真用户 id 知道了，就把这一行对齐：
			// 邮箱是去重的键之一，键停在旧的地方，下一次导入就会把这个
			// 账号当成新账号，再插一行。
			if wantMail != row.Email {
				// 别处已经有这个地址了就别拿：两个账号共用一个地址，掉线
				// 以后重登就分不清该登谁了——密码是对着其中一个的。
				other, err := tx.AccountByEmail(ctx, wantMail)
				switch {
				case err == nil && other.ID != row.ID:
					notes = append(notes, "另一个账号（"+other.ID+
						"）也在用这个地址，地址没有换过去："+wantMail)
					wantMail = row.Email
				case err != nil && !errors.Is(err, store.ErrNotFound):
					notes = append(notes, "没能查这个地址有没有别人在用，地址没有换过去："+
						wantMail+"（"+err.Error()+"）")
					wantMail = row.Email
				}
			}
			if wantMail != row.Email || wantUserID != row.UserID {
				if err := tx.SetAccountIdentity(ctx, row.ID, wantMail, wantUserID, now); err != nil {
					return err
				}
				if wantMail != row.Email {
					// 换了地址要说：那是从清单上写的那个，换成了站点自己
					// 认的那个。掉线以后重登用的是这一栏，地址换了人得
					// 知道。
					notes = append(notes, "这个账号的邮箱换成了站点那边的："+wantMail)
				}
				row.Email = wantMail
				row.UserID = wantUserID
			}
			return tx.SaveToken(ctx, store.Token{
				AccountID: row.ID,
				Access:    tok.Access,
				Refresh:   tok.Refresh,
				ExpiresAt: tok.Expires,
				Subject:   tok.Subject,
				UpdatedAt: now,
			})
		}
		acc := store.Account{
			ID:         c.newID(),
			Name:       mail,
			UserID:     wantUserID,
			Email:      wantMail,
			Status:     store.StatusActive,
			ProxyGroup: c.ProxyGroup,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		if err := tx.CreateAccount(ctx, acc); err != nil {
			return err
		}
		if err := tx.SaveToken(ctx, store.Token{
			AccountID: acc.ID,
			Access:    tok.Access,
			Refresh:   tok.Refresh,
			ExpiresAt: tok.Expires,
			Subject:   tok.Subject,
			UpdatedAt: now,
		}); err != nil {
			return err
		}
		row = acc
		return nil
	})
	if err != nil {
		msg := err.Error()
		if existed {
			// 原来那一行没动过：它还带着一条能用的链，报错就是，不用
			// 把这一行删掉——删掉就是毁掉一个还能用的账号。
			msg = "令牌没能存下，库里原来那一行没动：" + msg
		}
		return Result{Email: email}, &Error{Stage: StageStore, Message: msg}
	}

	if existed {
		notes = c.refresh(ctx, row, id.Email, now, notes)
		notes = append(notes, "这个账号已经在库里了，更新了原来那一行")
	}
	notes, stored := c.keep(ctx, row.ID, password, now, notes)
	res := Result{Email: email, OK: true, AccountID: row.ID, Message: strings.Join(notes, "；")}
	if !stored {
		res.Stage = StageStore
	}
	return res, nil
}

// refresh puts the new sign-in on the row that is already there.
//
// 状态、名字、出口组这三样在事务外写：它们各写各的列，写不进去不
// 影响别的——令牌已经是新的了，只是状态还没跟上，这种事要说出来，不
// 是导入失败。
//
// 名字只在真的问出身份的时候才改：问不出来的时候手里没有真名字，拿
// 清单上的邮箱把原来的名字盖掉，是把知道的东西换成不知道的。
func (c *Chain) refresh(ctx context.Context, acc store.Account,
	knownName string, now time.Time, notes []string) []string {
	// 它刚刚登进去了：状态回到 active。这笔写不进去不是导入失败——
	// 令牌已经是新的了，只是状态还没跟上，要说出来。
	if err := c.Store.UpdateAccountStatus(ctx, acc.ID, store.StatusActive, "", now); err != nil {
		notes = append(notes, "令牌换新了，但状态没能写成 active："+err.Error())
	}
	if knownName != "" && knownName != acc.Name {
		if err := c.Store.SetAccountName(ctx, acc.ID, knownName, now); err != nil {
			notes = append(notes, "名字没能更新："+err.Error())
		}
	}
	// 出口组：导入是从哪个口登的，账号以后就走哪个口。没带组的导入不动
	// 原来那一行的组和地址——那是人自己设的，不能因为一次没带组的导入就没了。
	//
	// 带了组的时候，账号自己那个地址要让位：出口是按「先看地址、再看组」
	// 找的（internal/egress），地址留着就还是从那个地方出去，组写着是 hk
	// 也没用。导入是人明确做的一件事，它带的组说了算，所以那个地址清掉，
	// 而且要说出来——地址没了是件人该知道的事。
	if c.ProxyGroup != "" && (c.ProxyGroup != acc.ProxyGroup || acc.ProxyURL != "") {
		if err := c.Store.SetAccountProxy(ctx, acc.ID, c.ProxyGroup, "", now); err != nil {
			notes = append(notes, "出口组没能更新："+err.Error())
		} else if acc.ProxyURL != "" {
			// 被清掉的是哪个地址，得写出来：那是一个人自己设的地址，
			// 没了就得能照着填回去。
			notes = append(notes, "账号自己那个出口地址（"+acc.ProxyURL+
				"）清掉了，改走导入带的出口组 "+c.ProxyGroup)
		}
	}
	return notes
}

// findIn gives the account row that belongs to this site account.
//
// 三样都问一遍，先问最靠得住的那一样：
//
//   - subject：令牌里带的那个用户 id。一个站点账号一个，重登多少次都是
//     它。清单上的邮箱可能是别名，站点那边报的邮箱又可能一时问不出来，
//     只有它是一直在的。
//   - 站点那边报的邮箱
//   - 清单上写的邮箱
//   - 站点那边报的那个真的 user id（realUserID）：有的账号站点那边报不
//     出邮箱，令牌里也没有 sub，只有它认得出是哪一个站点账号。
//
// 找到两样却指向两个不一样的行，说明库里本来就有同一个站点账号的两行：
// 那不是这次导入造成的，但要说出来——两行都在轮转池里，页面把它列两遍。
//
// 找不到不是错：那就是一个新账号。
func findIn(ctx context.Context, tx store.Tx, mail, email, subject, userID string) (store.Account, []string, error) {
	var notes []string
	var found store.Account
	have := false
	add := func(acc store.Account) {
		if !have {
			found, have = acc, true
			return
		}
		if acc.ID != found.ID {
			notes = append(notes, "库里已经有同一个站点账号的两行了（"+
				found.ID+" 和 "+acc.ID+"）")
		}
	}
	if subject != "" {
		acc, err := tx.AccountBySubject(ctx, subject)
		switch {
		case err == nil:
			add(acc)
		case errors.Is(err, store.ErrNotFound):
		default:
			return store.Account{}, nil, err
		}
	}
	asked := map[string]bool{}
	for _, key := range []string{mail, email} {
		if key == "" || asked[strings.ToLower(key)] {
			continue
		}
		asked[strings.ToLower(key)] = true
		acc, err := tx.AccountByEmail(ctx, key)
		switch {
		case err == nil:
			add(acc)
		case errors.Is(err, store.ErrNotFound):
		default:
			return store.Account{}, nil, err
		}
	}
	// 站点给的那个真的 user id：地址和令牌都认不出是哪一个站点账号的
	// 时候，它是最后那把钥匙。
	if userID != "" {
		acc, err := tx.AccountByUserID(ctx, userID)
		switch {
		case err == nil:
			add(acc)
		case errors.Is(err, store.ErrNotFound):
		default:
			return store.Account{}, nil, err
		}
	}
	return found, notes, nil
}

// keep encrypts the password and writes it down, when there is a
// keeper.
//
// It says whether the password made it: an account that is in without a
// password cannot sign itself back in, and the person has to know.
func (c *Chain) keep(ctx context.Context, id, password string, now time.Time, notes []string) ([]string, bool) {
	if c.Keeper == nil {
		return notes, true
	}
	cipher, err := c.Keeper(password)
	if err != nil {
		return append(notes, "密码没能加密存下："+err.Error()), false
	}
	if err := c.Store.SetPassword(ctx, id, cipher, now); err != nil {
		return append(notes, "密码没能存下："+err.Error()), false
	}
	return notes, true
}

// webError turns what the site said into what the runner needs to
// know: the stage, the site's own code, and whether trying again could
// work.
//
// A wrong password is not worth trying again. A dropped connection is.
//
// ctx 是这一趟导入的上下文：它没了的时候这一趟就结束了，再试也是白试，
// 那一条不该再说「值得再试一次」。
func (c *Chain) webError(ctx context.Context, stage Stage, err error) error {
	dead := ctx.Err() != nil
	var se *webauth.SiteError
	if errors.As(err, &se) {
		pe := &Error{
			Stage: stage,
			Code:  se.Code,
			// ExplainText 是一句话的说法：有错误码就用错误码，没有
			// 就用站点自己那个词（login_required 那种）。
			Message: se.ExplainText(),
		}
		if retryableCode(se.Code) && !dead {
			pe.Retry = true
		}
		return pe
	}
	return &Error{Stage: stage, Message: err.Error(), Retry: !dead}
}

// pollError turns what the token endpoint said into what the runner
// needs to know.
//
// The refusal and the transport failure are told apart: a device code
// that is used up or refused will be refused again, and a dropped
// connection is worth another go. The same goes for a failure one step
// earlier in the chain, and the two are told apart the same way.
//
// 设计文档 §5.3：网络错误、5xx、超时退避重试 2 次（1s、4s）；429 退避
// 30s 重试 1 次；设备码过期不重试。
//
// ctx 是这一趟导入的上下文：只有它自己没了的时候，超时才算「不用再试
// 了」。客户端自己的超时（http.Client.Timeout）报出来的错也是
// context.DeadlineExceeded，但那时候这个上下文还好好的活着——那是连接
// 那边的事，跟前面几步的连接断了是一回事，值得再试一次。
func (c *Chain) pollError(ctx context.Context, err error) error {
	// 上下文没了：再试也是白试，等的那一下也是白等。
	if ctx.Err() != nil {
		return &Error{Stage: StageToken, Message: err.Error()}
	}
	var oe *oauth.Error
	if errors.As(err, &oe) {
		switch oe.Code {
		case "expired_token", "access_denied":
			// 设备码过期了，或者站点不让这个码进来：再来一次也是这个
			// 答案，下一个账号重新开一个设备码就是了。
			return &Error{Stage: StageToken, Message: oe.Error()}
		case "missing_refresh_token":
			// 没有刷新令牌的账号活不过一小时：这是账号的事，不是连接
			// 的事，再来一次也是没有。
			return &Error{Stage: StageToken, Message: oe.Error()}
		}
		if oe.HTTPStatus == http.StatusTooManyRequests {
			// 站点在限流：等久一点再来一次。这样的错试三次没有意义，
			// 去了只是让它更烦。
			return &Error{Stage: StageToken, Message: oe.Error(), Retry: true,
				Wait: rateLimitWait, Tries: 2}
		}
		// 5xx 和别的站点自己的错：再来一次可能就成了。
		return &Error{Stage: StageToken, Message: oe.Error(), Retry: true}
	}
	// 传输上的问题：连接断了、读超时了。
	return &Error{Stage: StageToken, Message: err.Error(), Retry: true}
}

// rateLimitWait is how long to wait when the site says it is limiting
// how much it answers.
//
// 设计文档 §5.3：429、站点风控退避更久（30s）重试 1 次。
const rateLimitWait = 30 * time.Second

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

- [x] **Step 4: 跑测试，确认它过了**

Run: `go test ./internal/provision/ -count=1`
Expected: PASS

- [x] **Step 5: 提交**

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

- [x] **Step 1: 写失败的测试**

创建 `cmd/mmxcode/import_test.go`：

```go
package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mmxcode/internal/config"
	"mmxcode/internal/egress"
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

// TestTheImportReadsTheCreditsOfTheNewAccount says an account that
// just came in has its credits read.
//
// 导入完了要等上一趟扫描（usage.refresh.scan_every，默认 30 秒）才看
// 得到积分，是不让人话的：刚进来的账号马上就读一次账本。
func TestTheImportReadsTheCreditsOfTheNewAccount(t *testing.T) {
	var reads atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/minimax-cloud/api/v1/credit/details":
			reads.Add(1)
			w.Write([]byte(`{"total_count":1,"details":[{"credit_type":1,"granted_amount":"600","consumed_amount":"0","remaining_amount":"600","granted_at_ms":1763000000000,"expire_at_ms":1900000000000}]}`))
		case "/backend/account/token_plan/remains_percent":
			w.Write([]byte(`{"base_resp":{"status_code":0},"model_remains":[]}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	e := testEnvWithServer(t, srv)
	seedAccount(t, e, time.Now().Add(time.Hour))
	e.makeRunner = func(group string, interval time.Duration, keeper func(string) (string, error)) *provision.Runner {
		return &provision.Runner{
			Importer: &provision.Importer{
				Import: func(ctx context.Context, email, password string) (provision.Result, error) {
					return provision.Result{Email: email, OK: true, AccountID: "acc_1"}, nil
				},
			},
			Interval: interval,
			Sleep:    func(context.Context, time.Duration) error { return nil },
		}
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.txt")
	if err := os.WriteFile(path, []byte("a@example.com|pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runImport(context.Background(), e, &out, path, "", time.Millisecond, false); err != nil {
		t.Fatal(err)
	}
	if reads.Load() == 0 {
		t.Fatalf("the credits of the new account were not read:\n%s", out.String())
	}
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

func TestImportStopsAtItsTimeout(t *testing.T) {
	// 整趟导入有个时限：清单很大的时候，人不想等到天亮才知道它卡住了。
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.txt")
	if err := os.WriteFile(path, []byte("a@example.com|pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := aFakeEnv(t)
	e.makeRunner = func(group string, interval time.Duration, keeper func(string) (string, error)) *provision.Runner {
		return &provision.Runner{
			Importer: &provision.Importer{
				Import: func(ctx context.Context, email, password string) (provision.Result, error) {
					// 站点那边没有回答：这一趟就卡在这里。
					<-ctx.Done()
					return provision.Result{Email: email}, ctx.Err()
				},
			},
			Interval: interval,
			Sleep:    func(context.Context, time.Duration) error { return nil },
		}
	}
	cmd := newImportCmd(e)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--file", path, "--timeout", "100ms"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("the import ran past its timeout")
	}
}

func TestAnImportThatIsCutShortKeepsWhatItGot(t *testing.T) {
	// 时限到了的时候，已经进来的账号不能白跑：记录要收尾，页面上这趟
	// 导入不能永远显示成「还在跑」。
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.txt")
	if err := os.WriteFile(path, []byte("a@example.com|pw\nb@example.com|pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := testEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.makeRunner = func(group string, interval time.Duration, keeper func(string) (string, error)) *provision.Runner {
		return &provision.Runner{
			Importer: &provision.Importer{
				Import: func(ctx context.Context, email, password string) (provision.Result, error) {
					if email == "a@example.com" {
						return provision.Result{Email: email, OK: true, AccountID: "acc_1"}, nil
					}
					// 时限到了：这一趟就掐在这里。
					cancel()
					return provision.Result{Email: email}, ctx.Err()
				},
			},
			Interval: interval,
			Sleep:    func(context.Context, time.Duration) error { return nil },
		}
	}
	var out bytes.Buffer
	if err := runImport(ctx, e, &out, path, "", time.Millisecond, false); err == nil {
		t.Fatal("the import ran past its deadline")
	}
	// 已经进来的那个账号要说出来，也要说清楚这趟是被掐断的。
	if !strings.Contains(out.String(), "a@example.com") {
		t.Fatalf("out = %q", out.String())
	}
	if !strings.Contains(out.String(), "截断") {
		t.Fatalf("out = %q, want it to say the run was cut short", out.String())
	}
	runs, err := e.store.ListImportRuns(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("got %d runs", len(runs))
	}
	if runs[0].FinishedAt.IsZero() {
		t.Fatal("the run was never finished")
	}
	if runs[0].OK != 1 {
		t.Fatalf("ok = %d, want the one that got in", runs[0].OK)
	}
	if !strings.Contains(runs[0].Results, "a@example.com") {
		t.Fatalf("results = %q", runs[0].Results)
	}
}

func TestTheImportGivesTheAccountTheGroupItUsed(t *testing.T) {
	// --group 只管登录是不够的：账号自己的活也得从那个口出去，不然站点
	// 看到的是同一个账号从两个地方来。
	e := &env{
		cfg: config.Default(),
		egr: egress.New(map[string]string{"hk": egress.Direct}, ""),
	}
	c := e.chain("hk", nil)
	if c.ProxyGroup != "hk" {
		t.Fatalf("group = %q, want hk", c.ProxyGroup)
	}
}
```

- [x] **Step 2: 跑测试，确认它失败**

Run: `go test ./cmd/mmxcode/ -count=1 -run Import`
Expected: FAIL

- [x] **Step 3: 写实现**

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
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"mmxcode/internal/account"
	"mmxcode/internal/manager"
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

// webTimeout is how long one request to the account site may take.
//
// The site answers in seconds: the device code, the sign-in and the
// approval are all quick. A request that has not answered in half a
// minute is not going to.
const webTimeout = 30 * time.Second

func newImportCmd(e *env) *cobra.Command {
	var file string
	var group string
	var interval time.Duration
	var noStore bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "import",
		Short: "按清单批量登录账号",
		Long: "读一份每行「邮箱|密码」的清单，把这些账号自动登录进来。\n" +
			"密码登录之后默认加密存进库里（密钥在 " + PasswordKeyEnv + "），\n" +
			"没有这个环境变量就登录一次就丢。",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if timeout > 0 {
				// 整趟导入有个时限：清单很大的时候，人不愿意等到
				// 天亮才知道它卡住了。
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, timeout)
				defer cancel()
			}
			return runImport(ctx, e, cmd.OutOrStdout(), file, group, interval, noStore)
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "账号清单（每行 邮箱|密码）")
	cmd.Flags().StringVar(&group, "group", "", "每个账号走哪个出口组；账号自己那个出口地址会被这个组换掉")
	cmd.Flags().DurationVar(&interval, "interval", 3*time.Second, "每个账号之间的间隔")
	cmd.Flags().BoolVar(&noStore, "no-store-password", false, "登录成功就丢密码，不入库")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "整趟导入的时限（0 = 不限）")
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
	entries, bad := provision.Parse(bytes.NewReader(raw))
	fmt.Fprintf(out, "读入 %d 个账号。\n", len(entries))
	for _, b := range bad {
		// 坏行的那句话里没有密码：清单的解析器把密码摘掉了，
		// 这一行是要写进数据库的导入留痕里的。
		fmt.Fprintf(out, "第 %d 行：%s（%s）\n", b.Line, b.Reason, b.Text)
	}
	if len(entries) == 0 {
		return fmt.Errorf("清单里没有能用的账号")
	}

	keeper, err := passwordKeeper(noStore)
	if err != nil {
		return err
	}
	runner, err := e.runner(group, interval, keeper)
	if err != nil {
		return err
	}

	runID := newRunID()
	if e.store != nil {
		if err := e.store.CreateImportRun(ctx, store.ImportRun{
			ID: runID, Source: "cli", StartedAt: time.Now(), Total: len(entries),
		}); err != nil {
			return err
		}
	}

	results, runErr := runner.Run(ctx, entries)
	// 清单里的第几行：出错的账号要能找回去。
	lines := map[string]int{}
	for _, en := range entries {
		lines[strings.ToLower(en.Email)] = en.Line
	}
	ok := 0
	for _, r := range results {
		if r.OK {
			ok++
			fmt.Fprintf(out, "  ✔ %s\t入库成功", r.Email)
			// 账号进来了但密码没存下，也要说：不然下一次掉线时，
			// 这个账号只能等人手工重新入号。
			if r.Message != "" {
				fmt.Fprintf(out, "（%s）", r.Message)
			}
			fmt.Fprintln(out)
			// 刚进来的账号马上读一次账本：不等调度器那一趟（15 分
			// 钟），人才看得到它有多少积分。读不到不算导入失败——
			// 账号是进来了的，账本读不到是另一件事。
			if e.mgr != nil && r.AccountID != "" {
				if _, err := e.mgr.RefreshUsage(ctx, r.AccountID); err != nil {
					fmt.Fprintf(out, "      积分读不到：%v\n", err)
				}
			}
			continue
		}
		fmt.Fprintf(out, "  ✘ %s\t%s：%s", r.Email, r.Stage, r.Message)
		if line, known := lines[strings.ToLower(r.Email)]; known {
			fmt.Fprintf(out, "（第 %d 行", line)
		} else {
			fmt.Fprint(out, "（")
		}
		if r.Code != 0 {
			fmt.Fprintf(out, "错误码 %d", r.Code)
		}
		fmt.Fprintln(out, "）")
	}
	fmt.Fprintf(out, "\n已处理 %d 个，成功 %d，失败 %d。\n", len(results), ok, len(results)-ok)
	switch {
	case keeper != nil:
		fmt.Fprintf(out, "密码已加密入库（%s）。\n", PasswordKeyEnv)
	case noStore:
		fmt.Fprintln(out, "密码用完就丢（不入库）。")
	default:
		// 只有「没设密钥」才要说这件事：--no-store-password 是人自己
		// 说的，说他「没有密钥」是冤枉他。
		fmt.Fprintf(out, "密码没有存进库里（没有 %s）。\n", PasswordKeyEnv)
	}
	if runErr != nil {
		// 说清楚这趟是被掐断的：不是所有账号都失败了，是有人不想让它
		// 再跑下去了。已经进来的那些在上面写着。
		fmt.Fprintf(out, "清单里共 %d 个，还有 %d 个没轮到：这趟导入被中途截断了（%v）。\n",
			len(entries), len(entries)-len(results), runErr)
	}
	if e.store != nil {
		// 收尾的这一笔不能被掐断的上下文掐掉：时限到了的时候，已经
		// 进来的那些账号要留在记录里，不能白跑一场。
		if err := e.store.FinishImportRun(context.WithoutCancel(ctx), runID, time.Now(),
			ok, len(results)-ok, provision.ResultsJSON(results)); err != nil {
			// 收尾没写成也是错，但截断的原因不能跟着它一起没了。
			return errors.Join(runErr, err)
		}
	}
	return runErr
}

// passwordKeeper gives the function that encrypts a password, or nil
// when the password is not to be kept.
//
// The key is not set: no key, no password kept. The key is set but
// wrong: that is a mistake, and the import stops on it -- quietly
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

// reloginFn makes the function that signs an account in again.
//
// The account is already in the database: only the token is new. The
// way out is the account's own proxy group, so that the sign-in comes
// from where the account usually comes from — unless the person
// asked for another group.
func (e *env) reloginFn(group string) manager.ReloginFunc {
	return func(ctx context.Context, acc store.Account, password string) error {
		if group != "" {
			// 人指名的组得是真的：组名写错而默默走全局默认出口，
			// 看着跑通了，其实请求根本没走要去的那条路。
			if _, ok := e.cfg.Proxy.Groups[group]; !ok {
				return fmt.Errorf("no such outlet group %q; known: %s", group, groupNames(e.cfg.Proxy.Groups))
			}
		}
		g := group
		if g == "" {
			g = acc.ProxyGroup
		}
		chain := e.chain(g, nil)
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

// runner makes the batch driver.
//
// The way out is settled once, here, and not once per account: an
// address that does not work is a mistake in the configuration, and it
// is said once rather than once per account.
func (e *env) runner(group string, interval time.Duration, keeper func(string) (string, error)) (*provision.Runner, error) {
	if e.makeRunner != nil {
		return e.makeRunner(group, interval, keeper), nil
	}
	// 出口组先查一遍：组名写错而默默走全局默认出口，是最坏的一种
	// 错——看着跑通了，其实请求根本没走要去的那条路。
	if group != "" {
		if _, ok := e.cfg.Proxy.Groups[group]; !ok {
			return nil, fmt.Errorf("no such outlet group %q; known: %s", group, groupNames(e.cfg.Proxy.Groups))
		}
	}
	if _, err := e.egress(group); err != nil {
		return nil, err
	}
	return &provision.Runner{
		Importer: &provision.Importer{
			Import: func(ctx context.Context, email, password string) (provision.Result, error) {
				chain := e.chain(group, keeper)
				return chain.Import(ctx, email, password)
			},
		},
		Interval: interval,
	}, nil
}

// egress settles the way out of a group.
//
// The group says which way the traffic goes: an empty group is the
// global default.
func (e *env) egress(group string) (http.RoundTripper, error) {
	proxyURL := e.egr.Resolve(store.Account{ProxyGroup: group})
	return e.egr.Transport(proxyURL)
}

// chain makes the thing that does the real work.
//
// The keeper encrypts the password so that it can be kept; a nil one
// throws the password away after the sign-in.
//
// Every call makes a new chain, and a new chain has a new web client:
// the site keeps the session in a cookie, and two accounts sharing one
// session would be signing in as each other.
func (e *env) chain(group string, keeper func(string) (string, error)) *provision.Chain {
	tr, trErr := e.egress(group)
	up := e.up
	hc := &http.Client{Timeout: webTimeout}
	if trErr == nil {
		// The sign-in, the device code and the token all go out the
		// same way: the site sees one account from one address.
		hc.Transport = tr
		up = upstream.New(upstream.Options{Timeout: webTimeout, Transport: tr})
	}
	web := webauth.New(webauth.Options{
		BaseURL: site.AccountHost,
		HTTP:    hc,
		Lang:    site.Lang,
		// 网页那一套的公共 query 里有 timezone_offset：东八区是
		// 28800 秒，正是浏览器给的那个数。
		TZOffset: localTZOffset(),
	})
	c := &provision.Chain{
		Web: web,
		Start: func(ctx context.Context) (*oauth.Device, error) {
			if trErr != nil {
				return nil, trErr
			}
			return oauth.StartDevice(ctx, up, site.AccountURL(site.DeviceCodePath))
		},
		Poll: func(ctx context.Context, d *oauth.Device) (*oauth.Token, error) {
			return d.Poll(ctx, up, site.AccountURL(site.TokenPath))
		},
		Identity: func(ctx context.Context, access, subject string) (account.Identity, error) {
			// 账户信息也从这条路走：换了出口组却用旧出口读账户，
			// 站点看到的是两个地方来的同一个号。
			return e.acc.With(up).Identity(ctx, access, subject)
		},
		Keeper: keeper,
		// 账号从哪个口登进去的，以后它的活就从哪个口出：站点看到的
		// 是同一个账号从同一个地方来，而不是两个地方。
		ProxyGroup: group,
		Now:        time.Now,
		NewID:      newBatchAccountID,
	}
	if e.store != nil {
		c.Store = e.store
	}
	return c
}

// localTZOffset gives the timezone offset in seconds east of UTC.
//
// It is the number the browser sends: the site's web query carries
// -60 × getTimezoneOffset(), and JavaScript counts the minutes west
// of UTC, so the sign is the other way round.
func localTZOffset() int {
	_, offset := time.Now().Zone()
	return offset
}

// newRunID makes the name of one import run.
//
// The time alone is not enough: two runs that start in the same
// millisecond would write the same row, and the second one would be
// refused.
func newRunID() string {
	if r := randomHex(4); r != "" {
		return fmt.Sprintf("run_%d_%s", time.Now().UnixMilli(), r)
	}
	return fmt.Sprintf("run_%d", time.Now().UnixMilli())
}

// newBatchAccountID makes an account id for a batch import.
//
// The time alone is not enough: a batch signs in many accounts in a
// row, and two of them can land in the same millisecond.
func newBatchAccountID() string {
	if r := randomHex(4); r != "" {
		return fmt.Sprintf("acc_%d_%s", time.Now().UnixMilli(), r)
	}
	return fmt.Sprintf("acc_%d", time.Now().UnixMilli())
}

// randomHex gives a few random bytes as hex, or an empty string when
// there are none to give.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}
```

- [x] **Step 4: 跑测试，确认它过了**

Run: `go test ./cmd/mmxcode/ -count=1`
Expected: PASS

- [x] **Step 5: 跑整个测试套件**

Run: `go build ./... && go vet ./... && gofmt -l . && go test ./... -count=1`
Expected: 全绿

- [x] **Step 6: 提交**

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

- [x] **Step 1: 写失败的测试**

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
	if acc.LastError != "" {
		t.Fatalf("last error = %q, want none", acc.LastError)
	}
	tok, err := m.Store.GetToken(ctx, "acc_1")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Access != "new-access" || tok.Refresh != "new-refresh" {
		t.Fatalf("token = %+v, want the new one", tok)
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
	res, err := m.ReloginAll(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if res.Fixed != 1 || res.Failed != 0 {
		t.Fatalf("fixed = %d, failed = %d", res.Fixed, res.Failed)
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

- [x] **Step 2: 跑测试，确认它失败**

Run: `go test ./internal/manager/ ./internal/scheduler/ -count=1 -run Relogin`
Expected: FAIL

- [x] **Step 3: 写实现**

先在 `internal/config/config.go` 里加一节 `Batch`——调度器要读它，没有
这一节这一步编不过。（Task 10 只剩那些 yaml 文件了。）

```go
// Batch is one batch of credits.
//
// The site writes the amounts as strings, sometimes with a thousands
// separator, so they are kept as they come and read by ParseAmount.
type Batch struct {
	// CreditType is CreditTypePurchase or CreditTypeCheckin for the
	// batches the plan counts. Another type is a bucket the plan does
	// not count.
	CreditType int `json:"credit_type"`
	// GrantedAmount is what the batch gave.
	GrantedAmount string `json:"granted_amount"`
	// ConsumedAmount is what of it was used.
	ConsumedAmount string `json:"consumed_amount"`
	// RemainingAmount is what of it is left.
	RemainingAmount string `json:"remaining_amount"`
	// GrantedAtMS is when it was given, in milliseconds.
	GrantedAtMS int64 `json:"granted_at_ms"`
	// ExpiresAtMS is when it runs out, in milliseconds.
	ExpiresAtMS int64 `json:"expire_at_ms"`

	// Granted is what the batch gave, once read.
	Granted float64 `json:"-"`
	// Consumed is what of it was used, once read.
	Consumed float64 `json:"-"`
	// Remaining is what of it is left, once read.
	Remaining float64 `json:"-"`
	// GrantedAt is when it was given.
	GrantedAt time.Time `json:"-"`
	// ExpiresAt is when it runs out.
	ExpiresAt time.Time `json:"-"`
	// HasGrantedAt says the answer carried the time the batch was
	// given. A batch without one has no GrantedAt, not one in 1970.
	HasGrantedAt bool `json:"-"`
	// HasExpiresAt says the answer carried the time the batch runs
	// out. A batch without one cannot be read: the credits of a batch
	// that never expires cannot be told from those of a spent one, and
	// a wrong share is worse than none.
	HasExpiresAt bool `json:"-"`
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

在 `internal/config/config_test.go` 末尾加一个测试（这个包里已经有
`writeFile(path, body)` 这个helper，照着 `TestLoadFile` 的样子写）：

```go
func TestTheBatchSection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := `
batch:
  interval: 5s
  group: clash
  relogin:
    enabled: true
    cooldown: 2h
    max_accounts: 30
`
	if err := writeFile(path, body); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
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
	// reloginLocks serialises the relogin of one account.
	reloginLocks reloginLocks
```

同一账号的重登要一个一个来：调度器的一轮和命令行里的那一次可能撞上，
两个都登的话，站点那边就是两个设备码、两个会话。所以另加一个按账号号
分开的锁：

```go
// reloginLocks serialises the relogin of one account.
//
// 调度器的一轮和人在命令行里的那一次可能撞上。同一个账号被登两次，
// 站点那边就是两个设备码、两个会话、两条新链；后失败的那一次还会把
// 先成功那一次存好的令牌抹掉。
type reloginLocks struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

// lock takes the lock of one account and gives back the way to let it
// go.
func (l *reloginLocks) lock(id string) func() {
	l.mu.Lock()
	if l.m == nil {
		l.m = map[string]*sync.Mutex{}
	}
	mu, ok := l.m[id]
	if !ok {
		mu = &sync.Mutex{}
		l.m[id] = mu
	}
	l.mu.Unlock()
	mu.Lock()
	return mu.Unlock
}

```

并在文件里加类型与两个方法：

失败的那些也要说得出来：只给「成功几个、失败几个」，人得自己去一个一个
看是哪个账号、为什么；而「这一趟根本没跑」（连账号都没列出来）和「都登回
去了」在只看着两个数字的时候是一样的。

```go
// ReloginResult says what one round of re-logins came to.
type ReloginResult struct {
	// Fixed is how many accounts were signed in again.
	Fixed int
	// Failed is how many could not be signed in again.
	Failed int
	// Reasons says why the ones that failed failed: one line per account.
	Reasons []string
}

```

```go
// accountReason says which account it was, and what went wrong.
//
// 谁和为什么都要说：一个账号读不到，人得知道是哪一个，才知道去看哪
// 一行。
func accountReason(acc store.Account, err error) string {
	who := acc.Name
	if who == "" {
		who = acc.ID
	}
	return who + "（" + acc.ID + "）：" + err.Error()
}

```

```go
// ErrNotSignedOut says the account is not signed out.
//
// 重登是给签出的账号准备的。状态不是签出的账号，替它登录没有意义：
// 它手里那条链还能用，重登一次只会把还能用的令牌换掉。
var ErrNotSignedOut = errors.New("这个账号不是签出状态，不用重登")

// ReloginFunc signs an account in again with the password it was
// brought in with.
type ReloginFunc func(ctx context.Context, acc store.Account, password string) error

// Relogin signs an account in again with the password it was brought
// in with.
//
// An account without a password cannot be signed in again: there is
// nothing to sign in with. That is an error, not a silent skip — the
// person who asked wants to know.
//
// Only a signed-out account is signed in again. An account that is
// still active has a token that works, and signing it in again would
// replace that token with a new one for no reason.
func (m *Manager) Relogin(ctx context.Context, accountID string) error {
	acc, err := m.Store.GetAccount(ctx, accountID)
	if err != nil {
		return err
	}
	if acc.Status != store.StatusSignedOut {
		return ErrNotSignedOut
	}
	// 同一个账号的重登要一个一个来：调度器的一轮和命令行里的那一次
	// 可能撞上，两个都登的话，站点那边就是两个设备码、两个会话。
	unlock := m.reloginLocks.lock(accountID)
	defer unlock()
	// 等锁的时候别人可能已经把这个账号登回去了。再登一次是白登，
	// 而且会把刚换好的令牌再换一次。
	acc, err = m.Store.GetAccount(ctx, accountID)
	if err != nil {
		return err
	}
	if acc.Status != store.StatusSignedOut {
		return ErrNotSignedOut
	}
	cipher, err := m.Store.Password(ctx, accountID)
	if err != nil {
		return err
	}
	if cipher == "" {
		return errors.New("这个账号没有存密码，没法自己登回去")
	}
	if len(m.Key) == 0 {
		// 没有密钥，密文就开不了。密文在，说明当初是存过的，是密钥
		// 现在没了——说清楚，别让人以为是账号的问题。
		return errors.New("没有密码的密钥，存着的密文开不了")
	}
	password, err := m.Key.Open(cipher)
	if err != nil {
		return err
	}
	if m.ReloginFn == nil {
		return errors.New("没有地方能重新登录")
	}
	if err := m.ReloginFn(ctx, acc, password); err != nil {
		// 登不回去说的是这次登录失败，不是手里那条链的事。所以令牌
		// 不动：清空它只能毁掉一个还能用的凭据，什么也救不回来。
		// 这里写下的只是失败的原因，账号还是签出的。
		//
		// 写是**有条件**的：另一个进程可能刚把这个账号登回去了，那一
		// 行现在是 active 了，这时候把它写回 signed_out，就是把别人刚
		// 换好的令牌作废掉，还让这个账号显示成登出的状态——手里却握着
		// 一份能用的凭据，轮转池不用它，下一轮又要登一次。
		written, mErr := m.Store.MarkSignedOut(ctx, accountID, err.Error(), time.Now())
		if mErr != nil {
			return errors.Join(err, fmt.Errorf("the failure could not be saved: %w", mErr))
		}
		if !written {
			// 别人已经把它登回去了：原因照样要说给听着的人，但状态
			// 是别人的，不能由这一边改回去。
			return errors.Join(err, ErrNotSignedOut)
		}
		return err
	}
	// 旧的那条链就此作废：这次登录换来的令牌是新的链，旧的刷新令牌
	// 再也不会被发出去了。
	m.Renewer.Forget(accountID)
	return m.Store.UpdateAccountStatus(ctx, accountID, store.StatusActive, "", time.Now())
}

// ReloginAll signs in again every account that is signed out and has
// a password.
//
// It gives back how many it fixed, how many it did not, and why.
//
// 理由是要的：只说「失败 2」，人得自己去一个一个看是哪个账号、为什么。
// 而「一个都没试成」（连账号都没列出来）和「都试了，都登回去了」在只
// 看着两个数字的时候是一样的——上一次那个错就是这样被藏起来的。
//
// An account without a password is skipped and is not a failure: it
// was never brought in with one, so there is nothing to sign in with,
// and that is not this round's fault.
func (m *Manager) ReloginAll(ctx context.Context, limit int) (ReloginResult, error) {
	var out ReloginResult
	accounts, err := m.Store.ListAccounts(ctx)
	if err != nil {
		// 账号都没列出来，这一趟就根本没跑。把它说成「成功 0，失败 0」，
		// 人会以为没什么要登的。
		return out, fmt.Errorf("the accounts could not be listed: %w", err)
	}
	for _, acc := range accounts {
		if acc.Status != store.StatusSignedOut {
			continue
		}
		if limit > 0 && out.Fixed+out.Failed >= limit {
			break
		}
		cipher, err := m.Store.Password(ctx, acc.ID)
		if err != nil || cipher == "" {
			continue
		}
		if err := m.Relogin(ctx, acc.ID); err != nil {
			if errors.Is(err, ErrNotSignedOut) {
				// 别人刚把它登回去了：这不算失败。
				continue
			}
			out.Failed++
			out.Reasons = append(out.Reasons, accountReason(acc, err))
			continue
		}
		out.Fixed++
	}
	return out, nil
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
	res, err := s.Mgr.ReloginAll(ctx, s.Cfg.Batch.Relogin.MaxAccounts)
	if err != nil {
		// 这一趟根本没跑：说清楚，不然看着像「没什么要登的」。
		fmt.Printf("relogin: %v\n", err)
		return
	}
	if res.Fixed > 0 || res.Failed > 0 {
		fmt.Printf("relogin: %d fixed, %d failed\n", res.Fixed, res.Failed)
	}
	for _, r := range res.Reasons {
		fmt.Printf("relogin: %s\n", r)
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
// from where the account usually comes from — unless the person
// asked for another group.
func (e *env) reloginFn(group string) manager.ReloginFunc {
	return func(ctx context.Context, acc store.Account, password string) error {
		if group != "" {
			// 人指名的组得是真的：组名写错而默默走全局默认出口，
			// 看着跑通了，其实请求根本没走要去的那条路。
			if _, ok := e.cfg.Proxy.Groups[group]; !ok {
				return fmt.Errorf("no such outlet group %q; known: %s", group, groupNames(e.cfg.Proxy.Groups))
			}
		}
		g := group
		if g == "" {
			g = acc.ProxyGroup
		}
		chain := e.chain(g, nil)
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
	} else if strings.TrimSpace(os.Getenv(PasswordKeyEnv)) != "" {
		// 密钥设了但是不对：说一声，别让人以为密码存下了。
		fmt.Fprintf(os.Stderr, "warning: %s is set but unusable: %v\n", PasswordKeyEnv, err)
	}
	e.mgr.ReloginFn = e.reloginFn("")
```

设计文档 §7 里还有两样，计划里原先没有，一并做掉：

**一、`import` 的 `--timeout`（整趟导入的时限）。**

在 `cmd/mmxcode/import.go` 的 `newImportCmd` 里加一个 flag，并在 `RunE` 里
用它把命令行的上下文掐断（`runImport` 的参数不要动，测试按那个签名调它）：

```go
	var timeout time.Duration
	// ...
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "整趟导入的时限（0 = 不限）")
	// ...
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, timeout)
				defer cancel()
			}
			return runImport(ctx, e, cmd.OutOrStdout(), file, group, interval, noStore)
		},
```

**二、`mmxcode relogin` 命令**（设计文档 §7 里写着的那一条）：

```go
func newReloginCmd(e *env) *cobra.Command {
	var all bool
	var accountID string
	var group string
	cmd := &cobra.Command{
		Use:   "relogin",
		Short: "把签出的账号登回去",
		Long: "用存着的密码，把签出（signed_out）的账号重新登一遍。\n" +
			"没有存密码的账号登不回去——那个键没有就是没有。\n" +
			"同一个账号不会同时登两遍，不过这只在一个进程里算数：这个命令跟\n" +
			"一个正在跑着的 mmxcode serve 用同一个库的时候，两个进程可能同时登\n" +
			"同一个账号。要是不想撞上，就别一边开着 serve 一边跑这一条。",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRelogin(cmd.Context(), e, cmd.OutOrStdout(), accountID, group, all)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "重登所有签出且有密码的账号")
	cmd.Flags().StringVar(&accountID, "account", "", "只重登这一个账号")
	cmd.Flags().StringVar(&group, "group", "", "走哪个出口组（空 = 各账号自己的组）")
	return cmd
}

// runRelogin signs the signed-out accounts in again.
//
// Either one account or all of them: "all of them" without saying so
// is a surprise waiting to happen, and a named account is not a
// surprise at all.
func runRelogin(ctx context.Context, e *env, out io.Writer, accountID, group string, all bool) error {
	if accountID == "" && !all {
		return fmt.Errorf("要么说 --all，要么给一个 --account")
	}
	if group != "" {
		// 人说了走哪个出口组，就走那个：账号自己的组这时不算数。
		e.mgr.ReloginFn = e.reloginFn(group)
	}
	if accountID != "" {
		if err := e.mgr.Relogin(ctx, accountID); err != nil {
			return err
		}
		fmt.Fprintf(out, "重登完成：%s\n", accountID)
		return nil
	}
	res, err := e.mgr.ReloginAll(ctx, 0)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "重登完成：成功 %d，失败 %d。\n", res.Fixed, res.Failed)
	for _, r := range res.Reasons {
		fmt.Fprintf(out, "  %s\n", r)
	}
	if res.Failed > 0 {
		return fmt.Errorf("有 %d 个账号没登回去", res.Failed)
	}
	return nil
}

```

（`e.reloginFn` 要多一个参数：`e.reloginFn(group string)`。`group` 空的时候
用账号自己的组，非空的时候用这个组——账号自己的组是它平时登录走的那个地
方，重登也该从那里走。）

`reloginFn` 改成：

```go
// reloginFn makes the function that signs an account in again.
//
// The account is already in the database: only the token is new. The
// way out is the account's own proxy group, so that the sign-in comes
// from where the account usually comes from — unless the person
// asked for another group.
func (e *env) reloginFn(group string) manager.ReloginFunc {
	return func(ctx context.Context, acc store.Account, password string) error {
		if group != "" {
			// 人指名的组得是真的：组名写错而默默走全局默认出口，
			// 看着跑通了，其实请求根本没走要去的那条路。
			if _, ok := e.cfg.Proxy.Groups[group]; !ok {
				return fmt.Errorf("no such outlet group %q; known: %s", group, groupNames(e.cfg.Proxy.Groups))
			}
		}
		g := group
		if g == "" {
			g = acc.ProxyGroup
		}
		chain := e.chain(g, nil)
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

并且在 `cmd/mmxcode/root.go` 里挂上命令：

```go
	root.AddCommand(newReloginCmd(e))
```

- [x] **Step 4: 跑测试，确认它过了**

Run: `go test ./internal/manager/ ./internal/scheduler/ ./cmd/mmxcode/ -count=1`
Expected: PASS

- [x] **Step 5: 跑整个测试套件**

Run: `go build ./... && go vet ./... && gofmt -l . && go test ./... -count=1`
Expected: 全绿

- [x] **Step 6: 提交**

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
- Create: `internal/webui/templates/relogin.html`
- Modify: `internal/webui/templates/layout.html`
- Modify: `internal/webui/static/app.js`
- Modify: `cmd/mmxcode/serve.go`

- [x] **Step 1: 写失败的测试**

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
	s.SaveToken(context.Background(), store.Token{
		AccountID: "acc_01", Access: "tok", Refresh: "ref",
		ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
	})
	up := upstream.New(upstream.Options{Timeout: 5 * time.Second})
	groups := map[string]string{"warp": "socks5h://warp:4000"}
	// The account API is pointed at a stand-in server: the check-in
	// route reaches the account API, and a test must never talk to
	// MiniMax itself.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no upstream in the test", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	acc := account.New(up)
	acc.SetBase(srv.URL, srv.URL)
	mgr := manager.New(s, up, acc, egress.New(groups, ""), oauth.NewRenewer())
	// The outlet groups are the configuration's; the resolver is built
	// from the same map, so the page and the resolver agree on which
	// groups there are.
	if len(cfg.Proxy.Groups) == 0 {
		cfg.Proxy.Groups = groups
	}
	w := New(mgr, cfg, s)
	// The import and the relogin talk to the site, and a test must
	// never talk to MiniMax itself: they are fields so that the test
	// can stand in for the network.
	w.Import = func(ctx context.Context, entries []provision.Entry) ([]provision.Result, error) {
		out := make([]provision.Result, 0, len(entries))
		for _, e := range entries {
			out = append(out, provision.Result{Email: e.Email, OK: true, AccountID: "acc_02"})
		}
		return out, nil
	}
	w.Relogin = func(ctx context.Context) (manager.ReloginResult, error) {
		return manager.ReloginResult{Fixed: 1}, nil
	}
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
// TestTheImportPageIsThereWhenNoAPIKeysAreSet says the import page is
// there when the gateway has no key.
//
// 没有 key 的网关只听本机，明文密码不出这台机器，所以上传是开的。
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

```

（后来那道闸拆了，见 Task 12：配了 key 的时候页面也是开着的。）

```go
// TestUploadingAFileAnswersWithAJobID says an upload gets an answer
// that says which run to watch.
//
// 导入在后台跑，回答里给的是那一次的编号。
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

// TestTheReloginIsAskedFor says the relogin can be asked for.
//
// 重登是页面上一个按钮点出来的。
// 重登是页面上一个按钮点出来的。
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

- [x] **Step 2: 跑测试，确认它失败**

Run: `go test ./internal/webui/ -count=1 -run "Import|Relogin"`
Expected: FAIL

- [x] **Step 3: 写实现**

在 `internal/webui/webui.go` 里：

1. `WebUI` 结构加两个字段：

```go
	// Import brings a batch of accounts in. It is a field so that the
	// page and the command share one task, and so that a test can
	// stand in for the network.
	Import func(ctx context.Context, entries []provision.Entry) ([]provision.Result, error)
	// Relogin signs the accounts that are signed out in again.
	Relogin func(ctx context.Context) (fixed, failed int)
	// PasswordNote says what happens to the passwords of the accounts
	// that are brought in.
	//
	// It is empty when the passwords are kept. When they are not, the
	// page has to say so: an account whose password was thrown away
	// cannot sign itself back in, and the person has to know that
	// before the account drops out.
	PasswordNote string
```

2. 路由。在 `ServeHTTP` 的页面那一段里加：

```go
	case "/import":
		// 上传不再因为配了 key 就关着：那把 key 就是后台登录的那把，
		// 能登录的人就是管这个网关的人，明文密码走不走内网是他自己
		// 的选择。上传这个口子跟后台的其它页面一样，钥匙管着。
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

页面上「最近的导入」那几行，成功和失败都是零的时候看不出这一趟出了什
么事——像是什么都没干。所以每一趟还带着它整趟失败的原因。

```go
// importRunView is one import run as the page shows it.
//
// 整趟没成的导入，成功和失败都是零：光看那两个数，看不出这一趟出了什
// 么事，像是这一趟什么都没干。理由得摆在眼前。
type importRunView struct {
	store.ImportRun
	// Whole is why the run failed as a whole, when it did.
	Whole string
}

```

```go
// wholeReason gives the reason a run failed as a whole.
//
// 整趟没成的时候，结果里有一行是整趟的（whole），它说的就是这一趟为什
// 么没成。那一行不是账号：失败的账号数是零，可这一趟确实没成，得说。
func wholeReason(results string) string {
	if strings.TrimSpace(results) == "" {
		return ""
	}
	var list []provision.Result
	if err := json.Unmarshal([]byte(results), &list); err != nil {
		return ""
	}
	for _, r := range list {
		if r.Whole {
			return r.Message
		}
	}
	return ""
}

```

```go
// importData is the import page's data.
func (w *WebUI) importData(ctx context.Context) (map[string]any, error) {
	runs, err := w.Store.ListImportRuns(ctx, 10)
	if err != nil {
		return nil, err
	}
	views := make([]importRunView, 0, len(runs))
	for _, r := range runs {
		views = append(views, importRunView{ImportRun: r, Whole: wholeReason(r.Results)})
	}
	return map[string]any{
		"Runs":         views,
		"PasswordNote": w.PasswordNote,
		"TextLines":    maxImportTextLines,
	}, nil
}

// apiImport takes a list of accounts and brings them in.
//
// 两种交法，交的是同一份清单（每行 邮箱|密码）：
//
//   - 上传一个文件（multipart 的 file 那一栏）
//   - 贴一段清单（JSON 的 {"list": "…"}）
//
// The work goes on in the background, and the answer says which run to
// watch.
//
// The list is read into memory and thrown away afterwards: it holds
// passwords in the clear, and it must not land on the disk.
//
// 配了 key 也不关这个口子了：那把 key 就是后台登录的那把，能进来的人
// 就是管这个网关的人。明文密码走不走内网，是他自己的选择。
func (w *WebUI) apiImport(rw http.ResponseWriter, r *http.Request) {
	if w.Import == nil {
		http.Error(rw, "这个网关没有批量导入", http.StatusNotImplemented)
		return
	}
	var (
		entries []provision.Entry
		bad     []provision.BadLine
	)
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		// 贴过来的一段清单。
		var body struct {
			List string `json:"list"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(rw, "这个请求不是一段清单", http.StatusBadRequest)
			return
		}
		if n := countLines(body.List); n > maxImportTextLines {
			http.Error(rw, fmt.Sprintf("这段清单有 %d 行，最多 %d 行", n, maxImportTextLines),
				http.StatusBadRequest)
			return
		}
		entries, bad = provision.Parse(strings.NewReader(body.List))
	} else {
		// 上传的一个文件。
		//
		// The request has a size limit, and the memory that
		// ParseMultipartForm is given is bigger than it: the list
		// therefore never reaches a temporary file, it stays in
		// memory.
		r.Body = http.MaxBytesReader(rw, r.Body, maxImportBytes)
		if err := r.ParseMultipartForm(maxImportBytes * 2); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(rw, "这个清单太大了", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(rw, "这个请求不是文件上传", http.StatusBadRequest)
			return
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			http.Error(rw, "没有文件", http.StatusBadRequest)
			return
		}
		defer file.Close()
		entries, bad = provision.Parse(file)
	}
	for _, b := range bad {
		// 坏行的那句话里没有密码：清单的解析器把密码摘掉了。
		log.Printf("import: 第 %d 行：%s", b.Line, b.Reason)
	}
	if len(entries) == 0 {
		http.Error(rw, "清单里没有能用的账号", http.StatusBadRequest)
		return
	}
	runID := newImportRunID()
	now := time.Now()
	if err := w.Store.CreateImportRun(r.Context(), store.ImportRun{
		ID: runID, Source: "web", StartedAt: now, Total: len(entries),
	}); err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	go w.runImport(runID, entries)
	// 202：导入在后台跑着，这个回答只说这一次导入的号。
	writeJSONStatus(rw, http.StatusAccepted, map[string]any{"id": runID})
}

// runImport brings a batch in and writes down what came of it.
//
// A run that fails as a whole says why, in two places: in the log, and
// in the results the page shows. The run has no column of its own for
// it, so the reason goes in as a row of its own — otherwise the page
// says only "failed 1" and the operator never learns that the key was
// wrong.
//
// 那一行不是账号，所以它不算进失败的账号数里：一个账号都没处理的这一趟，
// 失败的账号数是零。整趟为什么没成，由那一行自己说。
func (w *WebUI) runImport(runID string, entries []provision.Entry) {
	ctx := context.Background()
	results, err := w.Import(ctx, entries)
	if err != nil {
		log.Printf("import %s: %v", runID, err)
		results = append(results, provision.Result{
			Email:   "（整趟）",
			OK:      false,
			Whole:   true,
			Stage:   stageOf(err),
			Message: "整趟导入没能完成：" + err.Error(),
		})
	}
	ok := 0
	failed := 0
	for _, r := range results {
		switch {
		case r.OK:
			ok++
			// 刚进来的账号马上读一次账本：不等调度器那一趟（15 分
			// 钟），人才看得到它有多少积分。
			w.refreshUsage(ctx, r.AccountID)
		case !r.Whole:
			// 整趟没成的那一行不是账号：它不算进失败的账号数里。
			failed++
		}
	}
	if err := w.Store.FinishImportRun(ctx, runID, time.Now(), ok, failed, provision.ResultsJSON(results)); err != nil {
		log.Printf("import %s: %v", runID, err)
	}
}

// apiRelogin signs the accounts that are signed out in again.
//
// 理由要带回去：只说「失败 2」，人得自己去一个一个看是哪个账号、为什么。
// 这一趟根本没跑的时候也要说，不能让它看着像「没什么要登的」。
func (w *WebUI) apiRelogin(rw http.ResponseWriter, r *http.Request) {
	if w.Relogin == nil {
		http.Error(rw, "这个网关没有重登", http.StatusNotImplemented)
		return
	}
	res, err := w.Relogin(r.Context())
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(rw, map[string]any{
		"fixed":   res.Fixed,
		"failed":  res.Failed,
		"reasons": res.Reasons,
	})
}

```

（`writeJSON` 这个包里多半已经有了；没有就照着别的 API 处理器的样子写一个。`log` 要 import `"log"`。）

写 JSON 的那个函数要能带状态码，而且头得先写：`WriteHeader` 之后头就
定下来了，之后再设的 `Content-Type` 到不了客户端，Go 改成猜正文，
发出去的是 `text/plain`。页面上那个脚本只认 json，猜出来的头会让它
读不出那个 id，上传明明成功了，页面上却写着「失败」。

```go
// writeJSON writes a JSON answer.
func writeJSON(rw http.ResponseWriter, v any) {
	writeJSONStatus(rw, http.StatusOK, v)
}

// writeJSONStatus writes a JSON answer with a status code.
//
// The header is set before the status is written: after WriteHeader the
// header map is frozen, and a Content-Type set afterwards never reaches
// the client. Go sniffs the body instead and says text/plain, and the
// page's script then cannot read the answer — it reads nothing, and the
// upload looks like it failed even though it went through.
func writeJSONStatus(rw http.ResponseWriter, status int, v any) {
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	rw.WriteHeader(status)
	json.NewEncoder(rw).Encode(v)
}

```

4. 页面的模板 `internal/webui/templates/import.html`：

先在 `internal/webui/templates/layout.html` 的导航里加一个入口：

```html
    <a href="/import">导入</a>
```

```html
{{define "content"}}
<section>
  <h1>批量导入</h1>
  {{if .PasswordNote}}
    <p class="muted" id="import-password-note">{{.PasswordNote}}</p>
  {{end}}
  <p class="muted">清单每行一个账号，写成 <code>邮箱|密码</code>；<code>#</code> 开头的是注释。
     不想让明文密码走网络的话，在服务器上跑
     <code>mmxcode import --file accounts.txt</code> 也一样。</p>

  <h2>上传文件</h2>
  <form id="import-form" enctype="multipart/form-data">
    <input type="file" name="file" accept=".txt" required>
    <button type="submit">开始</button>
  </form>

  <h2>或者直接贴清单</h2>
  <form id="import-text-form">
    <p>
      <textarea id="import-text" rows="10" cols="60"
        placeholder="a@example.com|pw&#10;b@example.com|pw"></textarea>
    </p>
    <p class="muted">最多 {{.TextLines}} 行。<span id="import-text-count">0 行</span></p>
    <button type="submit" id="import-text-button">开始</button>
  </form>

  <p class="muted" id="import-status"></p>
  <table id="import-results" hidden>
    <thead><tr><th>账号</th><th>结果</th><th>说明</th></tr></thead>
    <tbody></tbody>
  </table>
  {{if .Runs}}
  <h2>最近的导入</h2>
  <table>
    <thead><tr><th>时间</th><th>来源</th><th>成功</th><th>失败</th><th>说明</th></tr></thead>
    <tbody>
      {{range .Runs}}
      <tr>
        <td>{{.StartedAt.Format "2006-01-02 15:04:05"}}</td>
        <td>{{.Source}}</td>
        <td class="good">{{.OK}}</td>
        <td class="bad">{{.Failed}}</td>
        <td>{{.Whole}}</td>
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

// 贴清单：把那段文字交上去。跟上传文件是同一件事，格式也一样。
//
// 贴的那一段最多一百行：那是给人手写的那一份。超过就不交了 —— 服务端
// 也会拒，两边一样。
document.addEventListener('submit', async (ev) => {
  const form = ev.target.closest('#import-text-form');
  if (!form) return;
  ev.preventDefault();
  const status = document.getElementById('import-status');
  const box = document.getElementById('import-text');
  const list = box ? box.value : '';
  const lines = countListLines(list);
  if (lines > importTextLimit) {
    status.textContent = '失败：这段清单有 ' + lines + ' 行，最多 ' + importTextLimit + ' 行';
    return;
  }
  status.textContent = '导入中……';
  try {
    const res = await fetch('/api/import', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ list: list }),
    });
    const answer = await readAnswer(res);
    if (!answer.ok) throw new Error(messageOf(answer));
    watchImport(answer.json.id);
  } catch (e) {
    status.textContent = '失败：' + e.message;
  }
});

// 贴的那一段有多少行：写的时候就把数摆出来，超了也当场不让人交。
document.addEventListener('input', (ev) => {
  const box = ev.target.closest('#import-text');
  if (!box) return;
  const lines = countListLines(box.value);
  const counter = document.getElementById('import-text-count');
  if (counter) counter.textContent = lines + ' 行';
  const button = document.getElementById('import-text-button');
  if (button) button.disabled = lines > importTextLimit;
});

// countListLines counts the lines of a pasted list.
//
// 末尾那几个空行不算：粘贴的时候多带一个换行是常事，那不该把一行算
// 成两行。跟服务端那个算法一样。
function countListLines(text) {
  const trimmed = (text || '').replace(/[\r\n]+$/, '');
  if (trimmed === '') return 0;
  return trimmed.split('\n').length;
}

// importTextLimit is how many lines a pasted list may have.
//
// 页面上写死的那个数跟服务端的是同一个：服务端是 maxImportTextLines。
const importTextLimit = 100;
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
	writeJSON(rw, map[string]any{
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

7. **重登那一页**（设计文档 §8.1 里写着 `/accounts/relogin`：列出签出且有
   密码的账号，一键重登）。重登页**不受上传那个闸限制**——它不碰明文密码。

   路由，在页面那一段里加：

```go
	case "/relogin":
		// 重登页不接触明文密码：它登的是已经签出的账号，用的是库里
		// 存着的那一份。
		w.page(rw, r, "relogin.html", w.reloginData)
```

   页面要的数据，和一个处理函数：

```go
// reloginData is the relogin page's data.
//
// Only the accounts that are signed out and still have a password are
// listed: the others cannot be signed in again, and saying so is
// better than a button that cannot work.
func (w *WebUI) reloginData(ctx context.Context) (map[string]any, error) {
	accounts, err := w.Store.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0)
	for _, a := range accounts {
		if a.Status != store.StatusSignedOut {
			continue
		}
		cipher, err := w.Store.Password(ctx, a.ID)
		if err != nil || cipher == "" {
			continue
		}
		// 存着的密码是加密的，可它也不该到页面上：这里只说这个账号
		// 还能登回去。
		out = append(out, map[string]any{
			"ID": a.ID, "Name": a.Name, "Email": a.Email, "Error": a.LastError,
		})
	}
	return map[string]any{"SignedOut": out}, nil
}

```

   模板 `internal/webui/templates/relogin.html`：

```html
{{define "content"}}
<section>
  <h1>重登</h1>
  <p class="muted">签出（signed_out）而且还存着密码的账号，可以拿密码重新登一遍。</p>
  <p><button id="relogin-all">全部重登</button> <span class="muted" id="relogin-status"></span></p>
  <table>
    <thead><tr><th>账号</th><th>邮箱</th><th>上次为什么掉的</th></tr></thead>
    <tbody>
      {{range .SignedOut}}
      <tr>
        <td>{{.Name}}</td>
        <td>{{.Email}}</td>
        <td class="err">{{.Error}}</td>
      </tr>
      {{end}}
    </tbody>
  </table>
</section>
{{end}}
```

   `internal/webui/static/app.js` 里加一段（放在 `poll` 那个函数后面）：

```js
// 重登：把签出的账号重新登一遍。
document.addEventListener('click', async (ev) => {
  const button = ev.target.closest('#relogin-all');
  if (!button) return;
  const status = document.getElementById('relogin-status');
  status.textContent = '重登中……';
  try {
    const res = await fetch('/api/relogin', { method: 'POST' });
    const answer = await readAnswer(res);
    if (!answer.ok) throw new Error(messageOf(answer));
    status.textContent = '成功 ' + answer.json.fixed + '，失败 ' + answer.json.failed;
  } catch (e) {
    status.textContent = '失败：' + e.message;
  }
});
```

   导航里也加一条（重登页不受闸限制，所以一直显示）：

```html
    <a href="/relogin">重登</a>
```

   再加一个测试：

```go
// TestTheReloginPageListsTheSignedOutAccounts says the relogin page
// says which accounts are signed out.
//
// 签出了的账号要看得见，才知道该重登谁。
func TestTheReloginPageListsTheSignedOutAccounts(t *testing.T) {
	w, s := newTestWebUIWithConfig(t, config.Default())
	now := time.Now()
	if err := s.CreateAccount(context.Background(), store.Account{
		ID: "acc_09", Name: "掉了的", Email: "b@example.com",
		Status: store.StatusSignedOut, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// 没有密码就登不回去：这样的账号才拿得出手。
	if err := s.SetPassword(context.Background(), "acc_09", "cipher", now); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/relogin", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "b@example.com") {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

```

8. `cmd/mmxcode/serve.go` 里接线：

```go
			ui := webui.New(e.mgr, e.cfg, e.store)
			keeper, keeperErr := passwordKeeper(false)
			// 没有密钥，密码就不入库。页面上得说清楚：账号掉线以后
			// 没有人能自己登回去，人得知道这件事。
			ui.PasswordNote = passwordNote(keeper, keeperErr)
			ui.Import = func(ctx context.Context, entries []provision.Entry) ([]provision.Result, error) {
				runner := e.runner(e.cfg.Batch.Group, e.cfg.Batch.Interval.Duration(), keeper)
				return runner.Run(ctx, entries)
			}
			ui.Relogin = func(ctx context.Context) (int, int) {
				return e.mgr.ReloginAll(ctx, e.cfg.Batch.Relogin.MaxAccounts)
			}
```

（`provision` 要 import。）

再加一个函数，说清楚密码的去处：

```go
// passwordNote says what happens to the passwords, when they are not
// kept.
//
// 没有密钥的时候密码就不入库，页面上得说清楚：账号掉线以后没有人能
// 自己登回去。密钥有问题的时候导入自己就停了，页面不用多说。
func passwordNote(keeper func(string) (string, error), keeperErr error) string {
	if keeperErr != nil || keeper != nil {
		return ""
	}
	return "密码没有存进库里（没有 " + PasswordKeyEnv + "）：账号掉线以后没有人能自己登回去。"
}

```

- [x] **Step 4: 跑测试，确认它过了**

Run: `go test ./internal/webui/ ./cmd/mmxcode/ -count=1`
Expected: PASS

- [x] **Step 5: 提交**

```bash
git add internal/webui cmd/mmxcode
git commit -m "feat(webui): 批量导入页和重登页

上传只在没有配 API key 的时候开放：配了 key 就说明别的机器能进来，
明文密码不该从那儿过。

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

#### 几处容易踩的地方

- **状态码和 `Content-Type` 的先后。** 这个包里的 `writeJSON(rw, v)` 自己设
  `Content-Type`，但它**不写状态码**。要 202 就得先 `rw.WriteHeader(202)`——
  那样写的话，`writeJSON` 后设的那个头就被 Go 丢掉了（`WriteHeader` 之后
  头就定死了），回答会以 `text/plain` 出去，页面上的 `readAnswer` 就不把它
  当 JSON，`watchImport` 读 `answer.json.id` 时炸掉，页面显示"失败"，其实
  上传是成的。所以：**先把头设好，再写状态码**。
- **测这个要用真的服务器。** `httptest.NewRecorder` 的头不会像真的服务器那样
  定死，所以用 Recorder 测不出上面那个毛病。要起一个 `httptest.NewServer`，
  看客户端真正收到的是什么。
- **上传不许落盘。** `ParseMultipartForm(4<<20)` 这样的内存上限下，超过上限的
  清单会真的写到 `os.TempDir()` 里去——明文密码就落盘了，违反设计 §8.4。要用
  `http.MaxBytesReader` 把请求体卡住，并且给解析器的内存**比请求体的上限大**，
  溢出的那条路才走不到。
- **`e.runner` 返回两个值**（`(*provision.Runner, error)`），不能一句
  `runner := e.runner(...)` 就算完。
- **重登那一页只列还存着密码的账号。** 没有密码的账号登不回去，列出来配一个
  按不动的按钮比不列更糟。测试里那个账号得先 `SetPassword`。
- **密钥是坏的，要停下来。** `passwordKeeper` 的契约是：密钥没设 = 密码用完就
  丢；密钥设了但是坏的 = 停下来。把错误丢掉（`keeper, _ := ...`）会悄悄造出
  一库谁也登不回去的账号。

---

### Task 10: 配置与部署

**Files:**
- Modify: `config.example.yaml`
- Modify: `config.docker.yaml`
- Modify: `docker-compose.yml`
- Modify: `.env.example`

（`internal/config/config.go` 里的 `Batch` 一节已经在 Task 8 加了——调
度器要读它，那一步没有它编不过。这一步只管那些 yaml 文件。）

- [x] **Step 1: 看看现在是什么样**

Run: `cat config.example.yaml config.docker.yaml docker-compose.yml .env.example`
Expected: 看得见现在的样子。`.env.example` 已经有了，compose 读的那几个变量都在
里面——这一步是往里加一行，不是新建一个文件。

- [x] **Step 2: 写实现**

（`internal/config/config.go` 里的 `Batch` 与 `Relogin` 已经在 Task 8 加好了，
调度器要读它，那一步没有它编不过。这一步不碰 Go 代码，只管 yaml 和部署。）

在 `config.example.yaml` 和 `config.docker.yaml` 里各加同样的一节（两个文件
的其它内容一样，节也该一样）：

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

- [x] **Step 4: 跑测试，确认它过了**

Run: `go test ./internal/config/ -count=1`
Expected: PASS

- [x] **Step 5: 跑整个测试套件**

Run: `go build ./... && go vet ./... && gofmt -l . && go test ./... -count=1`
Expected: 全绿

- [x] **Step 6: 提交**

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

- [x] **Step 1: README 补上这一节**

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

- [x] **Step 2: spec 补上「做完了什么样」**

在设计文档的末尾加一节，记下实现时和设计不一样的地方（如果有）。没有就不加。

- [x] **Step 3: 把计划跟代码对起来**

改过代码之后，计划的代码块要跟仓库里的代码对得上。用：

```bash
python -I docs/superpowers/tools/refresh_plan_blocks.py docs/superpowers/plans/2026-11-14-mmxcode-batch-import.md .
python -I docs/superpowers/tools/sync_plan.py docs/superpowers/plans/2026-11-14-mmxcode-batch-import.md internal/sign/sign.go internal/sign/sign_test.go internal/secret/secret.go internal/secret/secret_test.go internal/webauth/webauth.go internal/webauth/rsa.go internal/webauth/webauth_test.go internal/provision/provision.go internal/provision/provision_test.go internal/provision/chain.go internal/provision/chain_test.go cmd/mmxcode/import.go cmd/mmxcode/import_test.go
python -I docs/superpowers/tools/check_plan_files.py docs/superpowers/plans/2026-11-14-mmxcode-batch-import.md .
python -I docs/superpowers/tools/check_plan_fences.py docs/superpowers/plans/2026-11-14-mmxcode-batch-import.md
```

四个脚本各管一件事：

- `refresh_plan_blocks.py` 把**片段**里的每个声明按它落地时的样子改过来
  （仓库里有另一个版本，就换成那个版本）。
- `sync_plan.py` 把**整文件**的代码块按字节换成仓库里的文件。
- `check_plan_files.py` 查两件事：整文件的块是不是都还能过 `gofmt`，以及计划里
  的每个声明是不是都在仓库里原样存在（片段也查）。
- `check_plan_fences.py` 查代码块里有没有混进中文散文——那种块编不过，错的
  提示又看不出是哪来的。

这个功能**自己写的**那几个文件（`sign`、`secret`、`webauth`、`provision`、
`import` 及其测试）在计划里是整文件带过来的，逐字节相同；只是改过的那些
（`store`、`webui`、`serve`、`config`、`manager`）在计划里是**改的那一部分**，
一个声明一个声明地对。

工具在 `docs/superpowers/tools/` 里，是这个项目用来对计划的脚本。

- [x] **Step 4: 最后的检查**

```bash
go build ./... && go vet ./... && gofmt -l . && go test ./... -count=1
```

全绿，`gofmt -l` 没有输出。

- [x] **Step 5: 提交**

```bash
git add README.md docs/
git commit -m "docs: 批量导入怎么用

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 12: 上传的那个闸：配了 key 也照样开着

**Files:**
- Modify: `internal/webui/webui.go`
- Modify: `internal/webui/webui_test.go`
- Modify: `internal/webui/templates/import.html`
- Modify: `internal/webui/templates/layout.html`
- Modify: `docs/superpowers/specs/2026-11-14-mmxcode-batch-import-design.md`
- Modify: `README.md`

Task 9 给上传那道闸定的规矩是「配了 API key 就关上传」（规格 §8.3）。那时的想法是：配了 key 就说明别的机器能进来，明文密码不该从那儿过。

后来那把 key 的意思变了：它是**后台登录密钥**，是「只有拿着它的人能进后台」，不是「我想让整条街都用这个网关」。规矩跟着变 —— 上传一直开着，能不能传看的是有没有登录。

为什么反过来了：后台登录密钥自己也是明文走同一个 HTTP 口过去的。它过去了，明文密码就不算「不该过」的东西 —— 关上传而放密钥过去，是拿一个已经过去的票去挡另一个。

- [x] **Step 1: 先写测试（红的）**

三处都要开：页面、导航里的链接、上传本身。原来的两条测试是反着写的（页面 404、上传 403），把它们改成「开着」。

```go
// TestTheImportPageIsThereWhenAPIKeysAreSet says the import page is
// there even when the gateway has a key.
//
// 配了 key 就把上传关掉，是从前的规矩，现在不关了：那把 key 就是后台
// 登录的那把，能登录的人就是管这个网关的人。明文密码走不走内网，是他
// 自己的选择。
func TestTheImportPageIsThereWhenAPIKeysAreSet(t *testing.T) {
	cfg := config.Default()
	cfg.Server.APIKeys = []string{"sk-x"}
	w, _ := newTestWebUIWithConfig(t, cfg)
	req := httptest.NewRequest(http.MethodGet, "/import", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "批量导入") {
		t.Fatalf("body = %q", body)
	}
	// 上传的口子得在页面上：只剩一句「这里不开放」不算数。
	if !strings.Contains(body, "import-form") {
		t.Fatalf("the upload form is not on the page: %q", body)
	}
	if strings.Contains(body, "不开放上传") {
		t.Fatalf("the page still says the upload is shut: %q", body)
	}
}

```

```go
// TestTheImportLinkIsInTheNavWhenAPIKeysAreSet says the nav has the
// import link even when the gateway has a key.
//
// 导航里没有那个链接，页面上就等于没有这个功能：人得自己敲地址。
func TestTheImportLinkIsInTheNavWhenAPIKeysAreSet(t *testing.T) {
	cfg := config.Default()
	cfg.Server.APIKeys = []string{"sk-x"}
	w, _ := newTestWebUIWithConfig(t, cfg)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `href="/import"`) {
		t.Fatalf("the nav has no import link: %q", rec.Body.String())
	}
}

```

```go
// TestTheImportIsTakenWithAPIKeysAreSet says the upload itself is
// taken even when the gateway has a key.
//
// 页面开着而接口关着，那个功能等于没有。
func TestTheImportIsTakenWithAPIKeysAreSet(t *testing.T) {
	cfg := config.Default()
	cfg.Server.APIKeys = []string{"sk-x"}
	w, _ := newTestWebUIWithConfig(t, cfg)
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
}

```

- [x] **Step 2: 看它们红**

```bash
go test ./internal/webui/ -count=1 -run "TestTheImport"
```

红的原因有三条，各对各的：页面 404、导航里没有链接、上传 403。

- [x] **Step 3: 拆掉那三道闸**

`internal/webui/webui.go` 的路由里，页面不再 404：

```go
	case "/import":
		// 上传不再因为配了 key 就关着：那把 key 就是后台登录的那把，
		// 能登录的人就是管这个网关的人，明文密码走不走内网是他自己
		// 的选择。上传这个口子跟后台的其它页面一样，钥匙管着。
		w.page(rw, r, "import.html", w.importData)
```

同一个文件里，`apiImport` 不再 403：

```go
// apiImport takes a list of accounts and brings them in.
//
// 两种交法，交的是同一份清单（每行 邮箱|密码）：
//
//   - 上传一个文件（multipart 的 file 那一栏）
//   - 贴一段清单（JSON 的 {"list": "…"}）
//
// The work goes on in the background, and the answer says which run to
// watch.
//
// The list is read into memory and thrown away afterwards: it holds
// passwords in the clear, and it must not land on the disk.
//
// 配了 key 也不关这个口子了：那把 key 就是后台登录的那把，能进来的人
// 就是管这个网关的人。明文密码走不走内网，是他自己的选择。
func (w *WebUI) apiImport(rw http.ResponseWriter, r *http.Request) {
	if w.Import == nil {
		http.Error(rw, "这个网关没有批量导入", http.StatusNotImplemented)
		return
	}
	var (
		entries []provision.Entry
		bad     []provision.BadLine
	)
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		// 贴过来的一段清单。
		var body struct {
			List string `json:"list"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(rw, "这个请求不是一段清单", http.StatusBadRequest)
			return
		}
		if n := countLines(body.List); n > maxImportTextLines {
			http.Error(rw, fmt.Sprintf("这段清单有 %d 行，最多 %d 行", n, maxImportTextLines),
				http.StatusBadRequest)
			return
		}
		entries, bad = provision.Parse(strings.NewReader(body.List))
	} else {
		// 上传的一个文件。
		//
		// The request has a size limit, and the memory that
		// ParseMultipartForm is given is bigger than it: the list
		// therefore never reaches a temporary file, it stays in
		// memory.
		r.Body = http.MaxBytesReader(rw, r.Body, maxImportBytes)
		if err := r.ParseMultipartForm(maxImportBytes * 2); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(rw, "这个清单太大了", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(rw, "这个请求不是文件上传", http.StatusBadRequest)
			return
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			http.Error(rw, "没有文件", http.StatusBadRequest)
			return
		}
		defer file.Close()
		entries, bad = provision.Parse(file)
	}
	for _, b := range bad {
		// 坏行的那句话里没有密码：清单的解析器把密码摘掉了。
		log.Printf("import: 第 %d 行：%s", b.Line, b.Reason)
	}
	if len(entries) == 0 {
		http.Error(rw, "清单里没有能用的账号", http.StatusBadRequest)
		return
	}
	runID := newImportRunID()
	now := time.Now()
	if err := w.Store.CreateImportRun(r.Context(), store.ImportRun{
		ID: runID, Source: "web", StartedAt: now, Total: len(entries),
	}); err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	go w.runImport(runID, entries)
	// 202：导入在后台跑着，这个回答只说这一次导入的号。
	writeJSONStatus(rw, http.StatusAccepted, map[string]any{"id": runID})
}

```

页面的数据里那个 `UploadOpen` 没有用了（它只用来分开两段模板），去掉：

```go
	return map[string]any{
		"Runs":         views,
		"PasswordNote": w.PasswordNote,
	}, nil
```

`internal/webui/templates/import.html`：上传的口子一直摆着，命令行那条留在旁边当另一个选择：

```html
{{define "content"}}
<section>
  <h1>批量导入</h1>
  {{if .PasswordNote}}
    <p class="muted" id="import-password-note">{{.PasswordNote}}</p>
  {{end}}
  <p class="muted">清单每行一个账号，写成 <code>邮箱|密码</code>；<code>#</code> 开头的是注释。
     不想让明文密码走网络的话，在服务器上跑
     <code>mmxcode import --file accounts.txt</code> 也一样。</p>
  <form id="import-form" enctype="multipart/form-data">
    <input type="file" name="file" accept=".txt" required>
    <button type="submit">开始</button>
  </form>
  <p class="muted" id="import-status"></p>
  <table id="import-results" hidden>
    <thead><tr><th>账号</th><th>结果</th><th>说明</th></tr></thead>
    <tbody></tbody>
  </table>
  {{if .Runs}}
```

`internal/webui/templates/layout.html`：导航里那个链接不再按 key 藏：

```html
    <a href="/import">导入</a>
```

- [x] **Step 4: 规格和 README 跟着改**

规格 §8.3 改成「上传的闸：登录过的人就能传」，把为什么反过来说清楚。README 里那两句「配了 key 的时候页面上没有上传入口」和「`/import` 批量导入（没配 key 时才开）」都要改掉。

- [x] **Step 5: 验收**

```bash
gofmt -l . && go build ./... && go vet ./... && go test ./... -count=1
```

---

---

### Task 13: 清单的两种交法 —— 上传文件，或者贴一段

**Files:**
- Modify: `internal/webui/webui.go`
- Modify: `internal/webui/webui_test.go`
- Modify: `internal/webui/templates/import.html`
- Modify: `internal/webui/static/app.js`
- Modify: `docs/superpowers/specs/2026-11-14-mmxcode-batch-import-design.md`
- Modify: `README.md`

手写三个账号不该先造一个文件、上传它、再等它传完。页面上再给一种交法：一个长输入框，贴进去就行。

交的是同一种清单（每行 `邮箱|密码`），所以服务端要认两种请求：

- 上传文件：`multipart` 的 `file` 那一栏，跟从前一样
- 贴一段：`application/json`，`{"list": "…"}`

贴的那一段**最多 100 行**：那是给人手写的那一份，一百行够写一百个账号。上传的那一份是批量那一份，它自己的大小上限还是 8 MB，行数不管。超过 100 行就拒掉（服务端 400，页面上也不让人交 —— 两边一样，光靠一边不算数）。

- [x] **Step 1: 先写测试（红的）**

三条：贴一段收下、100 行收下、101 行拒掉；再加一条：页面上有那个框。

```go
// TestTheImportTakesAPastedList says a list that is pasted in is
// taken as well as one that is uploaded.
//
// 页面上有两种交法：上传一个文件，或者贴一段清单。交的是同一件事：
// 每行 邮箱|密码。
func TestTheImportTakesAPastedList(t *testing.T) {
	w, s := newTestWebUIWithConfig(t, config.Default())
	body := strings.NewReader(`{"list":"a@example.com|pw\nb@example.com|pw\n"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/import", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d, body = %q", rec.Code, rec.Body.String())
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	// The import goes on in the background, so the run is waited for.
	var run store.ImportRun
	var err error
	for i := 0; i < 100; i++ {
		run, err = s.GetImportRun(context.Background(), out.ID)
		if err == nil && !run.FinishedAt.IsZero() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if run.FinishedAt.IsZero() {
		t.Fatalf("the run did not finish: %+v", run)
	}
	if run.Total != 2 || run.OK != 2 {
		t.Fatalf("run = %+v", run)
	}
}

```

```go
// TestTheImportTakesAHundredPastedLines says a list of exactly a
// hundred lines is taken.
//
// 上限是「最多 100 行」，100 行本身得收。
func TestTheImportTakesAHundredPastedLines(t *testing.T) {
	w, _ := newTestWebUIWithConfig(t, config.Default())
	list := strings.Repeat("a@example.com|pw\n", 100)
	body := strings.NewReader(`{"list":` + strconv.Quote(list) + `}`)
	req := httptest.NewRequest(http.MethodPost, "/api/import", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d, body = %q", rec.Code, rec.Body.String())
	}
}

```

```go
// TestTheImportRefusesALongerPastedList says a pasted list that is
// longer than a hundred lines is refused.
//
// 贴的那段是给人手写的那一份：超过 100 行就说不清了，宁可拒掉。
func TestTheImportRefusesALongerPastedList(t *testing.T) {
	w, _ := newTestWebUIWithConfig(t, config.Default())
	list := strings.Repeat("a@example.com|pw\n", 101)
	body := strings.NewReader(`{"list":` + strconv.Quote(list) + `}`)
	req := httptest.NewRequest(http.MethodPost, "/api/import", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, body = %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "100") {
		t.Fatalf("the body does not say the limit: %q", rec.Body.String())
	}
}

```

- [x] **Step 2: 看它们红**

```bash
go test ./internal/webui/ -count=1 -run "TestTheImport"
```

红的原因各对各的：贴的那段被当成文件上传（400「这个请求不是文件上传」），页面上没有那个框。

- [x] **Step 3: 服务端认两种请求**

`internal/webui/webui.go` 里 `apiImport` 分成两路：JSON 那路是贴过来的清单，多一行那把上限；multipart 那路跟从前一样。

```go
// maxImportTextLines is how many lines a pasted list may have.
//
// 贴的那一段是给人手写的那一份：一百行够写一百个账号，再多就该上传一
// 个文件了。上传的那一份是批量那一份，它自己的大小上限是
// maxImportBytes，行数不管。
const maxImportTextLines = 100

```

```go
// countLines counts the lines of a pasted list.
//
// 末尾那几个空行不算：粘贴的时候多带一个换行是常事，那不该把一行算
// 成两行。
func countLines(text string) int {
	text = strings.TrimRight(text, "\r\n")
	if text == "" {
		return 0
	}
	return strings.Count(text, "\n") + 1
}

```

- [x] **Step 4: 页面上那个框**

`internal/webui/templates/import.html`：上传文件那一个口子上面是「上传文件」，下面新加一段「或者直接贴清单」，一个 `textarea` 加一个按钮，旁边写着最多多少行。

`internal/webui/static/app.js`：贴的那一段交上去（`POST /api/import`，JSON），写的时候就把行数摆出来，超了把按钮禁掉。

- [x] **Step 5: 验收**

```bash
gofmt -l . && go build ./... && go vet ./... && node --check internal/webui/static/app.js && go test ./... -count=1
```

---

---

### Task 14: 默认组那条 —— 认得宽一点，把站点答的原文带出来

**Files:**
- Modify: `internal/webauth/webauth.go`
- Modify: `internal/webauth/webauth_test.go`
- Modify: `internal/provision/chain.go`
- Modify: `docs/superpowers/specs/2026-11-14-mmxcode-batch-import-design.md`

真账号上跑出来的第一处真问题：那一个号的导入死在**批准设备码**那一步，报的是
`device authorize: there is no default group to approve` —— 是我们自己抛的
（`code: 0`，站点没拒绝）。

死在批准那一步说明前面的都过了：探过了，**密码登录过了**（站点认了邮箱和密码，
给了会话）。所以跟代理没关系 —— 三步（设备码、密码登录、批准）都走
`e.egress(group)` 那个 transport。

那么这个错属于哪一种：

- **真的没有默认组** —— 站点的前端也是这么处理的，它自己会写「A default group is
  required to approve authorization.」。那这个号得去站点上把一个组设成默认的
- **我们没认出来** —— 规格 §2.4 里那个形状（`{…设备信息…, groups:[{id:…,
  is_default:true}]}`）是照着站点的 JS 读出来的，**从没在真账号上验过**；设计
  文档自己的「待实测」第 3 条就是这件事

第二种是实打实可能的：我们只认 `is_default` 是**真的布尔**。站点回的是 `1` 或
者 `"true"`，症状一模一样。

- [x] **Step 1: 先写测试（红的）**

```go
// TestTheDefaultGroupIsFoundEvenWhenItIsNotARealBool says a default
// group that is written as a number or as a string is found too.
//
// is_default 到底回的是不是真的布尔，没人验过（设计文档「待实测」第
// 3 条）。认错一个组和认不出一个组，后果一样：账号批不了设备码。
func TestTheDefaultGroupIsFoundEvenWhenItIsNotARealBool(t *testing.T) {
	for _, written := range []string{`1`, `"true"`, `true`} {
		f := newFakeSite(t)
		f.groups = `{"code":0,"data":{"groups":[{"id":"g1","is_default":` + written + `}]}}`
		c := f.client(t)
		f.login(t, c)
		if err := c.Approve(context.Background(), "ABCD-1234"); err != nil {
			t.Fatalf("is_default = %s: %v", written, err)
		}
		if f.approved["group_id"] != "g1" {
			t.Fatalf("is_default = %s: group_id = %v", written, f.approved["group_id"])
		}
	}
}
```

- [x] **Step 2: 看它红**

```bash
go test ./internal/webauth/ -count=1
```

红的原因：`is_default = 1` 那个也找不到默认组 —— 我们只认真布尔。

- [x] **Step 3: 三处改动**

`internal/webauth/webauth.go`：

```go
// isDefaultGroup says whether a group is the site's default one.
//
// 真假有三种写法：真的布尔、1、"true"。站点到底回哪一种，没人验过
// （设计文档「待实测」第 3 条）。认错一个组和认不出一个组，后果一样：
// 账号批不了设备码。
func isDefaultGroup(g map[string]any) bool {
	switch v := g["is_default"].(type) {
	case bool:
		return v
	case float64:
		return v != 0
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		return s == "true" || s == "1"
	}
	return false
}
```

```go
// noDefaultGroupError is that answer, with the site's own words in it.
//
// 它带上站点答的那个形状：默认组是真的没有，还是我们没认出来，看一眼
// 那个形状就知道（设计文档「待实测」第 3 条）。
type noDefaultGroupError struct {
	raw string
}

func (e noDefaultGroupError) Error() string {
	return "这个账号没有默认组，设备码批不了（去站点把一个组设成默认的再来）。站点答的是：" + e.raw
}

// Is makes errors.Is(err, ErrNoDefaultGroup) true for it.
func (e noDefaultGroupError) Is(target error) bool { return target == ErrNoDefaultGroup }
```

还有一个兜底：没有默认组、但**只有一个组**的时候，就用那一个。没得选的时候
不猜 —— 站点后头收不收是站点的事，让它答，比我们先拒了好。有好几个组的时候
还是拒（猜错了就是把账号批进别的组里去了）。

`internal/provision/chain.go`：那个分支的注释跟着改（它现在只管「好几个组、
没有一个是默认的」这一种）。

- [x] **Step 4: 验收**

```bash
gofmt -l . && go build ./... && go vet ./... && go test ./... -count=1
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
10. 同一个账号导两次，站点那边认不认。我们这边是一行：第二次导入会把这一行的
    令牌换掉。但站点那边——第二次登录换来的令牌会不会把第一次那把 refresh
    token 作废——只有真账号能试出来
11. **令牌里那个 `sub` 是不是每一次登录都是同一个。** 我们整套去重都压在这个
    前提上：导入按它找那一行，签出以后还留着它，下一次导入才不会把这个账号
    当成新账号再插一行
12. **站点那边报出来的地址能不能拿来登录。** `email` 那一栏写的是站点自己认的
    地址，掉线以后重登用的就是它；清单上那个地址（登录时用的）留在导入的结
    果里，库里没有那一份
