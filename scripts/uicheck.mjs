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
// 窗口得是宽的：窄屏那一套（侧栏变成横条）不是这里要验的东西。
await send('Emulation.setDeviceMetricsOverride', {
  width: 1440, height: 900, deviceScaleFactor: 1, mobile: false,
})

const out = {}

// ------------------------------------------------------------ 概览

await goto(base + '/')
out.overviewTitle = await evaluate('document.querySelector(".crumb h1") ? document.querySelector(".crumb h1").textContent : ""')
out.overviewStats = await evaluate('document.querySelectorAll(".stat").length')
out.overviewBars = await evaluate('document.querySelectorAll(".bar span").length')

// 面板能折起来：点一下头，收起来；再点一下，摊开。
out.panels = await evaluate('document.querySelectorAll(".panel[data-panel]").length')
await evaluate(`(() => {
  const b = document.querySelector('.panel[data-panel] .panel-toggle')
  if (b) b.click()
  return true
})()`)
await sleep(200)
out.panelCollapsed = await evaluate('document.querySelectorAll(".panel.is-collapsed").length')
out.panelBodyHidden = await evaluate('!!document.querySelector(".panel.is-collapsed .panel-body[hidden]")')
await evaluate(`(() => {
  const b = document.querySelector('.panel[data-panel] .panel-toggle')
  if (b) b.click()
  return true
})()`)
await sleep(200)
out.panelBack = await evaluate('document.querySelectorAll(".panel.is-collapsed").length')

// 侧栏能收成一条图标道。
await evaluate(`(() => {
  const b = document.getElementById('side-toggle')
  if (b) b.click()
  return true
})()`)
await sleep(200)
out.sideMin = await evaluate('document.body.classList.contains("side-min")')
out.sideLabelsHidden = await evaluate(
  'getComputedStyle(document.querySelector(".side-nav a span")).display === "none"')
await evaluate(`(() => {
  const b = document.getElementById('side-toggle')
  if (b) b.click()
  return true
})()`)
await sleep(200)
out.sideFull = await evaluate('!document.body.classList.contains("side-min")')

// ------------------------------------------------------------ 账号

await goto(base + '/accounts')
out.rows = await evaluate('document.querySelectorAll("tr[data-row]").length')
out.count = await evaluate('document.getElementById("account-count") ? document.getElementById("account-count").textContent : ""')
out.chips = await evaluate('document.querySelectorAll("#status-chips .chip").length')

// 筛账号：写两个字进去，看还剩几行。
if (out.rows) {
  await evaluate(`(() => {
    const box = document.getElementById('account-filter')
    box.value = '备用'
    box.dispatchEvent(new Event('input'))
    return true
  })()`)
  await sleep(200)
  out.shown = await evaluate('document.querySelectorAll("tr[data-row]:not(.is-hidden)").length')
  out.countAfter = await evaluate('document.getElementById("account-count").textContent')
  // 筛的条件得在地址栏里：改完东西重新取一次页面，它还在。
  out.filterInURL = await evaluate('location.search.indexOf("q=") >= 0')
  await evaluate(`(() => {
    const box = document.getElementById('account-filter')
    box.value = ''
    box.dispatchEvent(new Event('input'))
    return true
  })()`)
  await sleep(200)
}

// 命令面板：Ctrl+K 弹出来，里面有几条能跳的。
await evaluate(`(() => {
  document.dispatchEvent(new KeyboardEvent('keydown', { key: 'k', ctrlKey: true, bubbles: true }))
  return true
})()`)
await sleep(300)
out.paletteOpen = await evaluate('!!document.querySelector(".palette")')
out.paletteItems = await evaluate('document.querySelectorAll(".palette-item").length')
await key('Escape', 'Escape', 27)
await sleep(200)
out.paletteClosed = await evaluate('!document.querySelector(".palette")')

// 状态那几颗药丸：点「已登出」，只剩签出的那些。
await evaluate(`(() => {
  const chip = document.querySelector('#status-chips .chip[data-status="signed_out"]')
  if (chip) chip.click()
  return true
})()`)
await sleep(200)
out.afterChip = await evaluate('document.querySelectorAll("tr[data-row]:not(.is-hidden)").length')
out.chipPicked = await evaluate('document.querySelectorAll("#status-chips .chip.is-on").length')
// 药丸的条件得在地址栏里：改完东西重新取一次页面，它还在。
out.chipInURL = await evaluate('location.search.indexOf("s=signed_out") >= 0')
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
    // 第 3 格是积分（0 勾选、1 账号、2 状态、3 积分）。
    const cell = row ? row.cells[3] : null
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

