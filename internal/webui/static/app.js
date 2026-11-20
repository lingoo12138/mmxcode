// mmxcode 的 WebUI 脚本。
//
// 做几件事：改账号的出口、触发签到、测出口、登录。
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

async function post(url, body) {
  const res = await fetch(url, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const answer = await readAnswer(res);
  if (answer.status === 401) askForKey();
  if (!answer.ok) throw new Error(messageOf(answer));
  return answer.json;
}

async function patch(url, body) {
  const res = await fetch(url, {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  const answer = await readAnswer(res);
  if (answer.status === 401) askForKey();
  if (!answer.ok) throw new Error(messageOf(answer));
}

// del 删掉一个东西。
async function del(url) {
  const res = await fetch(url, { method: 'DELETE' });
  const answer = await readAnswer(res);
  if (answer.status === 401) askForKey();
  if (!answer.ok) throw new Error(messageOf(answer));
}

// signIn 拿一张票。
//
// 回答可能是登录页（HTML），也可能是 JSON：网关现在给的是前者。两样
// 都读，人看到的是同一句话。
async function signIn(key) {
  const res = await fetch('/api/session', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ key: key }),
  });
  const answer = await readAnswer(res);
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
  const label = document.createElement('p');
  label.textContent = '这个网关要一把 API key。填进去就能继续。';
  const form = document.createElement('form');
  const input = document.createElement('input');
  input.type = 'password';
  input.placeholder = 'API key';
  input.autocomplete = 'current-password';
  const button = document.createElement('button');
  button.type = 'submit';
  button.textContent = '进入';
  const msg = document.createElement('p');
  form.append(input, button);
  keyBox.append(label, form, msg);
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
  document.body.appendChild(keyBox);
}

document.addEventListener('click', async (ev) => {
  const checkin = ev.target.closest('[data-checkin]');
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

  const egress = ev.target.closest('[data-egress]');
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

  const groupEdit = ev.target.closest('[data-group-edit]');
  if (groupEdit) {
    editGroup(groupEdit.dataset.groupEdit, groupEdit.dataset.groupUrls || '');
    return;
  }

  const groupRm = ev.target.closest('[data-group-rm]');
  if (groupRm) {
    const name = groupRm.dataset.groupRm;
    if (!confirm(`删掉组 ${name}？指着它的账号回到全局组。`)) return;
    groupRm.disabled = true;
    try {
      await del(`/api/proxy-groups?name=${encodeURIComponent(name)}`);
      location.reload();
    } catch (e) {
      document.getElementById('status').textContent = '删除失败：' + e.message;
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
    document.getElementById('status').textContent = '已保存';
  } catch (e) {
    document.getElementById('status').textContent = '保存失败：' + e.message;
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
    const res = await fetch('/api/relogin', { method: 'POST' });
    const answer = await readAnswer(res);
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
    const res = await fetch('/api/usage/refresh', { method: 'POST' });
    const answer = await readAnswer(res);
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
  const revoke = ev.target.closest('[data-revoke]');
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
  const res = await fetch('/api/keys');
  const answer = await readAnswer(res);
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
// 备路。
function editGroup(name, urls) {
  const box = document.createElement('div');
  box.className = 'card';
  const label = document.createElement('p');
  label.textContent = `组 ${name} 的地址（一行一个，第一个是主路）`;
  const input = document.createElement('textarea');
  input.rows = 6;
  input.cols = 48;
  input.value = urls;
  const button = document.createElement('button');
  button.textContent = '存回去';
  const msg = document.createElement('p');
  box.append(label, input, button, msg);
  button.addEventListener('click', async () => {
    button.disabled = true;
    try {
      const list = input.value.split('\n').map((s) => s.trim()).filter(Boolean);
      await post('/api/proxy-groups', { name, urls: list });
      location.reload();
    } catch (e) {
      msg.textContent = e.message;
      button.disabled = false;
    }
  });
  document.body.appendChild(box);
}
