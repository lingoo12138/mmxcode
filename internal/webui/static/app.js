// mmxcode 的 WebUI 脚本。
//
// 做几件事：改账号的出口、触发签到、测出口、登录、挑账号。
// 页面本身是服务端渲染的，这里不重新渲染。
//
// 这里不用带 API key：浏览器同源的 fetch 会自己带上 mmxcode_session
// 那张票，页面的按钮因此不会 401。
//
// 上游答上来的东西（验证码、地址、错误）一律当字看：它们进页面的时候
// 只走 textContent 和 URL，不拼 HTML。

// readAnswer 读一个回答。
//
// 回答不一定是 JSON：网关拒绝了一把 key 的时候给的是登录页，那是
// HTML。所以回答先当字读下来，再看它是什么。
async function readAnswer(res) {
  const text = await res.text();
  const type = (res.headers.get('Content-Type') || '').toLowerCase();
  let json = null;
  // 没有 Content-Type 的回答也试一下：中间的一层代理可能把它拿掉了。
  if (type.indexOf('json') >= 0 || type === '') {
    try {
      json = JSON.parse(text);
    } catch (e) {
      json = null;
    }
  }
  return { ok: res.ok, status: res.status, json: json, text: text };
}

// messageOf 说一个回答说了什么。
//
// JSON 的错误在 error.message 里，登录页的那句话在 .bad 里。两样都读，
// 人看到的都是一句话，不是一段 HTML。
function messageOf(answer) {
  if (answer.json) {
    const e = answer.json.error;
    if (typeof e === 'string' && e) return e;
    if (e && e.message) return e.message;
    if (answer.json.message) return answer.json.message;
  }
  const page = pageMessage(answer.text);
  if (page) return page;
  return answer.text.trim() || ('HTTP ' + answer.status);
}

// pageMessage 读登录页上那句话。
//
// 页面是 HTML，所以它被解析成文档，只取 .bad 那一行的字。它从来不
// 被写回页面里去。
function pageMessage(html) {
  if (!html) return '';
  try {
    const doc = new DOMParser().parseFromString(html, 'text/html');
    const bad = doc.querySelector('.bad');
    return bad ? bad.textContent.trim() : '';
  } catch (e) {
    return '';
  }
}

// safeURL 给一个能点的地址，或者不给。
//
// 地址是上游答上来的，javascript: 那种点一下就跑。只有 http 和 https
// 才是链接，其余的不是链接。
function safeURL(raw) {
  try {
    const u = new URL(String(raw));
    if (u.protocol !== 'http:' && u.protocol !== 'https:') return null;
    return u.href;
  } catch (e) {
    return null;
  }
}

// busy says how many requests are in the air.
//
// 页面顶上那道细线就是它：只要还有一个请求在路上，它就亮着。手上那件
// 事也得说一声：那一行小字写着「正在签到」之类的话。
let inFlight = 0;

function setBusy(on, text) {
  inFlight += on ? 1 : -1;
  if (inFlight < 0) inFlight = 0;
  document.body.classList.toggle('is-busy', inFlight > 0);
  if (inFlight > 0 && text) {
    document.body.dataset.busy = text;
  } else if (inFlight <= 0) {
    delete document.body.dataset.busy;
  }
}

// call asks the gateway something.
//
// 页面上的按钮都在等这个回答，所以从发出到答回来，页顶那道线亮着。
async function call(url, options, busyText) {
  setBusy(true, busyText);
  try {
    const res = await fetch(url, options);
    return await readAnswer(res);
  } finally {
    setBusy(false);
  }
}

async function post(url, body, busyText) {
  const answer = await call(url, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  }, busyText);
  if (answer.status === 401) askForKey();
  if (!answer.ok) throw new Error(messageOf(answer));
  return answer.json;
}

async function patch(url, body, busyText) {
  const answer = await call(url, {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  }, busyText);
  if (answer.status === 401) askForKey();
  if (!answer.ok) throw new Error(messageOf(answer));
}

// del 删掉一个东西。
async function del(url, busyText) {
  const answer = await call(url, { method: 'DELETE' }, busyText);
  if (answer.status === 401) askForKey();
  if (!answer.ok) throw new Error(messageOf(answer));
}

// signIn 拿一张票。
//
// 回答可能是登录页（HTML），也可能是 JSON：网关现在给的是前者。两样
// 都读，人看到的是同一句话。
async function signIn(key) {
  const answer = await call('/api/session', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ key: key }),
  });
  if (!answer.ok) throw new Error(messageOf(answer));
  return answer;
}

// askForKey 要一把 key。
//
// 会话没了的时候，页面上的按钮都答 401。这里问一次 key，交给网关换
// 一张票，换到了就把页面重新加载。
let keyBox = null;
function askForKey() {
  if (keyBox) return;
  const card = makeCard('这个网关要一把 API key', '填进去就能继续。');
  keyBox = card.box;
  const field = document.createElement('div');
  field.className = 'field';
  const label = document.createElement('label');
  label.textContent = '后台登录密钥';
  label.htmlFor = 'card-key';
  const input = document.createElement('input');
  input.id = 'card-key';
  input.type = 'password';
  input.placeholder = 'API key';
  input.autocomplete = 'current-password';
  field.append(label, input);
  const button = document.createElement('button');
  button.type = 'submit';
  button.className = 'primary';
  button.textContent = '进入';
  card.form.append(field, button);
  card.form.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    button.disabled = true;
    try {
      await signIn(input.value);
      location.reload();
    } catch (e) {
      card.msg.textContent = e.message;
      button.disabled = false;
    }
  });
  card.open(input);
}

// makeCard 造一个飘出来的框。
//
// 它是个对话框：跟读屏器说得清自己是什么（role、aria-modal、标题），
// 焦点关在里面按 Tab 跑不出去，关上的时候焦点回到摆它出来的那一下。
// 三个飘出来的框（要 key、改组、批量换组）都长一个样，所以放在一起造。
let cardSeq = 0;
function makeCard(titleText, leadText) {
  const opener = document.activeElement;
  const box = document.createElement('div');
  box.className = 'card';
  const panel = document.createElement('div');
  panel.className = 'card-box';
  panel.setAttribute('role', 'dialog');
  panel.setAttribute('aria-modal', 'true');
  panel.setAttribute('tabindex', '-1');
  // 头一行：名字在左，关掉的那一下在右。
  //
  // 没有关掉的那一下，人就只能按 Esc 或者点外面：知道 Esc 的人不多。
  const head = document.createElement('div');
  head.className = 'card-head';
  const title = document.createElement('h3');
  cardSeq++;
  title.id = 'card-title-' + cardSeq;
  title.textContent = titleText;
  panel.setAttribute('aria-labelledby', title.id);
  const close = document.createElement('button');
  close.type = 'button';
  close.className = 'card-close';
  close.setAttribute('aria-label', '关上');
  close.appendChild(icon('x'));
  close.addEventListener('click', () => closeCard(box));
  head.append(title, close);
  const lead = document.createElement('p');
  lead.textContent = leadText;
  const form = document.createElement('form');
  const msg = document.createElement('p');
  msg.className = 'card-msg';
  msg.setAttribute('role', 'status');
  panel.append(head, lead, form, msg);
  box.appendChild(panel);

  box.addEventListener('click', (ev) => {
    if (ev.target === box) closeCard(box);
  });
  document.body.appendChild(box);
  // 关上的时候焦点得还给摆它出来的那一下。
  box._opener = opener;
  refreshInert();

  return {
    box: box,
    panel: panel,
    form: form,
    msg: msg,
    // open 把焦点摆进去：先落在框里的第一个能聚焦的东西上。
    open: function (first) {
      const target = first || panel.querySelector('input, select, textarea, button');
      if (target && target.focus) target.focus();
    },
  };
}

// refreshInert 把底下的页面按停。
//
// 框和抽屉是模态的：它们开着的时候，底下的表格不该还能按 —— Tab 跑出去
// 按到看不见的按钮上，人会以为自己在框里。
//
// 按停的是**底下那一层**，不是最上面那一层：
//
// - 页面（.shell）：框或者抽屉开着的时候按停。
// - 抽屉（.drawer）：只有它上面还飘着一个框的时候才按停。抽屉自己开着
//   的时候按停它，它里面的按钮就全按不动了：点一下掉到罩子上，抽屉自
//   己关上 —— 看着像按钮坏了。
// - 框（.card）：它是最上面那一层，不按停。
function refreshInert() {
  const card = document.querySelector('.card');
  const drawer = document.querySelector('.drawer');
  const shell = document.querySelector('.shell');
  if (shell) {
    if (card || drawer) shell.setAttribute('inert', '');
    else shell.removeAttribute('inert');
  }
  if (drawer) {
    if (card) drawer.setAttribute('inert', '');
    else drawer.removeAttribute('inert');
  }
}

// closeCard 关掉一个飘出来的框。
//
// 焦点回到摆它出来的那一下：不然人在一百行表格里就找不着刚才按的是哪
// 一个了。
function closeCard(box) {
  if (!box) return;
  const opener = box._opener;
  box.remove();
  if (box === keyBox) keyBox = null;
  refreshInert();
  if (opener && opener.focus && document.contains(opener)) opener.focus();
}

// Esc 关掉最上面那个框。
//
// 飘出来的框压在抽屉上面：先关框，再关抽屉。
document.addEventListener('keydown', (ev) => {
  if (ev.key !== 'Escape') return;
  // 命令面板也用它那层罩子：它比别的框都高，先关它。
  if (paletteBox) {
    closePalette();
    return;
  }
  const boxes = document.querySelectorAll('.card');
  if (boxes.length) {
    closeCard(boxes[boxes.length - 1]);
    return;
  }
  if (drawerBox) closeDrawer();
});
// say 在右下角说一句。
//
// 说过的话过一会儿自己没了：那是给人看的反馈，不是页面上的一行字。
let sayTimer = null;

function say(text) {
  const box = document.getElementById('status');
  if (!box) return;
  box.textContent = text;
  if (sayTimer) clearTimeout(sayTimer);
  sayTimer = setTimeout(() => {
    box.textContent = '';
  }, 4000);
}

// sayAndReload 说一句，然后重新取一页。
//
// 页面是服务端渲染的：改完了得重新取一次才是真的样子。可是重新取页面
// 会把刚才那句话一起冲掉 —— 那句话得先放进 sessionStorage，刷新完了
// 再摆出来。
function sayAndReload(text) {
  say(text);
  try {
    sessionStorage.setItem('mmxcode-flash', text);
  } catch (e) {
    /* 存不下也就算了 */
  }
  location.reload();
}

