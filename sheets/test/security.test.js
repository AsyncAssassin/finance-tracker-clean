'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const { environment, fakeSheet } = require('./fakes');
const BASE = 'https://api.example';
const TX = ['Дата', 'Категория', 'Сумма', 'Описание', 'ID', 'Статус', 'Ключ'];

test('changing the API address never sends the old bearer token', () => {
  const env = environment({ respond: () => ({ code: 200, body: {} }) });
  env.gs.PropertiesService.docs.setProperty('apiBaseUrl', 'https://other.example');
  assert.throws(() => env.gs.request_('get', '/api/budget'), /войдите/);
  assert.equal(env.requests.length, 0);
  assert.equal(env.gs.PropertiesService.user.getProperty('token'), null);
});

test('legacy sessions and missing or invalid expiry require login', () => {
  for (const extra of [{}, { tokenBaseUrl: BASE }, { tokenBaseUrl: BASE, expiresAt: 'invalid' }]) {
    const env = environment({ userProps: { token: 'old', ...extra } });
    assert.throws(() => env.gs.request_('get', '/api/budget'), /войдите/);
    assert.equal(env.requests.length, 0);
  }
});

test('only an HTTPS origin is accepted, without credentials or URL suffixes', () => {
  const { gs } = environment();
  assert.equal(gs.normalizeBaseUrl_(' HTTPS://API.Example:443/ '), BASE);
  for (const url of ['http://api.example', 'https://user:pass@api.example', 'https://api.example?q=x',
    'https://api.example#x', 'https://api.example/path', 'https://api.example\\@other.example',
    'https://api.example:8080', 'https://-bad.example', 'https://api..example']) {
    assert.throws(() => gs.normalizeBaseUrl_(url), /https/);
  }
});

test('login checks the server shown in the form and binds the response to that server', () => {
  let env;
  env = environment({ userProps: {}, respond: () => {
    env.gs.PropertiesService.docs.setProperty('apiBaseUrl', 'https://other.example');
    return { code: 201, body: { access_token: 'jwt', expires_at: '2999-01-01T00:00:00Z', user: { email: 'test@example.com' } } };
  } });
  const result = env.gs.login('test@example.com', 'test password phrase', true, 'https://old.example');
  assert.equal(result.ok, false);
  assert.match(result.message, /изменился/);
  assert.equal(env.requests.length, 0);
  env.gs.login('test@example.com', 'test password phrase', true, BASE);
  assert.equal(env.requests[0].url, BASE + '/api/auth/register');
  assert.equal(env.requests[0].params.followRedirects, false);
  assert.equal(env.gs.PropertiesService.user.getProperty('tokenBaseUrl'), BASE);
  assert.throws(() => env.gs.request_('get', '/api/budget'), /войдите/);
  assert.equal(env.requests.length, 1);
});

test('formulas from API responses remain literal text in transactions, budgets and reports', () => {
  const tx = fakeSheet('Транзакции', [TX]);
  const budget = fakeSheet('Бюджеты');
  const report = fakeSheet('Отчёт', [['С', '2026-10-01'], ['По', '2026-10-31']]);
  const env = environment({ sheets: [tx, budget, report], respond: (url) => {
    if (url.endsWith('/api/transactions')) return { code: 201, body: { id: 1, date: '2026-10-05', category: '=1+1', amount: 10, description: "'=2+2" } };
    if (url.endsWith('/api/budget')) return { code: 200, body: { items: [{ category: '=1+1', limit: 50, period: 'month' }] } };
    return { code: 200, body: { total: 10, categories: [{ category: '=1+1', spent: 10 }] } };
  } });
  env.gs.addTransaction({ category: '=1+1', amount: '10', key: '=key' });
  env.gs.loadBudgets_();
  env.gs.loadReport_();
  for (const sheet of [tx, budget, report]) assert.deepEqual(sheet.formulaWrites, []);
  assert.equal(tx.getRange('B2').getValue(), '=1+1');
  assert.equal(tx.getRange('D2').getValue(), "'=2+2");
  assert.equal(tx.getRange('G2').getValue(), '=key');
  assert.equal(budget.getRange('A2').getValue(), '=1+1');
  assert.equal(report.getRange('A5').getValue(), '=1+1');
});

test('CSV escapes literal apostrophes according to the API contract', () => {
  const { gs } = environment();
  const csv = gs.buildImportCsv_([{ date: '2026-10-05', category: "'=cat", amount: '10', description: "''text", key: "'key" }]);
  assert.equal(csv, "date,category,amount,description,external_id\n2026-10-05,''=cat,10,'''text,''key\n");
});

test('prototype-like keys survive retries and keys are flushed before the request', () => {
  const sheet = fakeSheet('Транзакции', [TX, ['2026-10-05', 'еда', 1, '', '', '', '__proto__']]);
  let env;
  env = environment({ sheets: [sheet], respond: (url, params) => {
    assert.equal(env.state.flushes, 1);
    assert.match(params.payload, /,__proto__\n$/);
    return { code: 200, body: { created: 1, failed: 0, rows: [{ line: 2, status: 'created', id: 7 }] } };
  } });
  env.gs.sendNewTransactions_();
  assert.equal(sheet.getRange('E2').getValue(), 7);
  assert.equal(sheet.getRange('G2').getValue(), '__proto__');
});

test('date cells cannot introduce extra query parameters', () => {
  const report = fakeSheet('Отчёт', [['С', '2026-10-01&to=2000-01-01'], ['По', '2026-10-31']]);
  const env = environment({ sheets: [report], respond: () => ({ code: 200, body: { total: 0, categories: [] } }) });
  env.gs.loadReport_();
  const url = new URL(env.requests[0].url);
  assert.deepEqual(url.searchParams.getAll('to'), ['2026-10-31']);
  assert.equal(url.searchParams.get('from'), '2026-10-01&to=2000-01-01');
});

test('business failures return values from public forms without throwing financial details', () => {
  const marker = 'private-financial-marker';
  const env = environment({ respond: () => ({ code: 409, body: { error: {
    code: 'budget_exceeded',
    details: { category: marker, period_label: '2026-10', spent: 12, limit: 10, remaining: -2 },
  } } }) });
  const result = env.gs.addTransaction({ category: marker, amount: '1', key: 'retry-key' });
  assert.equal(result.ok, false);
  assert.match(result.message, /private-financial-marker/);
  const login = environment({ userProps: {}, respond: () => ({ code: 403, body: { error: {
    code: 'registration_closed', message: 'registration is closed',
  } } }) });
  assert.equal(login.gs.login('test@example.com', 'valid passphrase here', true, BASE).ok, false);
});

test('unexpected form failures expose only fixed text', () => {
  const marker = 'private-transport-marker';
  for (const respond of [
    () => { throw new Error('https://api.example?secret=' + marker); },
    () => ({ code: 500, body: { error: { message: marker } } }),
    () => ({ code: 502, text: marker }),
  ]) {
    const env = environment({ respond });
    assert.throws(() => env.gs.addTransaction({ category: 'food', amount: '1', key: 'retry-key' }), (err) => {
      assert.equal(err.message, 'Не удалось выполнить операцию. Повторите попытку позже');
      assert.equal(err.stack.includes(marker), false);
      return true;
    });
  }
});
