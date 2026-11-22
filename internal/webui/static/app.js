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
// 页面顶上那道细线就是它：只要还有一个请求在路上，它就亮着。
let inFlight = 0;

function setBusy(on) {
  inFlight += on ? 1 : -1;
  if (inFlight < 0) inFlight = 0;
  document.body.classList.toggle('is-busy', inFlight > 0);
}

// call asks the gateway something.
//
// 页面上的按钮都在等这个回答，所以从发出到答回来，页顶那道线亮着。
async function call(url, options) {
  setBusy(true);
  try {
    const res = await fetch(url, options);
    return await readAnswer(res);
  } finally {
    setBusy(false);
  }
}

async function post(url, body) {
  const answer = await call(url, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (answer.status === 401) askForKey();
  if (!answer.ok) throw new Error(messageOf(answer));
  return answer.json;
}

async function patch(url, body) {
  const answer = await call(url, {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  if (answer.status === 401) askForKey();
  if (!answer.ok) throw new Error(messageOf(answer));
}

// del 删掉一个东西。
async function del(url) {
  const answer = await call(url, { method: 'DELETE' });
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
  keyBox = document.createElement('div');
  keyBox.className = 'card';
  const panel = document.createElement('div');
  panel.className = 'card-box';
  const title = document.createElement('h3');
  title.textContent = '这个网关要一把 API key';
  const label = document.createElement('p');
  label.textContent = '填进去就能继续。';
  const form = document.createElement('form');
  const input = document.createElement('input');
  input.type = 'password';
  input.placeholder = 'API key';
  input.autocomplete = 'current-password';
  const button = document.createElement('button');
  button.type = 'submit';
  button.textContent = '进入';
  const msg = document.createElement('p');
  msg.className = 'card-msg';
  form.append(input, button);
  panel.append(title, label, form, msg);
  keyBox.appendChild(panel);
  form.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    button.disabled = true;
    try {
      await signIn(input.value);
      location.reload();
    } catch (e) {
      msg.textContent = e.message;
      button.disabled = false;
    }
  });
  keyBox.addEventListener('click', (ev) => {
    if (ev.target === keyBox) closeCard(keyBox);
  });
  document.body.appendChild(keyBox);
  input.focus();
}

// closeCard 关掉一个飘出来的框。
function closeCard(box) {
  if (!box) return;
  box.remove();
  if (box === keyBox) keyBox = null;
}

// Esc 关掉最上面那个框。
document.addEventListener('keydown', (ev) => {
  if (ev.key !== 'Escape') return;
  const drawer = document.querySelector('.drawer');
  if (drawer) {
    closeDrawer();
    return;
  }
  const boxes = document.querySelectorAll('.card');
  if (!boxes.length) return;
  closeCard(boxes[boxes.length - 1]);
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
    button.title = '主题：' + names[current] + '（按一下换一个）';
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
  let want = '';
  const apply = () => {
    const q = box ? box.value.trim().toLowerCase() : '';
    let shown = 0;
    for (const row of rows) {
      const hitText = !q || row.textContent.toLowerCase().indexOf(q) >= 0;
      const status = row.dataset.status;
      const hitStatus = !want ||
        (want === 'error' ? status !== 'active' && status !== 'signed_out' : status === want);
      const hit = hitText && hitStatus;
      row.classList.toggle('is-hidden', !hit);
      if (hit) shown++;
    }
    if (counter) counter.textContent = rows.length ? shown + ' / ' + rows.length : '';
  };
  if (box) box.addEventListener('input', apply);
  if (chips) {
    chips.addEventListener('click', (ev) => {
      const chip = ev.target.closest('.chip');
      if (!chip) return;
      want = chip.dataset.status || '';
      for (const c of chips.querySelectorAll('.chip')) c.classList.toggle('is-on', c === chip);
      apply();
    });
  }
  apply();
})();

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
    th.addEventListener('click', () => {
      const col = Number(th.dataset.col);
      // 再点一次是反过来。
      const down = th.dataset.sort !== 'desc';
      for (const h of heads) delete h.dataset.sort;
      th.dataset.sort = down ? 'asc' : 'desc';
      const rows = Array.from(body.querySelectorAll('tr[data-row]'));
      rows.sort((a, b) => compare(cellValue(a, col), cellValue(b, col)) * (down ? 1 : -1));
      for (const row of rows) body.appendChild(row);
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
  const boxes = () => Array.from(document.querySelectorAll('.pick-row'));
  const paint = () => {
    const picked = boxes().filter((b) => b.checked);
    if (count) count.textContent = String(picked.length);
    bar.hidden = picked.length === 0;
    all.checked = picked.length > 0 && picked.length === boxes().length;
    for (const b of boxes()) {
      const row = b.closest('tr');
      if (row) row.classList.toggle('is-picked', b.checked);
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
      let ok = 0;
      const bad = [];
      for (const id of ids) {
        try {
          await post('/api/accounts/' + encodeURIComponent(id) + '/checkin');
          ok++;
        } catch (e) {
          bad.push(id);
        }
        say('签到 ' + (ok + bad.length) + ' / ' + ids.length);
      }
      checkin.disabled = false;
      say('签了 ' + ok + ' 个，失败 ' + bad.length + ' 个');
      location.reload();
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
    remove.addEventListener('click', async () => {
      const ids = pickedIDs();
      if (!ids.length) return;
      if (!confirm('删掉选中的 ' + ids.length + ' 个账号？它们的令牌和读数也跟着没了。')) return;
      remove.disabled = true;
      let ok = 0;
      for (const id of ids) {
        try {
          await del('/api/accounts/' + encodeURIComponent(id));
          ok++;
        } catch (e) {
          say('删除失败：' + e.message);
        }
      }
      say('删了 ' + ok + ' 个');
      location.reload();
    });
  }
})();

// askGroup 让人选一个组，给选中的账号换上。
//
// 组的名字从页面上那一列下拉框里拿：那一列列的就是库里所有的组。
function askGroup(ids) {
  const pick = document.querySelector('select[data-field="proxy_group"]');
  const names = [];
  if (pick) {
    for (const opt of pick.options) {
      if (opt.value) names.push(opt.value);
    }
  }
  const box = document.createElement('div');
  box.className = 'card';
  const panel = document.createElement('div');
  panel.className = 'card-box';
  const title = document.createElement('h3');
  title.textContent = '把选中的 ' + ids.length + ' 个账号放进哪个组';
  const label = document.createElement('p');
  label.textContent = '选「跟随全局」就是回到全局默认。';
  const form = document.createElement('form');
  const select = document.createElement('select');
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
  const button = document.createElement('button');
  button.type = 'submit';
  button.textContent = '换过去';
  const msg = document.createElement('p');
  msg.className = 'card-msg';
  form.append(select, button);
  panel.append(title, label, form, msg);
  box.appendChild(panel);
  form.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    button.disabled = true;
    let ok = 0;
    for (const id of ids) {
      try {
        await patch('/api/accounts/' + encodeURIComponent(id), { proxy_group: select.value });
        ok++;
      } catch (e) {
        msg.textContent = e.message;
      }
    }
    closeCard(box);
    say('换了 ' + ok + ' 个');
    location.reload();
  });
  box.addEventListener('click', (ev) => {
    if (ev.target === box) closeCard(box);
  });
  document.body.appendChild(box);
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

function closeDrawer() {
  if (!drawerBox) return;
  drawerBox.remove();
  const scrim = document.querySelector('.drawer-scrim');
  if (scrim) scrim.remove();
  drawerBox = null;
}

// openDrawer 摆出那个号的详情。
function openDrawer(row) {
  closeDrawer();
  const d = row.dataset;
  const scrim = document.createElement('div');
  scrim.className = 'drawer-scrim';
  scrim.addEventListener('click', closeDrawer);

  const box = document.createElement('aside');
  box.className = 'drawer';
  box.setAttribute('role', 'dialog');

  const head = document.createElement('div');
  head.className = 'drawer-head';
  const title = document.createElement('h3');
  title.textContent = d.name || d.email || d.row;
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

  const list = document.createElement('dl');
  list.className = 'kv';
  const statusText = d.status === 'active' ? '正常'
    : (d.status === 'signed_out' ? '已登出' : d.status);
  const left = d.hasAmounts === 'true'
    ? (Number(d.remaining).toFixed(2) + ' / ' + Number(d.granted).toFixed(2))
    : '读不到';
  const outlet = d.group ? ('组 ' + d.group) : (d.url || '跟随全局');
  const checked = d.checked === 'true' ? ('已签 ' + d.streak + ' 天') : '未签';
  for (const pair of [
    ['账号 ID', d.row],
    ['邮箱', d.email || '没有'],
    ['状态', statusText],
    ['积分', left],
    ['套餐', d.plan || '不知道'],
    ['额度窗口', d.window || '没有读到'],
    ['出口', outlet],
    ['今日签到', checked],
  ]) {
    const dt = document.createElement('dt');
    dt.textContent = pair[0];
    const dd = document.createElement('dd');
    dd.textContent = pair[1];
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
  const nameInput = document.createElement('input');
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
      say('已保存');
      closeDrawer();
      location.reload();
    } catch (e) {
      say('保存失败：' + e.message);
      save.disabled = false;
    }
  });
  const nameRow = document.createElement('div');
  nameRow.className = 'row';
  nameRow.append(nameInput, save);
  field.append(nameLabel, nameRow);
  body.appendChild(field);

  const foot = document.createElement('div');
  foot.className = 'drawer-foot';
  const checkin = document.createElement('button');
  checkin.type = 'button';
  checkin.className = 'primary icon';
  checkin.append(icon('check'), document.createTextNode('立即签到'));
  checkin.addEventListener('click', async () => {
    checkin.disabled = true;
    try {
      const out = await post('/api/accounts/' + encodeURIComponent(d.row) + '/checkin');
      say(out.outcome === 'claimed' ? ('领到 ' + out.credit) : out.outcome);
      location.reload();
    } catch (e) {
      say('失败：' + e.message);
      checkin.disabled = false;
    }
  });
  const remove = document.createElement('button');
  remove.type = 'button';
  remove.className = 'danger icon';
  remove.append(icon('trash'), document.createTextNode('删掉'));
  remove.addEventListener('click', async () => {
    if (!confirm('删掉这个账号？它的令牌和读数也跟着没了。')) return;
    try {
      await del('/api/accounts/' + encodeURIComponent(d.row));
      row.remove();
      closeDrawer();
      say('删掉了');
      location.reload();
    } catch (e) {
      say('删除失败：' + e.message);
    }
  });
  foot.append(checkin, remove);

  box.append(head, body, foot);
  document.body.appendChild(scrim);
  document.body.appendChild(box);
  drawerBox = box;
}