// 刷新之后把刚才那句话摆出来。
(function () {
  let text = '';
  try {
    text = sessionStorage.getItem('mmxcode-flash') || '';
    sessionStorage.removeItem('mmxcode-flash');
  } catch (e) {
    text = '';
  }
  if (!text) return;
  say(text);
})();

// icon 造一个图标。
//
// 图标是 Tabler 的官方路径，摆在 /static/icons.svg 里。颜色跟着
// currentColor 走，所以它永远跟周围的字一样重。
function icon(name, cls) {
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  svg.setAttribute('class', cls || 'ic ic-sm');
  svg.setAttribute('aria-hidden', 'true');
  const use = document.createElementNS('http://www.w3.org/2000/svg', 'use');
  use.setAttribute('href', '/static/icons.svg#' + name);
  svg.appendChild(use);
  return svg;
}

// statusNames 是机器认的字和人读的字中间那一层。
//
// 表格和抽屉得说同一句话：一个写「出错」，另一个写 error，人会以为是
// 两回事。
const statusNames = {
  active: '正常',
  signed_out: '已登出',
  error: '出错',
};

// outcomeWords 是签到那几种结果人读的话。
//
// 网关给的是 claimed、done、ineligible 这些机器字，直接摆到页面上，人
// 得自己猜。
function outcomeWords(out) {
  switch (out && out.outcome) {
    case 'claimed':
      return '领到 ' + (out.credit || 0);
    case 'done':
      return '今天已经签过了';
    case 'ineligible':
      return '这个号不能签';
    case 'inactive':
      return '这个号已经不能用了';
    case 'failed':
      return (out.message || '签到没成');
    default:
      return String((out && out.outcome) || '');
  }
}

// checkedWord says, in words, what a check-in did.
//
// 每一种结果都有话：签过的说签了几天，不能签的说不能签，不能用了的说
// 不能用了。签到失败也是一种结果 —— 它不是「没有结果」。
function checkedWord(out) {
  const o = out && out.outcome;
  if (o === 'claimed' || o === 'done') {
    return { word: '已签 ' + (out.streak || 1) + ' 天', cls: 'good' };
  }
  if (o === 'ineligible') return { word: '不能签', cls: '' };
  if (o === 'inactive') return { word: '不能用了', cls: 'bad' };
  if (o === 'failed') return { word: '签到失败', cls: 'bad' };
  return null;
}

// setChecked writes the result of a check-in into the cell that says
// whether the account checked in today.
//
// 每一种结果都写：签过的写「已签 N 天」，不能签的写「不能签」，不能
// 用了的写「不能用了」。只写签成了的那一种，别的结果就停在「未签」——
// 人看着像没签成，那句话也不对。
function setChecked(cell, out) {
  const w = checkedWord(out);
  if (!w || !cell) return;
  cell.textContent = '';
  const pill = document.createElement('span');
  pill.className = w.cls ? 'pill ' + w.cls : 'pill';
  pill.textContent = w.word;
  cell.appendChild(pill);
}

// windowWords says what the plan's windows look like, in one line.
//
// 跟服务器算的一样：「MiniMax-M2 61% · MiniMax-M2 12%」。
function windowWords(raw) {
  if (!raw) return '';
  let list = null;
  try {
    list = JSON.parse(raw);
  } catch (e) {
    return '';
  }
  if (!Array.isArray(list) || !list.length) return '';
  const parts = [];
  for (const x of list) {
    const name = String(x.ModelName || '');
    if (x.Unlimited) {
      parts.push(name + ' 不限');
      continue;
    }
    parts.push(name + ' ' + Math.round(Number(x.Used) || 0) + '%');
  }
  return parts.join(' · ');
}

// setCredits writes a freshly read balance into the table's credits cell.
//
// 桌子上的数是上一次读到的。现读了一遍，抽屉里改了，桌子里也得跟着改 —
// 不然两个数摆在一起，人得猜哪个是真的。
function setCredits(cell, snap) {
  if (!cell) return;
  cell.textContent = '';
  const granted = Number(snap.Granted) || 0;
  const remaining = Number(snap.Remaining) || 0;
  if (snap.HasAmounts !== true) {
    const warn = document.createElement('span');
    warn.className = 'warn';
    warn.textContent = '读不到';
    cell.appendChild(warn);
    return;
  }
  const pct = granted > 0 ? (remaining / granted * 100) : 0;
  const line = document.createElement('span');
  line.className = 'nowrap';
  line.textContent = remaining.toFixed(2) + ' / ' + granted.toFixed(0);
  const meterLine = document.createElement('span');
  meterLine.className = 'meter-line';
  const meter = document.createElement('span');
  meter.className = 'meter';
  const bar = document.createElement('span');
  bar.className = pct <= 0 ? 'bad' : (pct < 20 ? 'warn' : '');
  bar.style.width = pct.toFixed(1) + '%';
  meter.appendChild(bar);
  meterLine.appendChild(meter);
  const pctText = document.createElement('span');
  pctText.className = 'pct';
  pctText.textContent = pct.toFixed(0) + '%';
  meterLine.appendChild(pctText);
  cell.append(line, meterLine);
}

// ---------------------------------------------------------------- 主题

// 主题：跟着系统、浅色、深色，三样轮着来。
//
// 选了就记下来：下次打开还是那个样子。记不住也不碍事，跟着系统走。
(function () {
  const button = document.getElementById('theme-toggle');
  if (!button) return;
  const glyph = document.getElementById('theme-icon');
  const order = ['', 'light', 'dark'];
  const names = { '': '跟着系统', light: '浅色', dark: '深色' };
  const marks = { '': 'sun-moon', light: 'sun', dark: 'moon' };
  let current = '';
  try {
    current = localStorage.getItem('mmxcode-theme') || '';
  } catch (e) {
    current = '';
  }
  if (order.indexOf(current) < 0) current = '';
  const paint = () => {
    if (glyph) {
      const use = glyph.querySelector('use');
      if (use) use.setAttribute('href', '/static/icons.svg#' + marks[current]);
    }
    // 现在是哪个模式，得说得出来：图标变了只是一张图，读屏器读不出来。
    const text = '主题：' + names[current];
    button.title = text + '（按一下换一个）';
    button.setAttribute('aria-label', text);
    if (current) document.documentElement.setAttribute('data-theme', current);
    else document.documentElement.removeAttribute('data-theme');
  };
  paint();
  button.addEventListener('click', () => {
    current = order[(order.indexOf(current) + 1) % order.length];
    try {
      localStorage.setItem('mmxcode-theme', current);
    } catch (e) {
      /* 存不下也就算了 */
    }
    paint();
    // 换完了说一声：不然按下去页面上什么也没变，人不知道按没按上。
    say('主题换成' + names[current]);
  });
})();

// ---------------------------------------------------------------- 筛账号

// 筛账号：输入的字跟账号对得上就留着，对不上就藏起来。
//
// 状态那几颗药丸是另一回事：它筛的是状态，不是字。两样一起管。
(function () {
  const box = document.getElementById('account-filter');
  const chips = document.getElementById('status-chips');
  if (!box && !chips) return;
  const rows = Array.from(document.querySelectorAll('tr[data-row]'));
  const counter = document.getElementById('account-count');
  const empty = document.getElementById('no-rows');

  // 筛的条件放在地址栏里。
  //
  // 页面上改完东西要重新取一次页面：筛要是只活在页面上，那一下就丢了。
  // 筛出三个坏了的号，修掉一个，页面却跳回一百行，人得从头再筛一遍。
  // 放在地址栏里，改完还在，链接也能发给同事。
  const params = new URLSearchParams(location.search);
  if (box && params.get('q')) box.value = params.get('q');
  let want = params.get('s') || '';

  const keepURL = () => {
    const url = new URL(location.href);
    const q = box ? box.value.trim() : '';
    if (q) url.searchParams.set('q', q);
    else url.searchParams.delete('q');
    if (want) url.searchParams.set('s', want);
    else url.searchParams.delete('s');
    history.replaceState(null, '', url);
  };

  const apply = () => {
    const q = box ? box.value.trim().toLowerCase() : '';
    let shown = 0;
    for (const row of rows) {
      // ID 也算：它在抽屉里，表格上不一定看得见，可是人要能拿它筛。
      const hay = (row.textContent + ' ' + row.dataset.row).toLowerCase();
      const hitText = !q || hay.indexOf(q) >= 0;
      const status = row.dataset.status;
      const hitStatus = !want ||
        (want === 'error' ? status !== 'active' && status !== 'signed_out' : status === want);
      const hit = hitText && hitStatus;
      row.classList.toggle('is-hidden', !hit);
      // 藏起来的行，勾也要去掉：它看不见，却还占着「选中了几个」。
      if (!hit) {
        for (const b of row.querySelectorAll('.pick-row')) {
          if (b.checked) {
            b.checked = false;
            b.dispatchEvent(new Event('change', { bubbles: true }));
          }
        }
      }
      // 错的那一行是跟着这个号走的：号藏起来了，那一行也得藏起来。
      const band = bandOf(row);
      if (band) band.classList.toggle('is-hidden', !hit);
      if (hit) shown++;
    }
    if (counter) counter.textContent = rows.length ? shown + ' / ' + rows.length : '';
    if (empty) empty.hidden = shown > 0 || !rows.length;
    // 药丸的样子得跟着条件走：从地址栏里回来的页面上也是。
    if (chips) {
      for (const c of chips.querySelectorAll('.chip')) {
        const on = (c.dataset.status || '') === want;
        c.classList.toggle('is-on', on);
        c.setAttribute('aria-pressed', on ? 'true' : 'false');
      }
    }
  };
  if (box) box.addEventListener('input', () => { keepURL(); apply(); });
  if (chips) {
    chips.addEventListener('click', (ev) => {
      const chip = ev.target.closest('.chip');
      if (!chip) return;
      want = chip.dataset.status || '';
      keepURL();
      apply();
    });
  }
  apply();
})();

// ---------------------------------------------------------------- 补签

