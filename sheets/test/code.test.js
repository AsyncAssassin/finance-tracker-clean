// Tests of sheets/Code.gs outside Google: the script runs in a vm context
// with small fakes of the Apps Script services it uses.
//
//   node --test 'sheets/test/*.test.js'
'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const { environment, fakeSheet } = require('./fakes.js');


// ---------- tests ----------

test('formats of cells', () => {
  const { gs } = environment({ respond: () => ({ code: 200, body: {} }) });
  assert.equal(gs.formatDate_(new Date(Date.UTC(2026, 9, 5)), 'UTC'), '2026-10-05');
  assert.equal(gs.formatDate_('5.10.2026', 'UTC'), '2026-10-05');
  assert.equal(gs.formatDate_('2026-10-05', 'UTC'), '2026-10-05');
  assert.equal(gs.formatDate_('', 'UTC'), '');
  assert.equal(gs.formatAmount_(1250.5), '1250.50');
  assert.equal(gs.formatAmount_('1 250,50'), '1250.50');
  assert.equal(gs.formatAmount_('1\u00a0250,5'), '1250.5');
  assert.equal(gs.formatAmount_(0.1 + 0.2), '0.30');
  // More than two decimals are sent as they are, so the server rejects them.
  assert.equal(gs.formatAmount_(1.005), '1.005');
  assert.match(gs.formatAmount_(100 / 3), /^33\.333/);
});

test('dates follow the time zone of the spreadsheet, not of the script', () => {
  const sheet = fakeSheet('Транзакции', [
    ['Дата', 'Категория', 'Сумма', 'Описание', 'ID', 'Статус', 'Ключ'],
    [new Date('2026-10-04T17:00:00Z'), 'еда', 10, ''], // 5 October 00:00 in Novosibirsk
  ]);
  let csv;
  const env = environment({
    sheets: [sheet],
    spreadsheetTz: 'Asia/Novosibirsk',
    respond: (url, params) => { csv = params.payload; return { code: 200, body: { total: 1, created: 1, failed: 0, rows: [{ line: 2, status: 'created', id: 1 }] } }; },
  });
  env.gs.sendNewTransactions_();
  assert.match(csv, /\n2026-10-05,еда,10\.00,/);
});

test('the import CSV keeps one line per row', () => {
  const { gs } = environment({ respond: () => ({ code: 200, body: {} }) });
  const csv = gs.buildImportCsv_([
    { date: '2026-10-01', category: 'еда', amount: '10.00', description: 'продукты, "свежие"\nи вода', key: 'k1' },
    { date: '', category: ' кафе', amount: '5', description: '', key: 'k2' },
    { date: '2026-10-02', category: 'еда\r\nвода', amount: '1', description: 'x', key: 'k3' }, // Alt+Enter in a category
  ]);
  assert.equal(csv,
    'date,category,amount,description,external_id\n' +
    '2026-10-01,еда,10.00,"продукты, ""свежие"" и вода",k1\n' +
    ',\" кафе\",5,,k2\n' +
    '2026-10-02,еда вода,1,x,k3\n');
});

test('error messages', () => {
  const { gs } = environment({ respond: () => ({ code: 200, body: {} }) });
  const err = (code, error, text) => gs.describeError_({ code, body: error ? { error } : null, text: text || '' });
  assert.match(err(400, { code: 'validation_error', message: 'x', details: { password: 'too short', email: 'bad' } }),
    /email: bad; password: too short/);
  assert.match(err(409, { code: 'budget_exceeded', message: 'x', details: { category: 'еда', period_label: '2026-10', spent: 4750.5, limit: 5000, remaining: 249.5 } }),
    /«еда» за 2026-10.*осталось 249.5/);
  assert.equal(err(401, { code: 'invalid_credentials', message: 'x' }), 'Неверный email или пароль');
  assert.match(err(200, null, '<!DOCTYPE html><html>proxy</html>'), /веб-страница/);
  assert.match(err(409, { code: 'conflict', message: 'busy' }), /busy \(conflict\)/);
});

