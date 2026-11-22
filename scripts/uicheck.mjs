// 用 CDP 点一下页面：看看筛账号、排序、勾选、抽屉、飘出来的框和右下角
// 那句话是不是真的能用。
//
// 用法：node scripts/uicheck.mjs [http://127.0.0.1:8793]
import { spawn } from 'node:child_process'
import { setTimeout as sleep } from 'node:timers/promises'

const chrome = 'C:/Program Files/Google/Chrome/Application/chrome.exe'
const base = process.argv[2] || 'http://127.0.0.1:8793'
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

async function goto(url) {
  await send('Page.navigate', { url })
  await sleep(1500)
}

async function key(k, code, vk) {
  await send('Input.dispatchKeyEvent', { type: 'keyDown', key: k, code: code, windowsVirtualKeyCode: vk })
  await send('Input.dispatchKeyEvent', { type: 'keyUp', key: k, code: code, windowsVirtualKeyCode: vk })
  await sleep(250)
}

await send('Page.enable')

const out = {}

// ------------------------------------------------------------ 概览

await goto(base + '/')
out.overviewTitle = await evaluate('document.querySelector(".crumb h1") ? document.querySelector(".crumb h1").textContent : ""')
out.overviewKpis = await evaluate('document.querySelectorAll(".kpi").length')
out.overviewBars = await evaluate('document.querySelectorAll(".bar span").length')

// ------------------------------------------------------------ 账号

await goto(base + '/accounts')
out.rows = await evaluate('document.querySelectorAll("tr[data-row]").length')
out.count = await evaluate('document.getElementById("account-count") ? document.getElementById("account-count").textContent : ""')
out.chips = await evaluate('document.querySelectorAll("#status-chips .chip").length')

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
  await evaluate(`(() => {
    const box = document.getElementById('account-filter')
    box.value = ''
    box.dispatchEvent(new Event('input'))
    return true
  })()`)
  await sleep(200)
}

// 状态那几颗药丸：点「已登出」，只剩签出的那些。
await evaluate(`(() => {
  const chip = document.querySelector('#status-chips .chip[data-status="signed_out"]')
  if (chip) chip.click()
  return true
})()`)
await sleep(200)
out.afterChip = await evaluate('document.querySelectorAll("tr[data-row]:not(.is-hidden)").length')
out.chipPicked = await evaluate('document.querySelectorAll("#status-chips .chip.is-on").length')
await evaluate(`(() => {
  const chip = document.querySelector('#status-chips .chip[data-status=""]')
  if (chip) chip.click()
  return true
})()`)
await sleep(200)
out.afterAllChip = await evaluate('document.querySelectorAll("tr[data-row]:not(.is-hidden)").length')

// 排序：点「积分」那一栏，行该换个顺序。
if (out.rows) {
  await evaluate(`(() => {
    const th = document.querySelector('th.sortable.num')
    if (th) th.click()
    return true
  })()`)
  await sleep(200)
  out.sorted = await evaluate('document.querySelector("th.sortable.num").dataset.sort || ""')
  out.firstRemaining = await evaluate(`(() => {
    const row = document.querySelector('tr[data-row]')
    const cell = row ? row.cells[4] : null
    return cell ? (cell.dataset.sort || '') : ''
  })()`)
}

// 勾选：勾一个，底下浮出那条。
await evaluate(`(() => {
  const b = document.querySelector('.pick-row')
  if (b) { b.checked = true; b.dispatchEvent(new Event('change', { bubbles: true })) }
  return true
})()`)
await sleep(200)
out.bulkShown = await evaluate('!document.getElementById("bulkbar").hidden')
out.bulkCount = await evaluate('document.getElementById("bulk-count").textContent')

// 点一行：右边滑出那一格。
await evaluate(`(() => {
  const row = document.querySelector('tr[data-row]')
  if (row) row.click()
  return true
})()`)
await sleep(300)
out.drawerOpen = await evaluate('!!document.querySelector(".drawer")')
out.drawerFields = await evaluate('document.querySelectorAll(".drawer .kv dt").length')

// Esc 关掉它。
await key('Escape', 'Escape', 27)
out.drawerClosed = await evaluate('!document.querySelector(".drawer")')

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

// 主题：按一下换深浅。
out.themeBefore = await evaluate('document.documentElement.getAttribute("data-theme") || ""')
await evaluate(`(() => {
  const b = document.getElementById('theme-toggle')
  if (b) b.click()
  return true
})()`)
await sleep(200)
out.themeAfter = await evaluate('document.documentElement.getAttribute("data-theme") || ""')
await evaluate(`(() => {
  const b = document.getElementById('theme-toggle')
  if (b) b.click()
  return true
})()`)
await sleep(200)
out.themeAfter2 = await evaluate('document.documentElement.getAttribute("data-theme") || ""')

// ------------------------------------------------------------ 出口

await goto(base + '/egress')
await evaluate(`(() => {
  const b = document.querySelector('[data-group-edit]')
  if (b) b.click()
  return true
})()`)
await sleep(300)
out.cardOpen = await evaluate('!!document.querySelector(".card .card-box")')
out.cardTitle = await evaluate('document.querySelector(".card h3") ? document.querySelector(".card h3").textContent : ""')

// Esc 关上它。
await key('Escape', 'Escape', 27)
out.cardClosed = await evaluate('!document.querySelector(".card")')

console.log(JSON.stringify(out, null, 2))
ws.close()
proc.kill()
process.exit(0)