// 补签：给今天还没签到的号补签。
//
// 服务器 00:00 没开着的那一天、那趟跑到一半崩了、号是后来加进来的 ——
// 这些号今天还没签到，补上。站点只给当天的签到：过去漏掉的那几天补不
// 回来。
//
// 一趟要签的号可能有几百个，跑一分多钟，所以它是后台的活儿：这一页
// 看着它走到哪儿，不等着它。
(function () {
  const button = document.getElementById('checkin-catchup');
  if (!button) return;
  const status = document.getElementById('checkin-status');

  // watch 看着那一趟：它走到哪儿了、完了没有。
  const watch = async () => {
    for (;;) {
      let state = null;
      try {
        const answer = await call('/api/checkin');
        state = answer.json || {};
      } catch (e) {
        // 网关那边问不着了：这一趟的进度就看不见了，别把它当成「完了」。
        if (status) status.textContent = '问不着这一趟到哪儿了：' + e.message;
        return;
      }
      if (!state.running) {
        if (status) status.textContent = state.message || '补签完了';
        button.disabled = false;
        // 表格上的「今日签到」和概览上「能签的都签了」都变了：重新取
        // 一次页面。筛的条件在地址栏里，重取以后还在。
        location.reload();
        return;
      }
      if (status) {
        status.textContent = '补签中 ' + state.done + ' / ' + state.total +
          '（失败 ' + state.failed + '）';
      }
      await new Promise((res) => setTimeout(res, 1000));
    }
  };

  button.addEventListener('click', async () => {
    button.disabled = true;
    if (status) status.textContent = '补签中……';
    try {
      const answer = await call('/api/checkin', { method: 'POST' });
      const state = answer.json || {};
      if (state.running) {
        await watch();
        return;
      }
      if (status) status.textContent = state.message || '补签完了';
      button.disabled = false;
    } catch (e) {
      if (status) status.textContent = '补签没起来：' + e.message;
      button.disabled = false;
    }
  });

  // 页面开着的时候那一趟可能已经在跑了（别人叫的，或者刚才这一趟还没
  // 完）：看着它。
  call('/api/checkin').then((answer) => {
    const state = answer.json || {};
    if (state.running) {
      button.disabled = true;
      watch();
    } else if (state.message && status) {
      status.textContent = state.message;
    }
  }).catch(() => { /* 问不着就算了 */ });
})();

// bandOf finds the error line that belongs to an account row.
//
// 错的那一句自己占一行（占了整张表那么宽），它跟账号那一行是一对：排序
// 的时候得一齐挪，藏起来的时候也得一齐藏。
function bandOf(row) {
  if (!row || !row.dataset) return null;
  const owner = row.dataset.row;
  if (!owner) return null;
  return document.querySelector('tr.err-row[data-owner="' + owner + '"]');
}

// ---------------------------------------------------------------- 排序

// 点表头排一下。
//
// 一百个账号谁在前谁在后，自己说了算：签过到的在前，出错的在前，积分
// 多的在前，都行。
(function () {
  const table = document.querySelector('.table-wrap table');
  if (!table) return;
  const body = table.querySelector('tbody');
  const heads = table.querySelectorAll('thead th.sortable');
  if (!body || !heads.length) return;
  for (const th of heads) {
    const doSort = () => {
      const col = Number(th.dataset.col);
      // 再点一次是反过来。
      const down = th.dataset.sort !== 'desc';
      for (const h of heads) {
        delete h.dataset.sort;
        h.setAttribute('aria-sort', 'none');
      }
      th.dataset.sort = down ? 'asc' : 'desc';
      th.setAttribute('aria-sort', down ? 'ascending' : 'descending');
      const rows = Array.from(body.querySelectorAll('tr[data-row]'));
      rows.sort((a, b) => compare(cellValue(a, col), cellValue(b, col)) * (down ? 1 : -1));
      for (const row of rows) {
        body.appendChild(row);
        // 错的那一行跟着这个号走。
        const band = bandOf(row);
        if (band) body.appendChild(band);
      }
    };
    th.addEventListener('click', doSort);
    // 键盘也得能排：表头是能聚焦的，按回车或者空格就是排一下。
    th.addEventListener('keydown', (ev) => {
      if (ev.key === 'Enter' || ev.key === ' ') {
        ev.preventDefault();
        doSort();
      }
    });
  }
})();

// cellValue gives the value a cell sorts by.
function cellValue(row, col) {
  const cell = row.cells[col];
  if (!cell) return '';
  return cell.dataset.sort !== undefined ? cell.dataset.sort : cell.textContent.trim();
}

// compare puts two values in order.
//
// 两个都是数就按数比：积分那几栏按字比的话，「1000」会排在「200」前
// 面。其余的按字比，中文按中文的顺序。
function compare(a, b) {
  const x = Number(a);
  const y = Number(b);
  if (a !== '' && b !== '' && !isNaN(x) && !isNaN(y)) return x - y;
  return String(a).localeCompare(String(b), 'zh');
}

// ---------------------------------------------------------------- 挑账号

// 勾选：勾上以后底下浮出一条，能一次签到、一次设组、一次删掉。
//
// 一百个账号一个一个点签到是件傻事：它们一起签。
(function () {
  const all = document.getElementById('pick-all');
  const bar = document.getElementById('bulkbar');
  const count = document.getElementById('bulk-count');
  if (!all || !bar) return;
  // 只算看得见的那些：筛掉了的行不该被「全选」选上。
  //
  // 以前这里是全部的勾选框：筛出一个号，点一下全选，批量删除会把十四条
  // 一起删了 —— 人看见的只有一个。
  const boxes = () => Array.from(document.querySelectorAll('.pick-row'))
    .filter((b) => {
      const row = b.closest('tr');
      return row && !row.classList.contains('is-hidden');
    });
  const paint = () => {
    const picked = boxes().filter((b) => b.checked);
    if (count) count.textContent = String(picked.length);
    bar.hidden = picked.length === 0;
    all.checked = picked.length > 0 && picked.length === boxes().length;
    all.indeterminate = picked.length > 0 && picked.length < boxes().length;
    for (const b of document.querySelectorAll('.pick-row')) {
      const row = b.closest('tr');
      if (!row) continue;
      row.classList.toggle('is-picked', b.checked);
      // 错的那一行也是这个号的一部分：选中了它也得跟着亮。
      const band = bandOf(row);
      if (band) band.classList.toggle('is-picked', b.checked);
    }
  };
  const pickedIDs = () => boxes().filter((b) => b.checked).map((b) => b.dataset.id);

  all.addEventListener('change', () => {
    for (const b of boxes()) b.checked = all.checked;
    paint();
  });
  document.addEventListener('change', (ev) => {
    if (ev.target.closest('.pick-row')) paint();
  });

  const cancel = document.getElementById('bulk-cancel');
  if (cancel) {
    cancel.addEventListener('click', () => {
      for (const b of boxes()) b.checked = false;
      paint();
    });
  }

  const checkin = document.getElementById('bulk-checkin');
  if (checkin) {
    checkin.addEventListener('click', async () => {
      const ids = pickedIDs();
      if (!ids.length) return;
      checkin.disabled = true;
      const progress = document.getElementById('bulk-progress');
      let ok = 0;
      let bad = 0;
      for (const id of ids) {
        try {
          await post('/api/accounts/' + encodeURIComponent(id) + '/checkin');
          ok++;
        } catch (e) {
          bad++;
        }
        // 进度写在条上，不写飘字：一百个飘字没人看得见。
        if (progress) progress.textContent = (ok + bad) + ' / ' + ids.length;
      }
      sayAndReload('签了 ' + ok + ' 个，失败 ' + bad + ' 个');
    });
  }

  const group = document.getElementById('bulk-group');
  if (group) {
    group.addEventListener('click', () => {
      const ids = pickedIDs();
      if (!ids.length) return;
      askGroup(ids);
    });
  }

  const remove = document.getElementById('bulk-delete');
  if (remove) {
    remove.addEventListener('click', () => {
      const ids = pickedIDs();
      if (!ids.length) return;
      confirmCard('删掉选中的 ' + ids.length + ' 个账号？',
        '它们的令牌和读数也跟着没了。', '删掉', async () => {
          let ok = 0;
          const bad = [];
          for (const id of ids) {
            try {
              await del('/api/accounts/' + encodeURIComponent(id));
              ok++;
            } catch (e) {
              bad.push(id + '：' + e.message);
            }
          }
          sayAndReload('删了 ' + ok + ' 个' + (bad.length ? '，' + bad.length + ' 个没删掉' : ''));
        });
    });
  }
})();

// confirmCard 用自己的框问一句。
//
// 焦点先落在「取消」上：手快按回车不该删东西。
//
// 不用浏览器那个 confirm：那是浏览器的东西，不是这个后台的东西，而且它
// 一弹出来就把整个页面锁住了。
function confirmCard(title, lead, okText, onOk) {
  const card = makeCard(title, lead);
  const ok = document.createElement('button');
  ok.type = 'submit';
  ok.className = 'danger';
  ok.textContent = okText;
  const cancel = document.createElement('button');
  cancel.type = 'button';
  cancel.textContent = '取消';
  cancel.addEventListener('click', () => closeCard(card.box));
  // 两个动作摆一排：取消在左，正事在右。中间空着的地方别去填满。
  const acts = document.createElement('div');
  acts.className = 'card-actions';
  acts.append(cancel, ok);
  card.form.append(acts);
  card.form.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    ok.disabled = true;
    try {
      await onOk(card);
    } catch (e) {
      card.msg.textContent = e.message;
      ok.disabled = false;
    }
  });
  card.open(cancel);
  return card;
}

// askGroup 让人选一个组，给选中的账号换上。
//
// 组的名字从表格上那一列读：那一列记着库里所有的组。
function groupNames() {
  const table = document.querySelector('.acc-table');
  if (!table) return [];
  return (table.dataset.groups || '').split(',').filter(Boolean);
}

// groupList 把组里的地址读出来，一行一个。
//
// 地址在 data-group-list 里，是个 JSON 数组。属性名里不能带 url：
// Go 的 html/template 看见 url、src、uri 就当那是网址，把换行换成
// %0a，socks5 那种地址直接换成 #ZgotmplZ。
function groupList(raw) {
  if (!raw) return '';
  let list;
  try {
    list = JSON.parse(raw);
  } catch (e) {
    return '';
  }
  if (!Array.isArray(list)) return '';
  return list.join('\n');
}