test('requests need an address and a valid token', () => {
  let env = environment({ docProps: {}, respond: () => ({ code: 200, body: {} }) });
  assert.throws(() => env.gs.request_('get', '/api/budget'), /адрес API/);

  env = environment({ userProps: {}, respond: () => ({ code: 200, body: {} }) });
  assert.throws(() => env.gs.request_('get', '/api/budget'), /войдите/);

  env = environment({ userProps: { token: 'old', expiresAt: '2000-01-01T00:00:00Z' }, respond: () => ({ code: 200, body: {} }) });
  assert.throws(() => env.gs.request_('get', '/api/budget'), /войдите/, 'an expired token is not sent');

  env = environment({ respond: () => ({ code: 200, body: { items: [] } }) });
  const res = env.gs.request_('get', '/api/budget');
  assert.equal(res.code, 200);
  assert.equal(env.requests[0].url, 'https://api.example/api/budget');
  assert.equal(env.requests[0].params.headers.Authorization, 'Bearer tok');
  assert.equal(env.requests[0].params.muteHttpExceptions, true);
});

test('login stores the session; a 401 forgets it', () => {
  const env = environment({
    userProps: {},
    respond: (url) => (url.endsWith('/register')
      ? { code: 201, body: { user: { email: 'anna@example.com' }, access_token: 'jwt', expires_at: '2999-01-01T00:00:00Z' } }
      : { code: 401, body: { error: { code: 'unauthorized', message: 'expired' } } }),
  });
  const text = env.gs.login('Anna@Example.com', 'зелёный трамвай едет', true, 'https://api.example');
  assert.equal(text.ok, true);
  assert.match(text.message, /Аккаунт создан.*anna@example.com/);
  assert.equal(env.gs.PropertiesService.user.getProperty('token'), 'jwt');
  assert.deepEqual(JSON.parse(env.requests[0].params.payload), { email: 'Anna@Example.com', password: 'зелёный трамвай едет' });

  assert.throws(() => env.gs.check_(env.gs.request_('get', '/api/budget'), [200]), /Вход истёк/);
  assert.equal(env.gs.PropertiesService.user.getProperty('token'), null);
});

test('sending new rows imports them and writes back IDs and statuses', () => {
  const sheet = fakeSheet('Транзакции', [
    ['Дата', 'Категория', 'Сумма', 'Описание', 'ID', 'Статус', 'Ключ'],
    [new Date(Date.UTC(2026, 9, 1)), 'еда', 1250.5, 'продукты'],      // new
    ['2026-10-02', 'кафе', 100, 'обед', 5, 'сохранена', 'k-old'],      // sent before
    ['', '', '', ''],                                                   // blank
    ['03.10.2026', 'кафе', '1 800', 'ужин', '', '', 'k-retry'],        // sent, no answer
    ['2026-10-04', 'транспорт', 'сто', 'такси'],                        // bad amount
  ]);
  let csv;
  const env = environment({
    sheets: [sheet],
    respond: (url, params) => {
      csv = params.payload;
      return {
        code: 200,
        body: {
          total: 3, created: 1, failed: 2,
          rows: [
            { line: 2, status: 'created', id: 10 },
            { line: 3, status: 'failed', error: { code: 'already_exists', message: 'dup', details: { external_id: 'k-retry', id: 7 } } },
            { line: 4, status: 'failed', error: { code: 'validation_error', message: 'x', details: { amount: 'must be a number' } } },
          ],
        },
      };
    },
  });
  env.gs.sendNewTransactions_();

  assert.equal(env.requests.length, 1);
  assert.equal(env.requests[0].url, 'https://api.example/api/transactions/import');
  assert.equal(env.requests[0].params.contentType, 'text/csv; charset=utf-8');
  assert.equal(csv,
    'date,category,amount,description,external_id\n' +
    '2026-10-01,еда,1250.50,продукты,uuid-1\n' +
    '2026-10-03,кафе,1800,ужин,k-retry\n' +
    '2026-10-04,транспорт,сто,такси,uuid-2\n');
  const rows = sheet.getRange(2, 5, 5, 3).getValues();
  assert.deepEqual(rows[0], [10, 'сохранена', 'uuid-1']);
  assert.deepEqual(rows[1], [5, 'сохранена', 'k-old'], 'a sent row is left alone');
  assert.deepEqual(rows[3], [7, 'уже сохранена', 'k-retry']);
  assert.equal(rows[4][0], '');
  assert.match(rows[4][1], /^ошибка: .*amount: must be a number/);
  assert.match(env.toasts[0], /Сохранено: 1, с ошибками: 2/);
});

test('nothing to send makes no request', () => {
  const sheet = fakeSheet('Транзакции', [['Дата', 'Категория', 'Сумма', 'Описание', 'ID', 'Статус', 'Ключ'], ['2026-10-02', 'кафе', 100, '', 5, 'сохранена', 'k']]);
  const env = environment({ sheets: [sheet], respond: () => assert.fail('no request expected') });
  env.gs.sendNewTransactions_();
  assert.equal(env.requests.length, 0);
  assert.match(env.toasts[0], /Новых строк нет/);
});

