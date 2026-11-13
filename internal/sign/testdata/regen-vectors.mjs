// Regenerate the golden signature vectors in sign_test.go.
//
// The vectors are the contract between our Go signature and the one MiniMax
// Code sends: if these agree, our signature is byte-identical.
//
// The signed() function below is copied VERBATIM from the reference
// implementation, so that running this script proves the vectors against the
// reference itself and not against our own reading of it.
//
//   Source: https://github.com/magpie-community/plugins/blob/main/packages/minimax/index.mjs
//   (magpie-community/plugins, packages/minimax/index.mjs)
//
// Run it with Node 18 or newer:
//
//	node internal/sign/testdata/regen-vectors.mjs
//
// It prints one JSON object per vector. Every yy and x-signature it prints
// must equal the one in sign_test.go.

import { createHash } from "node:crypto"

const md5 = (s) => createHash("md5").update(s).digest("hex")
const SIGN_SECRET = "I*7Cf%WZ#S&%1RlZJ&C2" // MiniMax Code's request signing (x-signature)

function signed(site, path, { access, userID, body, origin = site.agent, query, now = Date.now(), tz = new Date().getTimezoneOffset() * -60, platform = process.platform } = {}) {
  const url = new URL(path, origin)
  url.search = new URLSearchParams({
    device_platform: "mcode", biz_id: "3", app_id: "3001", version_code: "22201",
    ...query,
    unix: String(now), timezone_offset: String(tz), sys_language: site.lang, lang: site.lang,
    device_id: "0", os_name: platform, browser_name: "mcode", user_id: String(userID ?? "").trim() || "0", client: "mcode",
  }).toString()
  const at = url.pathname + url.search
  const ts = Math.floor(now / 1000)
  const json = body === undefined ? undefined : JSON.stringify(body)
  const headers = {
    Accept: "application/json",
    "Content-Type": "application/json",
    "User-Agent": "MiniMaxCode",
    Authorization: "Bearer " + access,
    yy: json === undefined ? md5(`${encodeURIComponent(at)}_{}${md5(String(now))}ooui`) : md5(`${encodeURIComponent(at)}_${json}${md5(String(now))}ooui`),
    "x-timestamp": String(ts),
    "x-signature": json === undefined ? md5(`${ts}${SIGN_SECRET}`) : md5(`${ts}${SIGN_SECRET}${json}`),
  }
  return { url: url.toString(), init: { method: json === undefined ? "GET" : "POST", headers, ...(json === undefined ? {} : { body: json }) } }
}


export { signed }

const site = { agent: "https://agent.minimax.io", lang: "en" }

const cases = [
  {
    name: "identity GET",
    path: "/v1/api/user/info",
    opts: {
      access: "tok_1", userID: "1234567890",
      now: 1763150000000, tz: -480, platform: "linux",
    },
  },
  {
    name: "signin status GET",
    path: "/minimax-cloud/api/v1/signin/status",
    opts: {
      access: "tok_2", userID: "1234567890",
      now: 1763150001000, tz: -480, platform: "linux",
      query: { device_platform: "web", is_desktop: "1", desktop_version: "0.6.12" },
    },
  },
  {
    name: "signin claim POST",
    path: "/minimax-cloud/api/v1/signin/claim",
    opts: {
      access: "tok_3", userID: "1234567890",
      body: {}, now: 1763150002000, tz: -480, platform: "linux",
      query: { device_platform: "web", is_desktop: "1", desktop_version: "0.6.12" },
    },
  },
  {
    name: "credit details GET",
    path: "/minimax-cloud/api/v1/credit/details",
    opts: {
      access: "tok_4", userID: "1234567890",
      now: 1763150003000, tz: -480, platform: "linux",
      query: {
        device_platform: "web", is_desktop: "1", desktop_version: "0.6.12",
        page_number: "1", page_size: "20", include_expired: "true",
      },
    },
  },
  {
    name: "membership POST",
    path: "/matrix/api/v1/commerce/get_membership_info",
    opts: {
      access: "tok_5", userID: "1234567890",
      body: {}, now: 1763150004000, tz: -480, platform: "linux",
    },
  },
  {
    name: "workspace POST",
    path: "/matrix/api/v1/user/get_user_extra_info",
    opts: {
      access: "tok_6", userID: "",
      body: {}, now: 1763150005000, tz: 0, platform: "darwin",
    },
  },
]

for (const c of cases) {
  const r = signed(site, c.path, c.opts)
  const h = r.init.headers
  console.log(JSON.stringify({
    name: c.name,
    url: r.url,
    yy: h.yy,
    ts: h["x-timestamp"],
    sig: h["x-signature"],
    method: r.init.method,
    body: r.init.body ?? null,
  }))
}
