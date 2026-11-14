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
    const name = egress.dataset.egress;
    egress.disabled = true;
    egress.textContent = '测试中……';
    try {
      const out = await post('/api/egress/test', { name });
      egress.textContent = out.ok ? '可用' : '不可用';
      egress.className = out.ok ? 'good' : 'bad';
    } catch (e) {
      egress.textContent = '失败';
      egress.className = 'bad';
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
