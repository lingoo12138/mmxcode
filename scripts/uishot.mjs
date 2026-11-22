// 拍一张页面的照：给人看设计。
//
// 用法：node scripts/uishot.mjs [url] [out.png] [按一下的脚本]
//
// 第三个参数是截图之前要跑的那句脚本：比如点开一个框。
// 第四个参数写 dark 就是深色（preferredColorScheme=0）。
//
// 窗口的大小平时是 1440×900。要看窄屏，MMX_W / MMX_H 两个环境变量说了算：
// MMX_W=420 MMX_H=900 node scripts/uishot.mjs ...
import { spawn } from 'node:child_process'
import { setTimeout as sleep } from 'node:timers/promises'

const chrome = 'C:/Program Files/Google/Chrome/Application/chrome.exe'
const url = process.argv[2] || 'http://127.0.0.1:8793/egress'
const out = process.argv[3] || 'C:/Users/lishihua/AppData/Local/Temp/mmx-shot.png'
const before = process.argv[4] || ''
const dark = process.argv[5] === 'dark'
const port = 9334

const flags = [
  '--headless=new', '--disable-gpu', '--hide-scrollbars', `--remote-debugging-port=${port}`,
  '--user-data-dir=' + process.env.TEMP + '/mmx-cdp-profile2', 'about:blank',
]
if (dark) flags.splice(2, 0, '--blink-settings=preferredColorScheme=0')
const proc = spawn(chrome, flags, { stdio: 'ignore' })
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
  if (msg.id && pending.has(msg.id)) { pending.get(msg.id)(msg); pending.delete(msg.id) }
}
function send(method, params = {}) {
  const mid = ++id
  return new Promise((res) => { pending.set(mid, res); ws.send(JSON.stringify({ id: mid, method, params })) })
}
async function evaluate(expression) {
  const r = await send('Runtime.evaluate', { expression, returnByValue: true, awaitPromise: true })
  return r.result?.result?.value
}

await send('Page.enable')
await send('Emulation.setDeviceMetricsOverride', {
  width: Number(process.env.MMX_W || 1440),
  height: Number(process.env.MMX_H || 900),
  deviceScaleFactor: 1,
  mobile: false,
})
await send('Page.navigate', { url })
await sleep(1600)
if (before) {
  await evaluate(`(() => { ${before} ; return true })()`)
  await sleep(600)
}
const shot = await send('Page.captureScreenshot', { format: 'png' })
const fs = await import('node:fs')
fs.writeFileSync(out, Buffer.from(shot.result.data, 'base64'))
console.log('wrote', out)
ws.close()
proc.kill()
process.exit(0)
