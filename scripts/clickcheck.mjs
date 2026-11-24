// 看看页面上那个按钮按下去到底发生了什么。
//
// 用法：node scripts/clickcheck.mjs [url] [要点的选择器]
//
// url 给的是要打开的整页（默认是账号页）。它把按钮按一下，然后说：按
// 之前它长什么样、按完页面上哪儿变了、发了哪些请求。
import { spawn } from 'node:child_process'
import { setTimeout as sleep } from 'node:timers/promises'

const chrome = 'C:/Program Files/Google/Chrome/Application/chrome.exe'
const base = process.argv[2] || 'http://127.0.0.1:8793/accounts'
const sel = process.argv[3] || 'button[data-checkin]'
const port = 9335

const proc = spawn(chrome, [
  '--headless=new', '--disable-gpu', `--remote-debugging-port=${port}`,
  '--user-data-dir=' + process.env.TEMP + '/mmx-cdp-profile3', 'about:blank',
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
const logs = []
ws.onmessage = (ev) => {
  const msg = JSON.parse(ev.data)
  if (msg.id && pending.has(msg.id)) { pending.get(msg.id)(msg); pending.delete(msg.id) }
  if (msg.method === 'Runtime.consoleAPICalled') {
    logs.push('console: ' + (msg.params.args || []).map(a => a.value ?? a.description ?? '').join(' '))
  }
  if (msg.method === 'Runtime.exceptionThrown') {
    logs.push('exception: ' + JSON.stringify(msg.params.exceptionDetails?.exception?.description || msg.params.exceptionDetails?.text))
  }
  if (msg.method === 'Network.responseReceived') {
    logs.push('http ' + msg.params.response.status + ' ' + msg.params.response.url)
  }
}
function send(method, params = {}) {
  const mid = ++id
  return new Promise((res) => { pending.set(mid, res); ws.send(JSON.stringify({ id: mid, method, params })) })
}
async function evaluate(expression) {
  const r = await send('Runtime.evaluate', { expression, returnByValue: true, awaitPromise: true })
  if (r.result?.exceptionDetails) throw new Error(JSON.stringify(r.result.exceptionDetails))
  return r.result?.result?.value
}

await send('Page.enable')
await send('Runtime.enable')
await send('Network.enable')
await send('Emulation.setDeviceMetricsOverride', { width: 1440, height: 900, deviceScaleFactor: 1, mobile: false })
await send('Page.navigate', { url: base })
await sleep(1800)

console.log('buttons:', await evaluate(`document.querySelectorAll('${sel}').length`))
console.log('first visible:', await evaluate(`(() => {
  const b = document.querySelector('${sel}')
  if (!b) return 'none'
  const s = getComputedStyle(b)
  return JSON.stringify({ opacity: s.opacity, pointerEvents: s.pointerEvents, disabled: b.disabled, rect: b.getBoundingClientRect().width })
})()`))

await evaluate(`(() => {
  const b = document.querySelector('${sel}')
  if (!b) return false
  b.scrollIntoView()
  b.click()
  return true
})()`)
await sleep(2500)
console.log('status text:', await evaluate(`document.getElementById('status') ? document.getElementById('status').textContent : '(no status box)'`))
console.log('checked cell:', await evaluate(`(() => {
  const c = document.querySelector('[data-checked-cell]')
  return c ? c.textContent.trim() : '(no cell)'
})()`))
console.log('button state:', await evaluate(`(() => {
  const b = document.querySelector('${sel}')
  if (!b) return 'gone'
  const s = getComputedStyle(b)
  return JSON.stringify({ opacity: s.opacity, pointerEvents: s.pointerEvents, disabled: b.disabled, title: b.title })
})()`))
console.log('logs:\n' + logs.join('\n'))
ws.close()
proc.kill()
process.exit(0)