function askGroup(ids) {
  const names = groupNames();
  const card = makeCard(
    '把选中的 ' + ids.length + ' 个账号放进哪个组',
    '选「跟随全局」就是回到全局默认。');
  const select = document.createElement('select');
  select.id = 'card-group-pick';
  const field = document.createElement('div');
  field.className = 'field';
  const label = document.createElement('label');
  label.textContent = '出口组';
  label.htmlFor = 'card-group-pick';
  const none = document.createElement('option');
  none.value = '';
  none.textContent = '跟随全局';
  select.appendChild(none);
  for (const n of names) {
    const opt = document.createElement('option');
    opt.value = n;
    opt.textContent = n;
    select.appendChild(opt);
  }
  field.append(label, select);
  const button = document.createElement('button');
  button.type = 'submit';
  button.className = 'primary';
  button.textContent = '换过去';
  card.form.append(field, button);
  card.form.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    button.disabled = true;
    card.msg.textContent = '';
    let ok = 0;
    const bad = [];
    for (const id of ids) {
      try {
        await patch('/api/accounts/' + encodeURIComponent(id), { proxy_group: select.value });
        ok++;
      } catch (e) {
        bad.push(id + '：' + e.message);
      }
    }
    if (bad.length) {
      // 有没换成的：框不能关。关了，那句「谁没换成、为什么」就没了。
      card.msg.textContent = '换了 ' + ok + ' 个，' + bad.length + ' 个没换成。' + bad.join('；');
      button.disabled = false;
      return;
    }
    closeCard(card.box);
    sayAndReload('换了 ' + ok + ' 个');
  });
  card.open(select);
}

// ---------------------------------------------------------------- 抽屉

// 点一行，右侧滑出那个号的详情。
//
// 一行的格子里挤不下所有的话：上次为什么掉的、上次读到多少积分、出口
// 是谁 —— 这些要点开了才看得全。
(function () {
  const table = document.querySelector('.table-wrap table');
  if (!table) return;
  table.addEventListener('click', (ev) => {
    if (ev.target.closest('input, select, button, a')) return;
    const row = ev.target.closest('tr[data-row]');
    if (!row) return;
    openDrawer(row);
  });
})();

let drawerBox = null;
// drawerOpener 是摆出抽屉的那一行：抽屉关上的时候，焦点得回到那儿，
// 不然人在一百行里就找不着刚才在看哪一行了。
let drawerOpener = null;
// drawerTrigger 是行里那个名字按钮：它得说出抽屉开着没有。
let drawerTrigger = null;

function closeDrawer() {
  if (!drawerBox) return;
  drawerBox.remove();
  const scrim = document.querySelector('.drawer-scrim');
  if (scrim) scrim.remove();
  drawerBox = null;
  document.body.classList.remove('has-drawer');
  if (drawerTrigger) {
    drawerTrigger.setAttribute('aria-expanded', 'false');
    drawerTrigger = null;
  }
  refreshInert();
  if (drawerOpener) {
    const opener = drawerOpener;
    drawerOpener = null;
    // <tr> 聚焦不了：焦点得还给行里那个名字按钮。
    const btn = opener.querySelector ? opener.querySelector('.row-open') : null;
    if (btn && btn.focus) btn.focus();
    else if (opener.focus) opener.focus();
  }
}

// openDrawer 摆出那个号的详情。
function openDrawer(row) {
  closeDrawer();
  drawerOpener = row;
  const d = row.dataset;
  const scrim = document.createElement('div');
  scrim.className = 'drawer-scrim';
  scrim.setAttribute('aria-hidden', 'true');
  scrim.addEventListener('click', closeDrawer);

  const box = document.createElement('aside');
  box.id = 'acc-drawer';
  box.className = 'drawer';
  box.setAttribute('role', 'dialog');
  box.setAttribute('aria-modal', 'true');
  box.setAttribute('tabindex', '-1');

  // 摆它出来的那个按钮，得说出抽屉开着没有。
  const trigger = row.querySelector ? row.querySelector('.row-open') : null;
  if (trigger) {
    trigger.setAttribute('aria-expanded', 'true');
    drawerTrigger = trigger;
  }

  const head = document.createElement('div');
  head.className = 'drawer-head';
  const title = document.createElement('h3');
  title.id = 'drawer-title';
  title.textContent = d.name || d.email || d.row;
  box.setAttribute('aria-labelledby', 'drawer-title');
  const spacer = document.createElement('span');
  spacer.className = 'spacer';
  const close = document.createElement('button');
  close.type = 'button';
  close.className = 'small';
  close.appendChild(icon('x'));
  close.setAttribute('aria-label', '关上');
  close.addEventListener('click', closeDrawer);
  head.append(title, spacer, close);

  const body = document.createElement('div');
  body.className = 'drawer-body';

  // 抽屉底下那句话：出了错在这儿说。右下角那个飘字被抽屉盖住了，
  // 出了错人在那儿根本看不见。
  const footMsg = document.createElement('p');
  footMsg.className = 'card-msg';
  footMsg.setAttribute('role', 'status');

  const list = document.createElement('dl');
  list.className = 'kv';
  const statusText = d.state || statusNames[d.status] || d.status;
  const left = d.hasAmounts === 'true'
    ? (Number(d.remaining).toFixed(2) + ' / ' + Number(d.granted).toFixed(2))
    : '读不到';
  const outlet = d.group ? ('组 ' + d.group) : (d.proxy || '跟随全局');
  const checked = d.checked === 'true' ? ('已签 ' + d.streak + ' 天') : '未签';
  // kv 是抽屉里那一列「名字 → 值」。改一件事的时候要改其中一行，就得有
  // 法子找到它：每一行给自己一个记号（data-kv）。
  const kv = {};
  for (const pair of [
    ['账号 ID', d.row, 'id'],
    ['邮箱', d.email || '没有', 'email'],
    ['状态', statusText, 'state'],
    ['积分', left, 'credits'],
    ['套餐', d.plan || '未知', 'plan'],
    ['额度窗口', d.window || '没有读到', 'window'],
    ['出口', outlet, 'outlet'],
    ['今日签到', checked, 'checked'],
  ]) {
    const dt = document.createElement('dt');
    dt.textContent = pair[0];
    const dd = document.createElement('dd');
    dd.textContent = pair[1];
    dd.dataset.kv = pair[2];
    kv[pair[2]] = dd;
    list.append(dt, dd);
  }
  body.appendChild(list);

  if (d.error) {
    const err = document.createElement('div');
    err.className = 'err';
    err.textContent = d.error;
    body.appendChild(err);
  }

  // 改标签：名字是给人看的，改了得马上看得见。
  const field = document.createElement('div');
  field.className = 'field';
  const nameLabel = document.createElement('label');
  nameLabel.textContent = '标签';
  nameLabel.htmlFor = 'drawer-name';
  const nameInput = document.createElement('input');
  nameInput.id = 'drawer-name';
  nameInput.value = d.name || '';
  const save = document.createElement('button');
  save.type = 'button';
  save.className = 'small';
  save.append(icon('pencil'), document.createTextNode('存回去'));
  save.addEventListener('click', async () => {
    save.disabled = true;
    try {
      await patch('/api/accounts/' + encodeURIComponent(d.row), { name: nameInput.value });
      row.dataset.name = nameInput.value;
      const cell = row.querySelector('td[data-sort]');
      if (cell) cell.dataset.sort = nameInput.value;
      // 表格里那个名字也改过来。抽屉开着的时候不重取整页：重取一次抽屉
      // 就没了，人还得再点一遍 —— 看着像白按了。
      const label = row.querySelector('.row-open');
      if (label) label.textContent = nameInput.value.trim() || '未命名';
      title.textContent = nameInput.value.trim() || d.email || d.row;
      footMsg.textContent = '已保存';
      say('已保存');
    } catch (e) {
      footMsg.textContent = '保存失败：' + e.message;
      say('保存失败：' + e.message);
    }
    save.disabled = false;
  });
  const nameRow = document.createElement('div');
  nameRow.className = 'row';
  nameRow.append(nameInput, save);
  field.append(nameLabel, nameRow);
  body.appendChild(field);

  // 出口：组和单独的地址。表里只写着这个号走哪儿，改在这儿改。
  const outletField = document.createElement('div');
  outletField.className = 'field';
  const outletLabel = document.createElement('label');
  outletLabel.textContent = '出口';
  outletLabel.htmlFor = 'drawer-group';
  const outletRow = document.createElement('div');
  outletRow.className = 'row';
  const groupSelect = document.createElement('select');
  groupSelect.id = 'drawer-group';
  groupSelect.dataset.account = d.row;
  groupSelect.dataset.field = 'proxy_group';
  groupSelect.setAttribute('aria-label', '出口组');
  const none = document.createElement('option');
  none.value = '';
  none.textContent = '跟随全局';
  groupSelect.appendChild(none);
  for (const n of groupNames()) {
    const opt = document.createElement('option');
    opt.value = n;
    opt.textContent = n;
    groupSelect.appendChild(opt);
  }
  groupSelect.value = d.group || '';
  // stored 是存着的样子：存不进去的时候要退回这儿来。屏幕上不能写着没
  // 发生过的事。
  groupSelect.dataset.stored = d.group || '';
  const urlInput = document.createElement('input');
  urlInput.id = 'drawer-url';
  urlInput.placeholder = '单独地址（空 = 跟随全局）';
  urlInput.setAttribute('aria-label', '单独地址');
  urlInput.dataset.account = d.row;
  urlInput.dataset.field = 'proxy_url';
  urlInput.dataset.stored = d.proxy || '';
  urlInput.value = d.proxy || '';
  outletRow.append(groupSelect, urlInput);
  outletField.append(outletLabel, outletRow);
  body.appendChild(outletField);

  const foot = document.createElement('div');
  foot.className = 'drawer-foot';
  // 读一次额度：抽屉里的数是上一次读到的，旧的（积分只会往下走，旧的数
  // 是高估）。这里现读一遍，抽屉里就地改过来。
  const refresh = document.createElement('button');
  refresh.type = 'button';
  refresh.className = 'icon';
  refresh.append(icon('refresh'), document.createTextNode('读一次额度'));
  refresh.addEventListener('click', async () => {
    refresh.disabled = true;
    footMsg.textContent = '正在读……';
    try {
      const answer = await call('/api/accounts/' + encodeURIComponent(d.row) + '/usage', undefined, '正在读额度');
      if (!answer.ok) throw new Error(messageOf(answer));
      const snap = answer.json || {};
      const has = snap.HasAmounts === true;
      kv.credits.textContent = has
        ? (Number(snap.Remaining).toFixed(2) + ' / ' + Number(snap.Granted).toFixed(2))
        : '读不到';
      kv.plan.textContent = snap.Plan || '未知';
      kv.window.textContent = windowWords(snap.WindowsJSON) || '没有读到';
      // 表格里那两格也跟着变。
      setCredits(row.querySelector('[data-credits]'), snap);
      const planCell = row.querySelector('[data-plan]');
      if (planCell && snap.Plan) planCell.textContent = snap.Plan;
      footMsg.textContent = '读完了';
      say('读完了');
    } catch (e) {
      footMsg.textContent = '读不到：' + e.message;
      say('读不到：' + e.message);
    }
    refresh.disabled = false;
  });
  const checkin = document.createElement('button');
  checkin.type = 'button';
  checkin.className = 'primary icon';
  checkin.append(icon('check'), document.createTextNode('立即签到'));
  checkin.addEventListener('click', async () => {
    checkin.disabled = true;
    footMsg.textContent = '正在签到……';
    try {
      const out = await post('/api/accounts/' + encodeURIComponent(d.row) + '/checkin', undefined, '正在签到');
      footMsg.textContent = outcomeWords(out);
      // 「今日签到」那一行改过来，表格里那一格也改过来。整页重取会把
      // 抽屉收掉。
      const w = checkedWord(out);
      if (w) kv.checked.textContent = w.word;
      const cell = row.querySelector('[data-checked-cell]');
      if (cell) setChecked(cell, out);
      say(outcomeWords(out));
    } catch (e) {
      footMsg.textContent = '失败：' + e.message;
      say('失败：' + e.message);
    }
    checkin.disabled = false;
  });
  const remove = document.createElement('button');
  remove.type = 'button';
  remove.className = 'danger icon';
  remove.append(icon('trash'), document.createTextNode('删掉'));
  remove.addEventListener('click', () => {
    confirmCard('删掉这个账号？', '它的令牌和读数也跟着没了。', '删掉', async (card) => {
      await del('/api/accounts/' + encodeURIComponent(d.row));
      row.remove();
      closeDrawer();
      closeCard(card.box);
      sayAndReload('删掉了');
    });
  });
  foot.append(refresh, checkin, remove, footMsg);

  box.append(head, body, foot);
  document.body.appendChild(scrim);
  document.body.appendChild(box);
  drawerBox = box;
  // 抽屉开着的时候底下的页面不滚，飘字也挪到抽屉左边去。
  document.body.classList.add('has-drawer');
  refreshInert();
  box.focus();
}

