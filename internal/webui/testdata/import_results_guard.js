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
    tbody: {
      textContent: '',
      appendChild: function () { table.rows += 1; },
    },
    querySelector: function () { return table.tbody; },
  };
  return {
    table: table,
    document: {
      addEventListener: function () {},
      getElementById: function (id) {
        return id === 'import-results' ? table : null;
      },
      createElement: function () {
        return { textContent: '', appendChild: function () {} };
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

console.log('ok');