test('the report goes into its sheet with the cache status', () => {
  const sheet = fakeSheet('Отчёт', [['С', new Date(Date.UTC(2026, 9, 1))], ['По', '31.10.2026']]);
  const env = environment({
    sheets: [sheet],
    respond: () => ({
      code: 200,
      headers: { 'x-cache': 'HIT' },
      body: {
        from: '2026-10-01', to: '2026-10-31', total: 6750.5,
        categories: [
          { category: 'еда', spent: 4750.5, budget: { period: 'month', limit: 5000, remaining: 249.5, percent: 95.01 } },
          { category: 'здоровье', spent: 700, budget: null },
        ],
      },
    }),
  });
  env.gs.loadReport_();
  assert.equal(env.requests[0].url, 'https://api.example/api/reports/summary?from=2026-10-01&to=2026-10-31');
  assert.deepEqual(sheet.getRange(4, 1, 4, 5).getValues(), [
    ['Категория', 'Потрачено', 'Лимит за период', 'Остаток', '% бюджета'],
    ['еда', 4750.5, 5000, 249.5, 95.01],
    ['здоровье', 700, '', '', ''],
    ['Итого', 6750.5, '', '', ''],
  ]);
  assert.equal(sheet.getRange('D2').getValue(), 'Кеш отчёта: HIT');
});

test('budgets are saved row by row and loaded back', () => {
  const sheet = fakeSheet('Бюджеты', [
    ['Категория', 'Лимит', 'Период', 'Статус'],
    ['еда', 5000, '', ''],
    ['кафе', '3000,5', 'year', ''],
    ['', '', '', ''],
    ['транспорт', 0, 'month', ''],
  ]);
  const env = environment({
    sheets: [sheet],
    respond: (url, params) => {
      if (params.method === 'get') {
        return { code: 200, body: { items: [{ category: 'еда', limit: 5000, period: 'month' }] } };
      }
      const b = JSON.parse(params.payload);
      if (b.limit === '0.00') return { code: 400, body: { error: { code: 'validation_error', message: 'x', details: { limit: 'must be greater than 0' } } } };
      return { code: b.category === 'еда' ? 200 : 201, body: b };
    },
  });
  env.gs.saveBudgets_();
  assert.deepEqual(env.requests.map((r) => [r.params.method, r.url]), [
    ['post', 'https://api.example/api/budget'],
    ['post', 'https://api.example/api/budget'],
    ['post', 'https://api.example/api/budget'],
  ]);
  assert.deepEqual(env.requests.map((r) => JSON.parse(r.params.payload)), [
    { category: 'еда', limit: '5000.00', period: '' },
    { category: 'кафе', limit: '3000.5', period: 'year' },
    { category: 'транспорт', limit: '0.00', period: 'month' },
  ]);
  assert.deepEqual(sheet.getRange(2, 4, 4, 1).getValues().map((r) => r[0]),
    ['обновлён', 'создан', '', 'ошибка: Проверьте поля: limit: must be greater than 0']);

  env.gs.loadBudgets_();
  assert.equal(env.requests.at(-1).params.method, 'get');
  assert.equal(env.requests.at(-1).url, 'https://api.example/api/budget');
  assert.deepEqual(sheet.getRange(2, 1, 2, 4).getValues(), [['еда', 5000, 'month', 'загружен'], ['', '', '', '']]);
});