// Tab 不能从浮层里跑出去：抽屉或者飘出来的框开着的时候，后头的表格不该
// 还能按。
document.addEventListener('keydown', (ev) => {
  if (ev.key !== 'Tab') return;
  const layer = topLayer();
  if (!layer) return;
  const list = Array.from(layer.querySelectorAll(
    'button, input, select, textarea, [href], [tabindex]:not([tabindex="-1"])'));
  if (!list.length) return;
  const first = list[0];
  const last = list[list.length - 1];
  const idx = list.indexOf(document.activeElement);
  if (ev.shiftKey) {
    // 焦点在框自己身上（它是 tabindex="-1"，不在那一列里）也算在头一个前面。
    if (idx <= 0) {
      ev.preventDefault();
      last.focus();
    }
  } else if (idx === -1 || idx === list.length - 1) {
    ev.preventDefault();
    first.focus();
  }
});

// topLayer 是眼下最上面那层浮层：飘出来的框比抽屉高，先关框再关抽屉。
function topLayer() {
  const cards = document.querySelectorAll('.card');
  if (cards.length) return cards[cards.length - 1];
  return drawerBox;
}

document.addEventListener('click', async (ev) => {
  // 账号名是个按钮：按下去就摆出那个号的详情。
  const opener = ev.target.closest('.row-open');
  if (opener) {
    const row = opener.closest('tr[data-row]');
    if (row) openDrawer(row);
    return;
  }

  const checkin = ev.target.closest('button[data-checkin]');
  if (checkin) {
    const id = checkin.dataset.checkin;
    const row = checkin.closest('tr');
    const cell = row ? row.querySelector('[data-checked-cell]') : null;
    const use = checkin.querySelector('use');
    const svg = checkin.querySelector('svg');
    checkin.disabled = true;
    // 正在签到：图标转一圈，比把字换成「签到中……」安静。
    if (use) use.setAttribute('href', '/static/icons.svg#refresh');
    if (svg) svg.classList.add('is-spin');
    try {
      const out = await post('/api/accounts/' + encodeURIComponent(id) + '/checkin', undefined, '正在签到');
      // 结果摆进「今日签到」那一格：每一种结果都写。签过的写「已签 N
      // 天」，不能签的写「不能签」。只写签成了的那一种，表格就停在
      // 「未签」—— 人看着像没签成。
      if (cell) setChecked(cell, out);
      say(outcomeWords(out));
    } catch (e) {
      // 失败要说清楚为什么，还得能再按一次。
      if (use) use.setAttribute('href', '/static/icons.svg#alert-triangle');
      if (svg) svg.classList.remove('is-spin');
      checkin.disabled = false;
      checkin.title = String(e.message);
      checkin.setAttribute('aria-label', '签到失败：' + e.message);
      say('签到失败：' + e.message);
      return;
    }
    // 按钮还在那儿：还能再按一次（站点答「今天已经签过了」也是一句话）。
    // 把它抹掉的话，签过的号就没有按钮了 —— 看着像坏了。
    checkin.disabled = false;
    if (use) use.setAttribute('href', '/static/icons.svg#check');
    if (svg) svg.classList.remove('is-spin');
    return;
  }

  const egress = ev.target.closest('button[data-egress]');
  if (egress) {
    // 页面上显示的是去掉凭据的地址，服务器拿它去对上真的那个地址。
    const url = egress.dataset.egress;
    // 结果写在左边那格「健康」里，按钮自己还是那个按钮：按一下换了形状，
    // 表格就跳了。
    const health = egress.closest('td') ? egress.closest('td').previousElementSibling : null;
    egress.disabled = true;
    egress.textContent = '测试中……';
    try {
      const out = await post('/api/egress/test', { url }, '正在测出口');
      if (health) {
        health.textContent = '';
        const pill = document.createElement('span');
        pill.className = 'pill ' + (out.ok ? 'good' : 'bad');
        pill.textContent = out.ok ? '可用' : '不可用';
        health.appendChild(pill);
        if (!out.ok && out.error) {
          const sub = document.createElement('div');
          sub.className = 'sub bad';
          sub.textContent = out.error;
          health.appendChild(sub);
        }
      }
      egress.textContent = '再测一次';
      egress.title = out.ok ? '这条地址现在能用' : (out.error || '这条地址用不了');
    } catch (e) {
      if (health) {
        health.textContent = '';
        const pill = document.createElement('span');
        pill.className = 'pill bad';
        pill.textContent = '测不了';
        health.appendChild(pill);
        const sub = document.createElement('div');
        sub.className = 'sub bad';
        sub.textContent = e.message;
        health.appendChild(sub);
      }
      egress.textContent = '再测一次';
      egress.title = e.message;
    } finally {
      egress.disabled = false;
    }
    return;
  }

  const groupEdit = ev.target.closest('button[data-group-edit]');
  if (groupEdit) {
    editGroup(groupEdit.dataset.groupEdit, groupList(groupEdit.dataset.groupList));
    return;
  }

  const groupNew = ev.target.closest('button[data-group-new]');
  if (groupNew) {
    editGroup('', '');
    return;
  }

  const groupRm = ev.target.closest('button[data-group-rm]');
  if (groupRm) {
    const name = groupRm.dataset.groupRm;
    confirmCard('删掉组 ' + name + '？', '指着它的账号回到全局组。', '删掉', async (card) => {
      await del(`/api/proxy-groups?name=${encodeURIComponent(name)}`);
      closeCard(card.box);
      sayAndReload('删掉了组 ' + name);
    });
    return;
  }

  if (ev.target.id === 'start-login') {
    const box = document.getElementById('login-result');
    const button = ev.target.closest('#start-login');
    // 登录要走验证码，可能要等一会儿：按钮锁着，别让人按两遍。
    if (button) button.disabled = true;
    box.textContent = '请稍候……';
    try {
      const out = await post('/api/accounts');
      // 验证码和地址是上游给的：它们只作为字和地址进页面，不作为
      // 标记。地址还得是 http 或者 https 才配当链接。
      box.textContent = '';
      // 这一格讲的事变了：不再是「等一下」，而是「成了，去填验证码」。
      box.className = 'notice ok';
      const code = document.createElement('strong');
      code.textContent = out.user_code;
      const link = document.createElement('a');
      link.textContent = '这个页面';
      link.target = '_blank';
      link.rel = 'noopener noreferrer';
      const href = safeURL(out.verification_url);
      if (href) link.href = href;
      box.append('把验证码 ', code, ' 填到 ', link, '。');
      poll(out.id);
    } catch (e) {
      box.className = 'notice err';
      box.textContent = '失败：' + e.message;
      if (button) button.disabled = false;
    }
  }
});

document.addEventListener('change', async (ev) => {
  const sel = ev.target.closest('[data-field]');
  if (!sel) return;
  const body = {};
  body[sel.dataset.field] = sel.value;
  const account = sel.dataset.account || '';
  // 抽屉里那对东西和表格里的是同一个号：找得到它们，就知道改完要把哪
  // 几处摆成新的样子。
  const pick = document.querySelector(`select[data-account="${account}"][data-field="proxy_group"]`);
  const url = document.querySelector(`input[data-account="${account}"][data-field="proxy_url"]`);
  const row = sel.closest('tr') || document.querySelector(`tr[data-row="${account}"]`);
  // 地址和组是二选一：选了组，地址框就清掉；填了地址，下拉框就回到空。
  if (sel.dataset.field === 'proxy_group' && sel.value && url) url.value = '';
  if (sel.dataset.field === 'proxy_url' && sel.value && pick) pick.value = '';
  // 存不进去的时候，框里的话得退回去：屏幕上不能写着没发生过的事。
  // 退回到存着的样子（stored），不是退回到空。
  const restore = () => {
    if (pick) pick.value = pick.dataset.stored || '';
    if (url) url.value = url.dataset.stored || '';
  };
  sel.disabled = true;
  try {
    await patch(`/api/accounts/${encodeURIComponent(account)}`, body, '正在保存');
    // 存进去了：现在框里的话是真的了，把它记下来。
    if (pick) pick.dataset.stored = pick.value;
    if (url) url.dataset.stored = url.value;
    if (row) {
      if (sel.dataset.field === 'proxy_group') row.dataset.group = sel.value;
      else row.dataset.proxy = sel.value;
      const cell = row.querySelector('[data-outlet]');
      if (cell) {
        cell.textContent = '';
        if (sel.dataset.field === 'proxy_group') {
          if (sel.value) cell.textContent = sel.value;
          else cell.textContent = '跟随全局';
        } else if (sel.value) {
          const span = document.createElement('span');
          span.className = 'mono clip';
          span.textContent = sel.value;
          cell.appendChild(span);
        } else {
          cell.textContent = '跟随全局';
        }
      }
    }
    // 抽屉里那一行也改过来：抽屉开着的时候不重取整页。
    const kvOutlet = document.querySelector('#acc-drawer [data-kv="outlet"]');
    if (kvOutlet) {
      const group = pick ? pick.value : '';
      const address = url ? url.value : '';
      kvOutlet.textContent = group ? ('组 ' + group) : (address || '跟随全局');
    }
    say('已保存');
  } catch (e) {
    restore();
    say('保存失败：' + e.message);
  } finally {
    sel.disabled = false;
  }
});