document.addEventListener('click', async (ev) => {
  const checkin = ev.target.closest('button[data-checkin]');
  if (checkin) {
    const id = checkin.dataset.checkin;
    checkin.disabled = true;
    checkin.textContent = '签到中……';
    try {
      const out = await post(`/api/accounts/${id}/checkin`);
      checkin.textContent = out.outcome === 'claimed' ? `领到 ${out.credit}` : out.outcome;
    } catch (e) {
      checkin.textContent = '失败';
      checkin.title = String(e.message);
    }
    return;
  }

  const egress = ev.target.closest('button[data-egress]');
  if (egress) {
    // 页面上显示的是去掉凭据的地址，服务器拿它去对上真的那个地址。
    const url = egress.dataset.egress;
    egress.disabled = true;
    egress.textContent = '测试中……';
    try {
      const out = await post('/api/egress/test', { url });
      egress.textContent = out.ok ? '可用' : '不可用';
      egress.className = out.ok ? 'good' : 'bad';
    } catch (e) {
      egress.textContent = '失败';
      egress.className = 'bad';
    }
    return;
  }

  const groupEdit = ev.target.closest('button[data-group-edit]');
  if (groupEdit) {
    editGroup(groupEdit.dataset.groupEdit, groupEdit.dataset.groupUrls || '');
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
    if (!confirm(`删掉组 ${name}？指着它的账号回到全局组。`)) return;
    groupRm.disabled = true;
    try {
      await del(`/api/proxy-groups?name=${encodeURIComponent(name)}`);
      location.reload();
    } catch (e) {
      say('删除失败：' + e.message);
      groupRm.disabled = false;
    }
    return;
  }

  if (ev.target.id === 'start-login') {
    const box = document.getElementById('login-result');
    box.textContent = '请稍候……';
    try {
      const out = await post('/api/accounts');
      // 验证码和地址是上游给的：它们只作为字和地址进页面，不作为
      // 标记。地址还得是 http 或者 https 才配当链接。
      box.textContent = '';
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
      box.textContent = '失败：' + e.message;
    }
  }
});

