// The page's script must not throw on a run whose results are not an
// array.
//
// 跑法：node testdata/import_results_guard.js
//
// 一整趟导入失败的时候（比如密钥配错了），存下来的结果以前是 "null"。
// 页面上那个脚本 `JSON.parse(run.results || '[]')` 解析 "null" 是成的，
// 出来的是 null，接着 `for (const r of results)` 就炸了：页面上写着
// 「失败」，结果表也不出来。所以脚本自己也要守着这一头。
//
// 这个脚本给 app.js 一个最小的 document，然后拿几种存下来的结果去调
// showImportResults：哪一种都不许炸，也不许让结果表藏着。
const fs = require('fs');
const path = require('path');
const vm = require('vm');

function makePage() {
  const table = {
    hidden: true,
    rows: 0,
    cells: [],
    tbody: {
      textContent: '',
      appendChild: function (tr) {
        table.rows += 1;
        table.cells.push(tr.children.map(function (td) { return td.textContent; }));
      },
    },
    querySelector: function () { return table.tbody; },
  };
  const status = { textContent: '' };
  return {
    table: table,
    status: status,
    document: {
      addEventListener: function () {},
      getElementById: function (id) {
        if (id === 'import-results') return table;
        if (id === 'import-status') return status;
        return null;
      },
      createElement: function () {
        return {
          textContent: '',
          children: [],
          appendChild: function (c) { this.children.push(c); },
        };
      },
      querySelector: function () { return null; },
    },
  };
}

const page = makePage();
const source = fs.readFileSync(path.join(__dirname, '..', 'static', 'app.js'), 'utf8');
const context = {
  document: page.document,
  window: {},
  console: console,
  fetch: function () { return Promise.reject(new Error('no network in this test')); },
  setInterval: function () { return 0; },
  clearInterval: function () {},
};
vm.createContext(context);
vm.runInContext(source, context);

// 存下来的结果有三种：老代码的 "null"、新的 "[]"、还有空的那一种。
// 三种都得走得下去。
for (const stored of ['null', '[]', '']) {
  page.table.hidden = true;
  page.table.rows = 0;
  try {
    context.showImportResults({ results: stored });
  } catch (e) {
    console.error('showImportResults threw for ' + JSON.stringify(stored) + ': ' + e.message);
    process.exit(1);
  }
  if (page.table.hidden) {
    console.error('the results table stayed hidden for ' + JSON.stringify(stored));
    process.exit(1);
  }
}

// 整趟没成的那一行：它不是账号，但它得把整趟为什么没成说出来。
// 说了的那句话要落在页面上那一句「成功几个失败几个」的后面。
page.status.textContent = '';
context.showImportResults({
  ok: 0,
  failed: 0,
  results: '[{"email":"（整趟）","ok":false,"whole":true,"message":"整趟导入没能完成：the key is not right"}]',
});
if (!page.status.textContent.includes('the key is not right')) {
  console.error('the page does not say why the run failed: ' + page.status.textContent);
  process.exit(1);
}
if (!page.status.textContent.includes('失败 0')) {
  console.error('the page counts a whole-run failure as an account: ' + page.status.textContent);
  process.exit(1);
}

// 表里那一行写的是「整趟」，不是「失败」：上面那句说的是「失败 0」，
// 表里再有一个「失败」，两处对不上，人不知道该信哪个。
const wholeRow = page.table.cells[page.table.cells.length - 1];
if (!wholeRow || wholeRow[1] !== '整趟') {
  console.error('the whole-run row is labelled ' + JSON.stringify(wholeRow && wholeRow[1]) +
    ', want 整趟');
  process.exit(1);
}

// 账号那一行还是「失败」：整趟和账号得分开。
page.table.cells = [];
context.showImportResults({
  ok: 1,
  failed: 1,
  results: '[{"email":"a@example.com","ok":false,"message":"the password is wrong"}]',
});
const accountRow = page.table.cells[page.table.cells.length - 1];
if (!accountRow || accountRow[1] !== '失败') {
  console.error('the account row is labelled ' + JSON.stringify(accountRow && accountRow[1]) +
    ', want 失败');
  process.exit(1);
}

console.log('ok');