// poll 问一次登录走到哪儿了。
//
// 等的是人把验证码填到站点上：那是几分钟的事，也可能人填完就忘了。这
// 个轮询不能永远转下去 —— 等够了就停，把话摆出来，人还能再按一次。
const pollWait = 10 * 60 * 1000;

function poll(id) {
  const started = Date.now();
  const timer = setInterval(async () => {
    if (Date.now() - started > pollWait) {
      clearInterval(timer);
      const box = document.getElementById('poll-result');
      if (box) box.textContent = '等太久了，停了。回上边重新开始登录。';
      const button = document.getElementById('start-login');
      if (button) button.disabled = false;
      return;
    }
    try {
      const out = await post(`/api/accounts/poll`, { id });
      const box = document.getElementById('poll-result');
      if (out.done) {
        clearInterval(timer);
        if (box) box.textContent = '登录成功，正在刷新……';
        location.reload();
      } else if (out.error) {
        clearInterval(timer);
        if (box) box.textContent = '失败：' + out.error;
        const button = document.getElementById('start-login');
        if (button) button.disabled = false;
      }
    } catch (e) {
      /* 继续等 */
    }
  }, 3000);
}

// 导入：把文件交上去，然后一直问那一次导入走到哪儿了。
//
// 上传回来的只有一次导入的编号，进度得自己问。
document.addEventListener('submit', async (ev) => {
  const form = ev.target.closest('#import-form');
  if (!form) return;
  ev.preventDefault();
  const status = document.getElementById('import-status');
  const input = form.querySelector('input[type=file]');
  const button = form.querySelector('button[type=submit]');
  if (!input || !input.files || !input.files[0]) return;
  // 这件事要跑好一会儿：按钮得锁住，不然人按两下就导了两遍。
  lockImport(true);
  status.textContent = '上传中……';
  const data = new FormData();
  data.append('file', input.files[0]);
  try {
    const answer = await call('/api/import', { method: 'POST', body: data });
    if (!answer.ok) throw new Error(messageOf(answer));
    status.textContent = '导入中……';
    watchImport(answer.json.id);
  } catch (e) {
    status.textContent = '失败：' + e.message;
    lockImport(false);
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
  lockImport(true);
  status.textContent = '导入中……';
  try {
    const answer = await call('/api/import', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ list: list }),
    });
    if (!answer.ok) throw new Error(messageOf(answer));
    watchImport(answer.json.id);
  } catch (e) {
    status.textContent = '失败：' + e.message;
    lockImport(false);
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
  // 正在导入的时候别把按钮放开了：那是另一次导入的按钮。
  if (button && !importRunning) button.disabled = lines > importTextLimit;
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

// importWait is how long a single import run may take.
//
// 一百个账号，一个一个登上去：那是好几分钟的事。给得宽一点，可也得有个
// 头：不然服务端那边卡住了，这个轮询就永远转下去。
const importWait = 20 * 60 * 1000;

// importRunning 记着有一趟导入正在跑。
//
// 跑着的时候，两个「开始」按钮都得锁着：不然人以为按了一下没动静，又
// 按一下，就导了两遍。
let importRunning = false;

// lockImport locks both import buttons while a run is going.
function lockImport(on) {
  importRunning = on;
  const text = document.getElementById('import-text');
  const lines = countListLines(text ? text.value : '');
  for (const b of document.querySelectorAll('#import-form button[type=submit]')) {
    b.disabled = on;
  }
  const tb = document.getElementById('import-text-button');
  if (tb) tb.disabled = on || lines > importTextLimit;
}

function watchImport(id) {
  const started = Date.now();
  const timer = setInterval(async () => {
    if (Date.now() - started > importWait) {
      clearInterval(timer);
      lockImport(false);
      const status = document.getElementById('import-status');
      if (status) status.textContent = '导入等太久了，停了。看看下面的结果，或者再导一次。';
      sayImport('导入等太久了，停了。');
      return;
    }
    try {
      const answer = await call('/api/import/' + encodeURIComponent(id));
      if (!answer.ok) throw new Error(messageOf(answer));
      const run = answer.json;
      const status = document.getElementById('import-status');
      if (run.finished_at) {
        clearInterval(timer);
        lockImport(false);
        // 完了就把「导入中」换掉：不然那句话一直挂在那儿，人以为还在跑。
        status.textContent = '导入完了：成功 ' + (run.ok || 0) + '，失败 ' + (run.failed || 0);
        showImportResults(run);
        sayImport(status.textContent);
      } else {
        status.textContent = '导入中……成功 ' + run.ok + '，失败 ' + run.failed;
      }
    } catch (e) {
      clearInterval(timer);
      lockImport(false);
      const status = document.getElementById('import-status');
      if (status) status.textContent = '失败：' + e.message;
      sayImport('失败：' + e.message);
    }
  }, 3000);
}

// sayImport 念一句导入的话。
//
// 进度那一行每三秒变一次，不能拿去念：读屏器自己都说不过来。完了的那
// 一句才是该念的。
function sayImport(text) {
  const live = document.getElementById('import-live');
  if (live) live.textContent = text;
}

function showImportResults(run) {
  const table = document.getElementById('import-results');
  if (!table) return;
  const body = table.querySelector('tbody');
  body.textContent = '';
  let results = [];
  try { results = JSON.parse(run.results || '[]'); } catch (e) { results = []; }
  // 解析出来的东西得是数组才能遍历：JSON.parse('null') 是成的，出来的是
  // null，for...of 一个 null 就炸了，页面上写着「失败」，结果表也不出来。
  if (!Array.isArray(results)) results = [];
  for (const r of results) {
    const tr = document.createElement('tr');
    // 整趟没成的那一行不是账号：它写的是「整趟」，不是「失败」——不然
    // 表里有一个「失败」，而上面那句说的是「失败 0」，两处对不上。
    const label = r.whole ? '整趟' : (r.ok ? '成功' : '失败');
    const cells = [r.email, label, r.message || ''];
    for (const c of cells) {
      const td = document.createElement('td');
      td.textContent = c;
      tr.appendChild(td);
    }
    body.appendChild(tr);
  }
  table.hidden = false;
  // 整趟没成的那一行不是账号，它说的是这一趟出了什么事。写在那句
  // 「成功几个失败几个」的后面：失败了几个账号是账号的事，整趟为什么
  // 没成是另一回事，两句话得都在。
  const whole = results.find((r) => r && r.whole);
  if (whole) {
    const status = document.getElementById('import-status');
    if (status) {
      status.textContent = '导入完了：成功 ' + (run.ok || 0) + '，失败 ' + (run.failed || 0) +
        '。' + (whole.message || '整趟导入没能完成');
    }
  }
}

// 重登：把断了的账号重新登一遍。
//
// 两个门：一个登全部，一个登一个。登一个是给人用的：一趟里登二十个，
// 人要是只想登一个，就得能点那一个。
document.addEventListener('click', async (ev) => {
  const one = ev.target.closest('[data-relogin]');
  if (one) {
    const id = one.getAttribute('data-relogin');
    const status = document.getElementById('relogin-status') || document.getElementById('status');
    one.disabled = true;
    if (status) status.textContent = '重登中……';
    try {
      const answer = await call('/api/relogin/' + encodeURIComponent(id), { method: 'POST' });
      if (!answer.ok) throw new Error(messageOf(answer));
      if (status) status.textContent = '登回去了：' + id;
      // 这一行已经修好了：从页面上拿掉，剩下的行还在那儿等着。
      // 理由那一行也是这个账号的，一块儿拿掉。
      const row = one.closest('[data-relogin-row]');
      if (row) {
        const who = row.getAttribute('data-relogin-row');
        row.remove();
        document.querySelectorAll('[data-relogin-why="' + who + '"]').forEach(n => n.remove());
      }
    } catch (e) {
      if (status) status.textContent = '失败：' + e.message;
      one.disabled = false;
    }
    return;
  }
  const button = ev.target.closest('#relogin-all');
  if (!button) return;
  const status = document.getElementById('relogin-status');
  // 重登要跑好一会儿：按钮锁住，不然人按两下就登了两遍。
  button.disabled = true;
  status.textContent = '重登中……';
  try {
    const answer = await call('/api/relogin', { method: 'POST' });
    if (!answer.ok) throw new Error(messageOf(answer));
    // 理由要摆出来：只说「失败 2」，人得自己去一个一个看是哪个账号、
    // 为什么。
    const reasons = Array.isArray(answer.json.reasons) ? answer.json.reasons : [];
    let text = '成功 ' + answer.json.fixed + '，失败 ' + answer.json.failed;
    if (reasons.length) {
      text += '。' + reasons.join('；');
    }
    status.textContent = text;
  } catch (e) {
    status.textContent = '失败：' + e.message;
  } finally {
    button.disabled = false;
  }
});

// 用量：现读一遍账本。
//
// 页面上那些数是上一次读数时的数，按一下「刷新」就是现在读一遍。
// 读完之后要把整页取回来——页面是服务端渲染的，只有重新取一次才是
// 现在的数。
document.addEventListener('click', async (ev) => {
  const button = ev.target.closest('#usage-refresh');
  if (!button) return;
  const status = document.getElementById('usage-status');
  button.disabled = true;
  if (status) status.textContent = '正在读账本……';
  try {
    const answer = await call('/api/usage/refresh', { method: 'POST' });
    if (!answer.ok) throw new Error(messageOf(answer));
    // 理由要摆出来：只说「失败 1」，人得自己去一个一个看是哪个账号、
    // 为什么。
    const reasons = Array.isArray(answer.json.reasons) ? answer.json.reasons : [];
    let text = '读了 ' + answer.json.refreshed + ' 个账号，失败 ' + answer.json.failed;
    if (reasons.length) {
      text += '。' + reasons.join('；');
    }
    // 刷新会把页面上的一切换掉，刚才那句话得留下来。
    try {
      sessionStorage.setItem('mmxcode-usage-refresh', text);
    } catch (e) {
      /* 存不下也就算了 */
    }
    location.reload();
  } catch (e) {
    if (status) status.textContent = '失败：' + e.message;
    button.disabled = false;
  }
});

// 刷新之后把刚才那句话摆出来。
(function () {
  let text = '';
  try {
    text = sessionStorage.getItem('mmxcode-usage-refresh') || '';
    sessionStorage.removeItem('mmxcode-usage-refresh');
  } catch (e) {
    text = '';
  }
  if (!text) return;
  const status = document.getElementById('usage-status');
  if (status) status.textContent = text;
})();

// 用量页上那张图：跟着指针走的那根竖线，和浮着的那个数。
//
// 图是服务端画好的：没有脚本的时候它也在，线、点、格线、那几个数都在。
// 这里添的是那层交互：
//
//   - 指针落在哪一块，那一块就是哪一天（不用瞄那个两像素的点）；
//   - 键盘上左右键走日子，看到的是同一层东西；
//   - 点一天就去问那一天是哪些号在用，问回来就地摆上，不重画这一页。
(function () {
  const fig = document.getElementById('usage-chart');
  if (!fig) return;
  const svg = fig.querySelector('.chart-svg');
  const tip = document.getElementById('chart-tip');
  const cross = fig.querySelector('.chart-cross');
  const hoverDot = document.getElementById('chart-hover-dot');
  if (!svg || !tip || !cross) return;
  const days = Array.prototype.slice.call(fig.querySelectorAll('.chart-day'));
  if (!days.length) return;

  // 那个浮着的数摆在哪：那个点上面。摆不下就摆下面。
  //
  // 位置是百分比：那张皮是拉开的，字那层不是，所以字的大小不变，位置
  // 跟着皮走。贴着边的点，浮着的数要往里让，不然它掉出图外。
  function place(day) {
    const left = Number(day.dataset.left);
    const top = Number(day.dataset.top);
    tip.hidden = false;
    tip.classList.remove('is-start', 'is-middle', 'is-end', 'is-below');
    if (left > 72) tip.classList.add('is-end');
    else if (left < 16) tip.classList.add('is-start');
    else tip.classList.add('is-middle');
    if (top < 28) tip.classList.add('is-below');
    tip.style.left = left + '%';
    tip.style.top = top + '%';
  }

  // 摆那个数：值在前，名字在后（这里要的是数，名字是次要的）。
  function show(day) {
    days.forEach(function (d) { d.classList.remove('is-on'); });
    day.classList.add('is-on');
    const cx = Number(day.dataset.cx);
    cross.setAttribute('x1', cx);
    cross.setAttribute('x2', cx);
    cross.classList.add('is-on');
    if (hoverDot) {
      hoverDot.style.left = day.dataset.left + '%';
      hoverDot.style.top = day.dataset.top + '%';
      hoverDot.hidden = false;
    }
    tip.textContent = '';
    const value = document.createElement('strong');
    value.textContent = Number(day.dataset.used).toLocaleString('zh-CN',
      { minimumFractionDigits: 2, maximumFractionDigits: 2 }) + ' 分';
    const name = document.createElement('span');
    name.textContent = day.dataset.short + ' · ' + day.dataset.accounts + ' 个号';
    tip.appendChild(value);
    tip.appendChild(name);
    place(day);
  }

  function hide() {
    days.forEach(function (d) { d.classList.remove('is-on'); });
    cross.classList.remove('is-on');
    if (hoverDot) hoverDot.hidden = true;
    tip.hidden = true;
  }

  fig.addEventListener('pointermove', (ev) => {
    const day = ev.target.closest ? ev.target.closest('.chart-day') : null;
    if (day) show(day);
  });

  fig.addEventListener('pointerleave', hide);

  // 键盘上也是同一层东西：左右键走日子。
  fig.addEventListener('keydown', (ev) => {
    if (ev.key !== 'ArrowLeft' && ev.key !== 'ArrowRight') return;
    ev.preventDefault();
    const at = days.indexOf(document.querySelector('.chart-day.is-on'));
    let next = at < 0 ? 0 : at + (ev.key === 'ArrowRight' ? 1 : -1);
    if (next < 0) next = days.length - 1;
    if (next >= days.length) next = 0;
    show(days[next]);
  });

  fig.addEventListener('focus', () => {
    if (document.querySelector('.chart-day.is-on')) return;
    show(days[days.length - 1]);
  });

  // 点一天：问那一天是哪些号在用，问回来就地摆上。
  document.addEventListener('click', async (ev) => {
    const link = ev.target.closest ? ev.target.closest('.chart-days a') : null;
    if (!link) return;
    const day = link.querySelector('.chart-day');
    if (!day) return;
    ev.preventDefault();
    const href = link.getAttribute('href');
    const panel = document.getElementById('usage-day');
    const body = document.getElementById('usage-day-body');
    const title = document.getElementById('usage-day-title');
    if (!panel || !body) {
      location.href = href;
      return;
    }
    try {
      const answer = await call('/api/usage/day?day=' + encodeURIComponent(day.dataset.day));
      if (!answer.ok) throw new Error(messageOf(answer));
      body.textContent = '';
      const list = document.createElement('ul');
      list.className = 'rows';
      const accounts = Array.isArray(answer.json.accounts) ? answer.json.accounts : [];
      accounts.forEach((a) => {
        const li = document.createElement('li');
        const who = document.createElement('span');
        who.className = 'who';
        const t = document.createElement('span');
        t.className = 't';
        t.textContent = a.name || a.id;
        const s = document.createElement('span');
        s.className = 's mono';
        s.textContent = a.id;
        who.appendChild(t);
        who.appendChild(s);
        const pill = document.createElement('span');
        pill.className = 'pill';
        pill.textContent = a.used + ' 分';
        li.appendChild(who);
        li.appendChild(pill);
        list.appendChild(li);
      });
      if (!accounts.length) {
        const li = document.createElement('li');
        li.className = 'muted';
        li.textContent = '那天没有账。';
        list.appendChild(li);
      }
      body.appendChild(list);
      if (title) title.textContent = day.dataset.day + ' 是哪些号在用';
      panel.hidden = false;
      history.pushState(null, '', href);
    } catch (e) {
      // 问不到就照旧走那一遍：链接还在，页面自己会摆出来。
      location.href = href;
    }
  });
})();

// 密钥：申请一把新的，撤销一把旧的。
//
// 申请出来的那一把 key 只在这一刻给得出：往后页面上只有它的开头。所以
// 它得摆在人眼前，还得能复制走。
document.addEventListener('submit', async (ev) => {
  const form = ev.target.closest('#key-form');
  if (!form) return;
  ev.preventDefault();
  const status = document.getElementById('key-status');
  const name = document.getElementById('key-name');
  const button = form.querySelector('button[type=submit]');
  if (status) status.textContent = '正在申请……';
  if (button) button.disabled = true;
  try {
    const out = await post('/api/keys', { name: name ? name.value : '' });
    const plain = document.getElementById('key-plain');
    if (plain) {
      plain.textContent = out.key;
      // 焦点落在 key 上：Ctrl+A、Ctrl+C 直接能用。
      plain.focus();
      plain.setSelectionRange(0, out.key.length);
    }
    const box = document.getElementById('key-new');
    if (box) box.hidden = false;
    if (status) status.textContent = '申请好了。抄走收好：过了这一会儿就拿不出来了。';
    if (name) name.value = '';
    await renderKeys();
  } catch (e) {
    if (status) status.textContent = '失败：' + e.message;
  } finally {
    if (button) button.disabled = false;
  }
});

document.addEventListener('click', async (ev) => {
  const copy = ev.target.closest('#key-copy');
  if (copy) {
    const plain = document.getElementById('key-plain');
    if (!plain) return;
    const label = document.getElementById('key-copy-label');
    try {
      await navigator.clipboard.writeText(plain.textContent);
      if (label) label.textContent = '复制好了';
    } catch (e) {
      // 剪贴板不给用的时候，人自己选中那段字也一样。
      if (label) label.textContent = '选中最下面那段字自己复制';
    }
    return;
  }
  const revoke = ev.target.closest('button[data-revoke]');
  if (revoke) {
    const id = revoke.dataset.revoke;
    const name = revoke.dataset.name || '这把 key';
    // 撤销了就是废了：拿它的那边从此答 401。得先问一句。
    if (!confirm('撤销' + name + '？用它的那边从此就答 401 了。')) return;
    revoke.disabled = true;
    try {
      await del('/api/keys/' + encodeURIComponent(id));
      await renderKeys();
      const status = document.getElementById('key-status');
      if (status) status.textContent = '撤销了' + name;
    } catch (e) {
      revoke.disabled = false;
      const status = document.getElementById('key-status');
      if (status) status.textContent = '失败：' + e.message;
    }
  }
});

// renderKeys 把钥匙列表重新摆一遍。
//
// 页面是服务端渲染的，可这一页的列表是刚刚才变的：重新取一遍整页会把
// 刚拿到手的那把 key 弄没，所以只把表格重摆一次。
async function renderKeys() {
  const body = document.getElementById('key-rows');
  if (!body) return;
  const answer = await call('/api/keys');
  if (!answer.ok) return;
  const keys = Array.isArray(answer.json.keys) ? answer.json.keys : [];
  body.textContent = '';
  for (const k of keys) {
    const tr = document.createElement('tr');
    const revoked = !!k.revoked_at;
    const cells = [
      k.name || '（没名字）',
      k.prefix || '',
      (k.created_at || '').replace('T', ' ').slice(0, 16),
    ];
    for (const c of cells) {
      const td = document.createElement('td');
      td.textContent = c;
      tr.appendChild(td);
    }
    const state = document.createElement('td');
    const label = document.createElement('span');
    if (revoked) {
      label.textContent = '已撤销';
      label.className = 'muted';
    } else {
      label.textContent = '能用';
      label.className = 'good';
    }
    state.appendChild(label);
    tr.appendChild(state);
    const last = document.createElement('td');
    if (!revoked) {
      const button = document.createElement('button');
      button.type = 'button';
      button.className = 'small danger';
      button.textContent = '撤销';
      button.dataset.revoke = k.id;
      button.dataset.name = k.name || '这把 key';
      button.setAttribute('aria-label', '撤销 ' + (k.name || '这把 key'));
      last.appendChild(button);
    }
    tr.appendChild(last);
    body.appendChild(tr);
  }
  if (!keys.length) {
    // 空清单也得有话说：不然表里就是一片白。
    const tr = document.createElement('tr');
    const td = document.createElement('td');
    td.className = 'empty';
    td.colSpan = 5;
    td.textContent = '还没有申请过 key。';
    tr.appendChild(td);
    body.appendChild(tr);
  }
  // 重摆完表，焦点得还在表里：别把人甩回页面顶上。
  const again = body.querySelector('button[data-revoke]');
  if (again) again.focus();
}

// editGroup 改一个组。
//
// 组里的地址一行一个，顺序就是故障转移的顺序：第一个是主路，后面的都是
// 备路。名字空着就是新建一个。
function editGroup(name, urls) {
  const card = makeCard(
    name ? ('组 ' + name + ' 的地址') : '新建一个组',
    '一行一个，第一个是主路，后面的都是备路。');
  const nameField = document.createElement('div');
  nameField.className = 'field';
  const nameLabel = document.createElement('label');
  nameLabel.textContent = '组的名字';
  nameLabel.htmlFor = 'card-group-name';
  const nameInput = document.createElement('input');
  nameInput.id = 'card-group-name';
  nameInput.placeholder = '比如：clash';
  nameInput.value = name;
  nameField.append(nameLabel, nameInput);

  const addrField = document.createElement('div');
  addrField.className = 'field';
  const addrLabel = document.createElement('label');
  addrLabel.textContent = '组里的地址';
  addrLabel.htmlFor = 'card-group-list';
  const input = document.createElement('textarea');
  input.id = 'card-group-list';
  input.rows = 6;
  input.value = urls;
  addrField.append(addrLabel, input);

  const button = document.createElement('button');
  button.type = 'submit';
  button.className = 'primary';
  button.textContent = '存回去';
  card.form.append(nameField, addrField, button);
  card.form.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    button.disabled = true;
    card.msg.textContent = '';
    const groupName = nameInput.value.trim();
    if (!groupName) {
      card.msg.textContent = '这个组还没有名字。';
      button.disabled = false;
      return;
    }
    try {
      const list = input.value.split('\n').map((s) => s.trim()).filter(Boolean);
      await post('/api/proxy-groups', { name: groupName, urls: list });
      closeCard(card.box);
      sayAndReload('存好了');
    } catch (e) {
      card.msg.textContent = e.message;
      button.disabled = false;
    }
  });
  card.open(name ? input : nameInput);
}