document.addEventListener('change', async (ev) => {
  const sel = ev.target.closest('[data-field]');
  if (!sel) return;
  const body = {};
  body[sel.dataset.field] = sel.value;
  // 地址和组是二选一：选了组，地址框就清掉；填了地址，下拉框就回到空。
  if (sel.dataset.field === 'proxy_group' && sel.value) {
    const input = document.querySelector(`input[data-account="${sel.dataset.account}"][data-field="proxy_url"]`);
    if (input) input.value = '';
  }
  if (sel.dataset.field === 'proxy_url' && sel.value) {
    const pick = document.querySelector(`select[data-account="${sel.dataset.account}"][data-field="proxy_group"]`);
    if (pick) pick.value = '';
  }
  try {
    await patch(`/api/accounts/${sel.dataset.account}`, body);
    say('已保存');
  } catch (e) {
    say('保存失败：' + e.message);
  }
});

function poll(id) {
  const timer = setInterval(async () => {
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
  if (!input || !input.files || !input.files[0]) return;
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
    const answer = await call('/api/import', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ list: list }),
    });
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

function watchImport(id) {
  const timer = setInterval(async () => {
    try {
      const answer = await call('/api/import/' + encodeURIComponent(id));
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
      status.textContent = '成功 ' + (run.ok || 0) + '，失败 ' + (run.failed || 0) +
        '。' + (whole.message || '整趟导入没能完成');
    }
  }
}

// 重登：把签出的账号重新登一遍。
document.addEventListener('click', async (ev) => {
  const button = ev.target.closest('#relogin-all');
  if (!button) return;
  const status = document.getElementById('relogin-status');
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
  if (status) status.textContent = '正在申请……';
  try {
    const out = await post('/api/keys', { name: name ? name.value : '' });
    const plain = document.getElementById('key-plain');
    if (plain) plain.textContent = out.key;
    const box = document.getElementById('key-new');
    if (box) box.hidden = false;
    if (status) status.textContent = '申请好了。抄走收好：过了这一会儿就拿不出来了。';
    if (name) name.value = '';
    await renderKeys();
  } catch (e) {
    if (status) status.textContent = '失败：' + e.message;
  }
});

document.addEventListener('click', async (ev) => {
  const copy = ev.target.closest('#key-copy');
  if (copy) {
    const plain = document.getElementById('key-plain');
    if (!plain) return;
    try {
      await navigator.clipboard.writeText(plain.textContent);
      copy.textContent = '复制好了';
    } catch (e) {
      // 剪贴板不给用的时候，人自己选中那段字也一样。
      copy.textContent = '选中最下面那段字自己复制';
    }
    return;
  }
  const revoke = ev.target.closest('button[data-revoke]');
  if (revoke) {
    revoke.disabled = true;
    try {
      await del('/api/keys/' + encodeURIComponent(revoke.dataset.revoke));
      await renderKeys();
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
      button.textContent = '撤销';
      button.dataset.revoke = k.id;
      last.appendChild(button);
    }
    tr.appendChild(last);
    body.appendChild(tr);
  }
}

// editGroup 改一个组。
//
// 组里的地址一行一个，顺序就是故障转移的顺序：第一个是主路，后面的都是
// 备路。名字空着就是新建一个。
function editGroup(name, urls) {
  const box = document.createElement('div');
  box.className = 'card';
  const panel = document.createElement('div');
  panel.className = 'card-box';
  const title = document.createElement('h3');
  title.textContent = name ? ('组 ' + name + ' 的地址') : '新建一个组';
  const label = document.createElement('p');
  label.textContent = '一行一个，第一个是主路，后面的都是备路。';
  const form = document.createElement('form');
  const nameInput = document.createElement('input');
  nameInput.placeholder = '组的名字';
  nameInput.value = name;
  const input = document.createElement('textarea');
  input.rows = 6;
  input.value = urls;
  const button = document.createElement('button');
  button.type = 'submit';
  button.textContent = '存回去';
  const msg = document.createElement('p');
  msg.className = 'card-msg';
  form.append(nameInput, input, button);
  panel.append(title, label, form, msg);
  box.appendChild(panel);
  form.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    button.disabled = true;
    const groupName = nameInput.value.trim();
    if (!groupName) {
      msg.textContent = '这个组还没有名字。';
      button.disabled = false;
      return;
    }
    try {
      const list = input.value.split('\n').map((s) => s.trim()).filter(Boolean);
      await post('/api/proxy-groups', { name: groupName, urls: list });
      location.reload();
    } catch (e) {
      msg.textContent = e.message;
      button.disabled = false;
    }
  });
  box.addEventListener('click', (ev) => {
    if (ev.target === box) closeCard(box);
  });
  document.body.appendChild(box);
  (name ? input : nameInput).focus();
}
