// 用 CDP 点一下页面：看看筛账号、飘出来的框和右下角那句话是不是真的能用。
//
// 用法：node scripts/uicheck.mjs http://127.0.0.1:8793/
import { spawn } from 'node:child_process'
import { setTimeout as sleep } from 'node:timers/promises'

const chrome = 'C:/Program Files/Google/Chrome/Application/chrome.exe'
const base = process.argv[2] || 'http://127.0.0.1:8793/'
const port = 9333

const proc = spawn(chrome, [
  '--headless=new', '--disable-gpu', `--remote-debugging-port=${port}`,
  '--user-data-dir=' + process.env.TEMP + '/mmx-cdp-profile', 'about:blank',
], { stdio: 'ignore' })

process.on('exit', () => proc.kill())

let target = null
for (let i = 0; i < 40; i++) {
  await sleep(300)
  try {
    const list = await (await fetch(`http://127.0.0.1:${port}/json/list`)).json()
    target = list.find((t) => t.type === 'page')
    if (target) break
  } catch (e) { /* 还没起来 */ }
}
if (!target) throw new Error('chrome 没有起来')

const ws = new WebSocket(target.webSocketDebuggerUrl)
await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej })

let id = 0
const pending = new Map()
ws.onmessage = (ev) => {
  const msg = JSON.parse(ev.data)
  if (msg.id && pending.has(msg.id)) {
    pending.get(msg.id)(msg)
    pending.delete(msg.id)
  }
}

function send(method, params = {}) {
  const mid = ++id
  return new Promise((res) => {
    pending.set(mid, res)
    ws.send(JSON.stringify({ id: mid, method, params }))
  })
}

async function evaluate(expression) {
  const r = await send('Runtime.evaluate', { expression, returnByValue: true, awaitPromise: true })
  if (r.result?.exceptionDetails) throw new Error(JSON.stringify(r.result.exceptionDetails))
  return r.result?.result?.value
}

await send('Page.enable')
await send('Page.navigate', { url: base })
await sleep(1500)

const out = {}

out.title = await evaluate('document.title')
out.rows = await evaluate('document.querySelectorAll("tr[data-row]").length')
out.count = await evaluate('document.getElementById("account-count") ? document.getElementById("account-count").textContent : ""')

// 筛账号：写两个字进去，看还剩几行。
if (out.rows) {
  await evaluate(`(() => {
    const box = document.getElementById('account-filter')
    box.value = 'user03'
    box.dispatchEvent(new Event('input'))
    return true
  })()`)
  await sleep(200)
  out.shown = await evaluate('document.querySelectorAll("tr[data-row]:not(.is-hidden)").length')
  out.countAfter = await evaluate('document.getElementById("account-count").textContent')
}

// 飘出来的框：点一下「改」，看有没有弹出来。
await evaluate(`(() => {
  const box = document.getElementById('account-filter')
  if (box) { box.value = ''; box.dispatchEvent(new Event('input')) }
  const b = document.querySelector('[data-group-edit]')
  if (!b) return 'no-button'
  b.click()
  return 'clicked'
})()`)
await sleep(300)
out.cardOpen = await evaluate('!!document.querySelector(".card .card-box")')
out.cardTitle = await evaluate('document.querySelector(".card h3") ? document.querySelector(".card h3").textContent : ""')

// Esc 关上它。
await send('Input.dispatchKeyEvent', { type: 'keyDown', key: 'Escape', code: 'Escape', windowsVirtualKeyCode: 27 })
await send('Input.dispatchKeyEvent', { type: 'keyUp', key: 'Escape', code: 'Escape', windowsVirtualKeyCode: 27 })
await sleep(200)
out.cardClosed = await evaluate('!document.querySelector(".card")')

// 右下角那句话。
if (out.rows) {
  await evaluate(`(() => {
    const sel = document.querySelector('select[data-field="proxy_group"]')
    sel.dispatchEvent(new Event('change', { bubbles: true }))
    return true
  })()`)
  await sleep(600)
  out.toast = await evaluate('document.getElementById("status").textContent')
  out.toastVisible = await evaluate('getComputedStyle(document.getElementById("status")).opacity')
}

console.log(JSON.stringify(out, null, 2))
ws.close()
proc.kill()
process.exit(0)