// 出口那一格：改一下，下拉框得把话说出来。
if (out.rows) {
  await evaluate(`(() => {
    const row = document.querySelector('tr[data-row]')
    if (row) row.click()
    return true
  })()`)
  await sleep(300)
  await evaluate(`(() => {
    const sel = document.querySelector('select[data-field="proxy_group"]')
    if (sel) sel.dispatchEvent(new Event('change', { bubbles: true }))
    return true
  })()`)
  await sleep(600)
  out.toast = await evaluate('document.getElementById("status").textContent')
  out.toastVisible = await evaluate('getComputedStyle(document.getElementById("status")).opacity')
  await key('Escape', 'Escape', 27)
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

// ------------------------------------------------------------ 用量

await goto(base + '/usage')
out.chartDays = await evaluate('document.querySelectorAll(".chart-day").length')
out.chartTicks = await evaluate('document.querySelectorAll(".chart-tick").length')
out.chartXLabels = await evaluate('document.querySelectorAll(".chart-xlabel").length')
out.chartValues = await evaluate('document.querySelectorAll(".chart-value").length')
out.chartDots = await evaluate('document.querySelectorAll(".chart-dot").length')
out.usedToday = await evaluate('(() => { const s = Array.prototype.slice.call(document.querySelectorAll(".stat")).find(x => /今天消耗/.test(x.textContent)); return s ? s.textContent.replace(/\\s+/g, " ").trim() : "" })()')

// 指针落在图上：十字线出来了，浮着的那个数也出来了。
await evaluate(`(() => {
  const rect = document.querySelectorAll('.chart-day')[8]
  const b = rect.getBoundingClientRect()
  rect.dispatchEvent(new PointerEvent('pointermove', { bubbles: true, clientX: b.left + b.width / 2, clientY: b.top + b.height / 2 }))
  return true
})()`)
await sleep(200)
out.tipShown = await evaluate('!document.getElementById("chart-tip").hidden')
out.tipText = await evaluate('document.getElementById("chart-tip").textContent')
out.crossOn = await evaluate('document.querySelector(".chart-cross").classList.contains("is-on")')
out.hoverDot = await evaluate('!document.getElementById("chart-hover-dot").hidden')

// 键盘上也是同一层东西：左右键走日子。
await evaluate(`(() => { document.getElementById('usage-chart').focus(); return true })()`)
await sleep(200)
await key('ArrowLeft', 'ArrowLeft', 37)
await sleep(200)
out.tipAfterKey = await evaluate('document.getElementById("chart-tip").textContent')

// 点一天：问那一天是哪些号在用，问回来就地摆上。
await evaluate(`(() => {
  const rect = document.querySelectorAll('.chart-day')[3]
  rect.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }))
  return true
})()`)
await sleep(600)
out.dayPanel = await evaluate('!document.getElementById("usage-day").hidden')
out.dayRows = await evaluate('document.querySelectorAll("#usage-day-body li").length')
out.dayTitle = await evaluate('document.getElementById("usage-day-title").textContent')
out.dayInURL = await evaluate('location.search.indexOf("day=") >= 0')

// ------------------------------------------------------------ 重登

await goto(base + '/relogin')
out.reloginRows = await evaluate('document.querySelectorAll("tr[data-relogin-row]").length')
out.reloginWhys = await evaluate('document.querySelectorAll("tr[data-relogin-why]").length')
out.reloginWhyText = await evaluate('document.querySelector(".err-band") ? document.querySelector(".err-band").textContent.trim() : ""')
out.reloginPills = await evaluate('Array.prototype.map.call(document.querySelectorAll("td .pill"), x => x.textContent.trim()).join(",")')

// 点一个「重登」：那一行和它下面那句理由都得从页面上走掉。
if (out.reloginRows) {
  await evaluate(`(() => {
    const b = document.querySelector('[data-relogin]')
    if (b) b.click()
    return true
  })()`)
  await sleep(800)
  out.reloginStatus = await evaluate('document.getElementById("relogin-status").textContent')
  out.reloginRowsAfter = await evaluate('document.querySelectorAll("tr[data-relogin-row]").length')
  out.reloginWhysAfter = await evaluate('document.querySelectorAll("tr[data-relogin-why]").length')
}

console.log(JSON.stringify(out, null, 2))
ws.close()
proc.kill()
process.exit(0)