// ---------------------------------------------------------------- 侧栏

// 侧栏能收成一条图标道：宽的表格要地方的时候用得上。
//
// 收起来的样子记下来：下次打开还是那个样子。
(function () {
  const button = document.getElementById('side-toggle');
  if (!button) return;
  let min = false;
  try {
    min = localStorage.getItem('mmxcode-side') === 'min';
  } catch (e) {
    min = false;
  }
  const paint = () => {
    document.body.classList.toggle('side-min', min);
    button.setAttribute('aria-expanded', min ? 'false' : 'true');
    const text = min ? '摊开侧栏' : '收起侧栏';
    button.setAttribute('aria-label', text);
    button.title = text;
  };
  paint();
  button.addEventListener('click', () => {
    min = !min;
    try {
      localStorage.setItem('mmxcode-side', min ? 'min' : 'full');
    } catch (e) {
      /* 存不下也就算了 */
    }
    paint();
  });
})();

// ---------------------------------------------------------------- 折叠

// 面板的头就是开关：点一下收起来，再点一下摊开。
//
// 一页上摆着四块东西，其中两块这会儿用不上：收起来眼前就干净了。收起来
// 的样子记下来：下次打开还是那个样子。
(function () {
  const panels = Array.from(document.querySelectorAll('.panel[data-panel]'));
  if (!panels.length) return;
  let saved = {};
  try {
    saved = JSON.parse(localStorage.getItem('mmxcode-panels') || '{}') || {};
  } catch (e) {
    saved = {};
  }
  const paint = (panel) => {
    const id = panel.dataset.panel;
    const button = panel.querySelector('.panel-toggle');
    const body = panel.querySelector('.panel-body');
    if (!button || !body) return;
    // 存下来的 false 才是收起来的。没存过的都是摊开的。
    const open = saved[id] !== false;
    panel.classList.toggle('is-collapsed', !open);
    button.setAttribute('aria-expanded', open ? 'true' : 'false');
    body.hidden = !open;
  };
  for (const panel of panels) {
    paint(panel);
    const button = panel.querySelector('.panel-toggle');
    if (!button) continue;
    button.addEventListener('click', () => {
      const id = panel.dataset.panel;
      const open = button.getAttribute('aria-expanded') === 'true';
      saved[id] = !open;
      try {
        localStorage.setItem('mmxcode-panels', JSON.stringify(saved));
      } catch (e) {
        /* 存不下也就算了 */
      }
      paint(panel);
    });
  }
})();

