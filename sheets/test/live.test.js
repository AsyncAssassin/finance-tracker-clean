// Code.gs against a running stack: the same script, with UrlFetchApp made of
// curl, registers a user, sends rows, loads the report and the budgets.
// Runs only with E2E_BASE_URL, like the Go end-to-end test:
//
//   E2E_BASE_URL=http://localhost:8080 node --test 'sheets/test/*.test.js'
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { execFileSync } = require('node:child_process');
const { environment, fakeSheet } = require('./fakes.js');

const BASE = process.env.E2E_BASE_URL;

// curlFetch does what UrlFetchApp.fetch does with muteHttpExceptions: a
// synchronous request that returns any status.
function curlFetch(url, params) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'gs-'));
  const args = ['-s', '-X', params.method.toUpperCase(), '-D', path.join(dir, 'h'), '-o', path.join(dir, 'b'), '-w', '%{http_code}'];
  for (const [k, v] of Object.entries(params.headers || {})) args.push('-H', k + ': ' + v);
  if (params.payload !== undefined) {
    fs.writeFileSync(path.join(dir, 'p'), params.payload);
    args.push('-H', 'Content-Type: ' + params.contentType, '--data-binary', '@' + path.join(dir, 'p'));
  }
  args.push(url);
  const code = Number(execFileSync('curl', args, { encoding: 'utf8' }));
  const headers = {};
  for (const line of fs.readFileSync(path.join(dir, 'h'), 'utf8').split('\r\n')) {
    const i = line.indexOf(':');
    if (i > 0) headers[line.slice(0, i)] = line.slice(i + 1).trim();
  }
  const body = fs.readFileSync(path.join(dir, 'b'), 'utf8');
  fs.rmSync(dir, { recursive: true });
  return { code, body, headers };
}

test('the sheet works against the running API', { skip: !BASE && 'E2E_BASE_URL is not set' }, () => {
  const tx = fakeSheet('Транзакции', [
    ['Дата', 'Категория', 'Сумма', 'Описание', 'ID', 'Статус', 'Ключ'],
    ['2026-10-01', 'еда', 1250.5, 'продукты'],
    ['02.10.2026', 'кафе', '1 800,00', 'обед'],
    ['2026-10-03', 'кафе', 1500, 'ужин'],
    ['2026-10-03', 'транспорт', 'сто', 'такси'],
  ]);
  const budgets = fakeSheet('Бюджеты', [['Категория', 'Лимит', 'Период', 'Статус'], ['кафе', 3000, 'month', '']]);
  const report = fakeSheet('Отчёт', [['С', '2026-10-01'], ['По', '2026-10-31']]);
  const env = environment({
    sheets: [tx, budgets, report],
    docProps: { apiBaseUrl: BASE.replace(/\/+$/, '') },
    userProps: {},
    respond: curlFetch,
  });
  const gs = env.gs;

  // Real Apps Script requires public HTTPS. Only the local Node harness
  // substitutes origin validation for its loopback-only HTTP transport.
  const origin = new URL(BASE).origin;
  const productionNormalize = gs.normalizeBaseUrl_;
  gs.normalizeBaseUrl_ = (value) => {
    if (new URL(value).origin === origin && ['localhost', '127.0.0.1', '[::1]'].includes(new URL(value).hostname)) return origin;
    return productionNormalize(value);
  };

  const email = process.env.E2E_SHEETS_EMAIL || ('sheet-' + Date.now() + '@example.com');
  assert.match(gs.login(email, 'зелёный трамвай едет', true, origin).message, /Аккаунт создан/);
  const duplicate = gs.login(email, 'зелёный трамвай едет', true, origin);
  assert.equal(duplicate.ok, false);
  assert.match(duplicate.message, /уже есть/);
  assert.match(gs.login(email, 'зелёный трамвай едет', false, origin).message, /Вы вошли/);

  gs.saveBudgets_();
  assert.equal(budgets.getRange('D2').getValue(), 'создан');

  gs.sendNewTransactions_();
  const sent = tx.getRange(2, 5, 4, 2).getValues();
  assert.equal(sent[0][1], 'сохранена');
  assert.equal(sent[1][1], 'сохранена');
  assert.match(sent[2][1], /^ошибка: Превышен бюджет категории «кафе»/);
  assert.match(sent[3][1], /^ошибка: Проверьте поля: amount/);
  assert.ok(sent[0][0] > 0 && sent[1][0] > 0);

  // Sending again changes nothing: the rows with an ID are skipped and the
  // failed ones fail the same way.
  gs.sendNewTransactions_();
  assert.deepEqual(tx.getRange(2, 5, 2, 1).getValues(), [[sent[0][0]], [sent[1][0]]]);

  const added = gs.addTransaction({ date: '2026-10-05', category: 'здоровье', amount: '700', description: 'лекарства' });
  assert.equal(added.ok, true);
  assert.match(added.message, /Сохранена транзакция №\d+/);

  gs.loadReport_();
  const table = report.getRange(4, 1, 5, 5).getValues();
  assert.deepEqual(table[0], ['Категория', 'Потрачено', 'Лимит за период', 'Остаток за диапазон', '% бюджета']);
  assert.equal(report.getRange('A3').getValue(), 'Остаток относится к датам отчёта. Следующий расход проверяется по периоду бюджета на его дату.');
  assert.deepEqual(table[4], ['Итого', 3750.5, '', '', '']);
  gs.loadReport_();
  assert.match(report.getRange('D2').getValue(), /^Кеш отчёта: (HIT|выключен)$/);

  gs.loadBudgets_();
  assert.deepEqual(budgets.getRange(2, 1, 1, 3).getValues(), [['кафе', 3000, 'month']]);

  // Literal apostrophes must survive the script -> real CSV parser path.
  tx.appendRow(gs.literalRows_([['2026-10-06', "'=category", 1, "''description", '', '', "'key"]])[0]);
  gs.sendNewTransactions_();
  const items = gs.check_(gs.request_('get', '/api/transactions'), [200]).body.items;
  const imported = items.find((item) => item.external_id === "'key");
  assert.ok(imported, 'external_id was changed by CSV unescaping');
  assert.equal(imported.category, "'=category");
  assert.equal(imported.description, "''description");
  gs.loadReport_();
  assert.deepEqual(report.formulaWrites, []);
});