test('the API address is checked and normalized', () => {
  const { gs } = environment({ respond: () => ({ code: 200, body: {} }) });
  assert.equal(gs.normalizeBaseUrl_(' https://api.example.com/ '), 'https://api.example.com');
  assert.throws(() => gs.normalizeBaseUrl_('api.example.com'), /https:\/\//);
});

test('a second send while one runs is refused', () => {
  const sheet = fakeSheet('Транзакции', [['Дата', 'Категория', 'Сумма', 'Описание', 'ID', 'Статус', 'Ключ'], ['2026-10-01', 'еда', 1, '']]);
  const env = environment({ sheets: [sheet], locked: true, respond: () => assert.fail('no request while locked') });
  assert.throws(() => env.gs.sendNewTransactions_(), /уже идёт/);
  assert.equal(sheet.getRange(2, 7).getValue(), '', 'no keys are written');
});

test('results find their rows by key even if the rows moved during the request', () => {
  const sheet = fakeSheet('Транзакции', [
    ['Дата', 'Категория', 'Сумма', 'Описание', 'ID', 'Статус', 'Ключ'],
    ['2026-10-01', 'еда', 1, 'первая'],
    ['2026-10-02', 'кафе', 2, 'вторая'],
  ]);
  const env = environment({
    sheets: [sheet],
    respond: () => {
      // The user sorts the sheet while the request runs.
      sheet.data.splice(1, 2, sheet.data[2], sheet.data[1]);
      return { code: 200, body: { total: 2, created: 2, failed: 0, rows: [{ line: 2, status: 'created', id: 11 }, { line: 3, status: 'created', id: 12 }] } };
    },
  });
  env.gs.sendNewTransactions_();
  assert.deepEqual(sheet.getRange(2, 4, 2, 2).getValues(), [['вторая', 12], ['первая', 11]]);
});

test('a copied row gets its own key and is stored as new', () => {
  const sheet = fakeSheet('Транзакции', [
    ['Дата', 'Категория', 'Сумма', 'Описание', 'ID', 'Статус', 'Ключ'],
    ['2026-10-01', 'еда', 1, 'оригинал', 12, 'сохранена', 'k1'],
    ['2026-10-02', 'еда', 5, 'копия', '', '', 'k1'],
  ]);
  let csv;
  const env = environment({
    sheets: [sheet],
    respond: (url, params) => { csv = params.payload; return { code: 200, body: { total: 1, created: 1, failed: 0, rows: [{ line: 2, status: 'created', id: 13 }] } }; },
  });
  env.gs.sendNewTransactions_();
  assert.match(csv, /,копия,uuid-1\n$/);
  assert.deepEqual(sheet.getRange(3, 5, 1, 3).getValues(), [[13, 'сохранена', 'uuid-1']]);
});

test('the dialog retries with the same key and does not duplicate the row', () => {
  const sheet = fakeSheet('Транзакции', [['Дата', 'Категория', 'Сумма', 'Описание', 'ID', 'Статус', 'Ключ']]);
  let calls = 0;
  const env = environment({
    sheets: [sheet],
    respond: (url, params) => {
      calls++;
      const b = JSON.parse(params.payload);
      assert.equal(b.external_id, 'dialog-key');
      assert.equal(b.amount, '1250.50', 'the dialog normalizes the amount like the sheet');
      return calls === 1
        ? { code: 201, body: { id: 21, date: b.date, category: b.category, amount: 1250.5, description: b.description } }
        : { code: 409, body: { error: { code: 'already_exists', message: 'dup', details: { external_id: 'dialog-key', id: 21 } } } };
    },
  });
  const form = { key: 'dialog-key', date: '2026-10-05', category: 'еда', amount: '1 250,50', description: 'обед' };
  assert.match(env.gs.addTransaction(form).message, /Сохранена транзакция №21/);
  assert.match(env.gs.addTransaction(form).message, /Уже была сохранена транзакция №21/);
  assert.equal(sheet.getLastRow(), 2, 'one row for one transaction');
  assert.deepEqual(sheet.getRange(2, 5, 1, 3).getValues(), [[21, 'уже сохранена', 'dialog-key']]);
});

test('budgets: an expired login stops the rest and every row gets a fresh status', () => {
  const sheet = fakeSheet('Бюджеты', [
    ['Категория', 'Лимит', 'Период', 'Статус'],
    ['еда', 5000, '', 'создан'],
    ['кафе', 3000, '', 'создан'],
  ]);
  const env = environment({ sheets: [sheet], respond: () => ({ code: 401, body: { error: { code: 'unauthorized', message: 'expired' } } }) });
  env.gs.saveBudgets_();
  assert.equal(env.requests.length, 1);
  assert.match(sheet.getRange('D2').getValue(), /^ошибка: Вход истёк/);
  assert.equal(sheet.getRange('D3').getValue(), 'не отправлен: нужно войти заново');
  assert.equal(env.gs.PropertiesService.user.getProperty('token'), null);
});

test('budgets: a network failure is shown in the rows', () => {
  const sheet = fakeSheet('Бюджеты', [
    ['Категория', 'Лимит', 'Период', 'Статус'],
    ['еда', 5000, '', ''],
    ['кафе', 3000, '', ''],
  ]);
  const env = environment({ sheets: [sheet], respond: () => { throw new Error('DNS error: api.example'); } });
  env.gs.saveBudgets_();
  assert.equal(sheet.getRange('D2').getValue(), 'ошибка: Не удалось связаться с API');
  assert.equal(sheet.getRange('D3').getValue(), 'не отправлен: сервер недоступен');
});