// ---------------------------------------------------------------- 快捷键

// / 走到筛账号那格，Esc 把那格清空。
//
// 一百行的表，筛是最快的那一下：从顶上 Tab 到那个框要按十几下。
document.addEventListener('keydown', (ev) => {
  const tag = (ev.target && ev.target.tagName) || '';
  const typing = tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' ||
    (ev.target && ev.target.isContentEditable);
  if (ev.key === '/' && !typing && !ev.metaKey && !ev.ctrlKey && !ev.altKey) {
    const box = document.getElementById('account-filter');
    if (!box) return;
    ev.preventDefault();
    box.focus();
    box.select();
    return;
  }
  if (ev.key === 'Escape' && typing) {
    const box = document.getElementById('account-filter');
    if (box && ev.target === box && box.value) {
      box.value = '';
      box.dispatchEvent(new Event('input', { bubbles: true }));
    }
  }
});

// Ctrl+K 弹命令面板：不用记地址就能跳到任何一页。
//
// 面板自己是个模态：它开着的时候，底下的页面不该还能按。
const paletteCommands = [
  { label: '概览', ic: 'layout-dashboard', to: '/' },
  { label: '账号', ic: 'users', to: '/accounts' },
  { label: '用量', ic: 'chart-bar', to: '/usage' },
  { label: '出口', ic: 'world', to: '/egress' },
  { label: '密钥', ic: 'key', to: '/keys' },
  { label: '登录新账号', ic: 'login', to: '/login' },
  { label: '导入账号', ic: 'upload', to: '/import' },
  { label: '重登掉线的账号', ic: 'refresh', to: '/relogin' },
];

let paletteBox = null;

function closePalette() {
  if (!paletteBox) return;
  paletteBox.remove();
  paletteBox = null;
  refreshInert();
}

function openPalette() {
  closePalette();
  const box = document.createElement('div');
  box.className = 'card';
  const panel = document.createElement('div');
  panel.className = 'card-box palette-box';
  panel.setAttribute('role', 'dialog');
  panel.setAttribute('aria-modal', 'true');
  panel.setAttribute('aria-label', '命令');
  const input = document.createElement('input');
  input.type = 'search';
  input.placeholder = '去哪儿，或者做什么';
  input.setAttribute('aria-label', '命令');
  const list = document.createElement('ul');
  list.className = 'palette';
  const paint = (q) => {
    list.textContent = '';
    const hit = paletteCommands.filter((c) => c.label.indexOf(q) >= 0);
    for (const c of hit) {
      const li = document.createElement('li');
      const b = document.createElement('button');
      b.type = 'button';
      b.className = 'palette-item';
      b.append(icon(c.ic, 'ic ic-sm'), document.createTextNode(c.label));
      b.addEventListener('click', () => {
        closePalette();
        location.href = c.to;
      });
      li.appendChild(b);
      list.appendChild(li);
    }
    if (!hit.length) {
      const li = document.createElement('li');
      li.className = 'muted';
      li.textContent = '没有这样的命令';
      list.appendChild(li);
    }
  };
  input.addEventListener('input', () => paint(input.value.trim()));
  paint('');
  panel.append(input, list);
  box.appendChild(panel);
  box.addEventListener('click', (ev) => {
    if (ev.target === box) closePalette();
  });
  document.body.appendChild(box);
  paletteBox = box;
  refreshInert();
  input.focus();
}

document.addEventListener('keydown', (ev) => {
  if ((ev.metaKey || ev.ctrlKey) && (ev.key || '').toLowerCase() === 'k') {
    ev.preventDefault();
    if (paletteBox) closePalette();
    else openPalette();
  } else if (ev.key === 'Escape' && paletteBox) {
    ev.preventDefault();
    closePalette();
  }
});

// 顶栏上那个按钮：快捷键藏得太深，得有个能看见的入口。
(function () {
  const button = document.getElementById('palette-open');
  if (!button) return;
  button.addEventListener('click', openPalette);
  // 苹果的机器上按的是 ⌘，别在人家屏幕上写 Ctrl。
  const key = document.getElementById('palette-key');
  if (key) {
    const mac = /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent || '');
    key.textContent = mac ? '⌘K' : 'Ctrl K';
    button.setAttribute('aria-label', mac ? '命令面板（⌘K）' : '命令面板（Ctrl+K）');
  }
})();

// ---------------------------------------------------------------- 右键

// 右键一行：几个常用的动作摆出来。
//
// 一百行里找一个号的 ID：以前得点开抽屉抄；现在右键就抄。
document.addEventListener('contextmenu', (ev) => {
  const row = ev.target.closest('tr[data-row]');
  if (!row) return;
  ev.preventDefault();
  const id = row.dataset.row || '';
  const mail = row.dataset.email || '';
  const name = row.dataset.name || mail || id;
  const items = [
    { label: '打开详情', ic: 'pencil', run: () => openDrawer(row) },
    { label: '复制账号 ID', ic: 'copy', run: () => copyText(id) },
    { label: '复制邮箱', ic: 'copy', run: () => copyText(mail) },
  ];
  const menu = document.createElement('div');
  menu.className = 'rowmenu';
  menu.setAttribute('role', 'menu');
  for (const it of items) {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'rowmenu-item';
    b.setAttribute('role', 'menuitem');
    b.append(icon(it.ic, 'ic ic-sm'), document.createTextNode(it.label));
    b.addEventListener('click', () => {
      menu.remove();
      it.run();
    });
    menu.appendChild(b);
  }
  document.body.appendChild(menu);
  const r = menu.getBoundingClientRect();
  menu.style.left = Math.max(8, Math.min(ev.clientX, window.innerWidth - r.width - 8)) + 'px';
  menu.style.top = Math.max(8, Math.min(ev.clientY, window.innerHeight - r.height - 8)) + 'px';
  const away = () => {
    menu.remove();
    document.removeEventListener('click', away, true);
    document.removeEventListener('keydown', away, true);
  };
  setTimeout(() => {
    document.addEventListener('click', away, true);
    document.addEventListener('keydown', away, true);
  }, 0);
});

// copyText 把一段字抄走，抄不成也说一声。
function copyText(text) {
  if (!text) return;
  navigator.clipboard.writeText(text).then(
    () => say('抄好了'),
    () => say('抄不了：' + text)
  );
}
