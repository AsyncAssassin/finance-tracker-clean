/**
 * Finance Tracker: Google Таблица как клиент HTTP API проекта.
 *
 * Меню «Finance Tracker»:
 *   Подключение…                 адрес API; общий для документа (свойства документа)
 *   Войти или зарегистрироваться…  токен хранится в личных свойствах пользователя
 *   Добавить транзакцию…          одна транзакция: POST /api/transactions
 *   Отправить новые строки        строки листа «Транзакции» без ID одним запросом
 *                                 POST /api/transactions/import; ID или ошибка пишутся в строку
 *   Загрузить отчёт за период     GET /api/reports/summary за период из B1:B2 листа «Отчёт»
 *   Сохранить / загрузить бюджеты лист «Бюджеты»: POST /api/budget, GET /api/budget
 *
 */

const SHEET_TX = 'Транзакции';
const SHEET_REPORT = 'Отчёт';
const SHEET_BUDGETS = 'Бюджеты';

// Колонки листа «Транзакции». «Ключ» содержит external_id строки: с ним повторная
// отправка (например, после обрыва связи) не создаёт транзакцию дважды.
const TX_HEADER = ['Дата', 'Категория', 'Сумма', 'Описание', 'ID', 'Статус', 'Ключ'];
const TX = { date: 0, category: 1, amount: 2, description: 3, id: 4, status: 5, key: 6 };
const BUDGET_HEADER = ['Категория', 'Лимит', 'Период', 'Статус'];

function onOpen() {
  SpreadsheetApp.getUi()
    .createMenu('Finance Tracker')
    .addItem('Подключение…', 'showSettings')
    .addItem('Войти или зарегистрироваться…', 'showLogin')
    .addItem('Выйти', 'logout')
    .addSeparator()
    .addItem('Добавить транзакцию…', 'showAddTransaction')
    .addItem('Отправить новые строки листа «Транзакции»', 'sendNewTransactions')
    .addItem('Загрузить отчёт за период', 'loadReport')
    .addSeparator()
    .addItem('Сохранить бюджеты с листа «Бюджеты»', 'saveBudgets')
    .addItem('Загрузить бюджеты', 'loadBudgets')
    .addSeparator()
    .addItem('Подготовить листы', 'setupSheets')
    .addToUi();
}

// ---------- Пункты меню: ошибки показываются окном, а не красной плашкой ----------

function showSettings() { run_(showSettings_); }
function showLogin() { run_(showLogin_); }
function showAddTransaction() { run_(showAddTransaction_); }
function sendNewTransactions() { run_(sendNewTransactions_); }
function loadReport() { run_(loadReport_); }
function saveBudgets() { run_(saveBudgets_); }
function loadBudgets() { run_(loadBudgets_); }
function setupSheets() { run_(setupSheets_); }

function run_(fn) {
  try {
    fn();
  } catch (e) {
    SpreadsheetApp.getUi().alert('Finance Tracker', e.message, SpreadsheetApp.getUi().ButtonSet.OK);
  }
}

// ---------- Подключение и вход ----------

function showSettings_() {
  const ui = SpreadsheetApp.getUi();
  const current = getBaseUrl_();
  const answer = ui.prompt('Подключение к Finance Tracker',
    'Адрес API, например https://api.example.com' + (current ? '\nСейчас: ' + current : ''),
    ui.ButtonSet.OK_CANCEL);
  if (answer.getSelectedButton() !== ui.Button.OK) return;
  const url = normalizeBaseUrl_(answer.getResponseText());
  if (url !== current) clearSession_();
  PropertiesService.getDocumentProperties().setProperty('apiBaseUrl', url);
  const ping = request_('get', '/ping', { auth: false });
  ui.alert('Finance Tracker', ping.code === 200 && ping.text === 'pong'
    ? 'Подключено: ' + url
    : 'Адрес сохранён, но /ping ответил ' + ping.code + '. Проверьте, что сервер запущен.', ui.ButtonSet.OK);
}

function normalizeBaseUrl_(text) {
  const url = String(text || '').trim().replace(/\/+$/, '').toLowerCase();
  const match = /^https:\/\/([a-z0-9.-]+)(?::443)?$/.exec(url);
  if (!match || !match[1].split('.').every(function (label) {
    return /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(label);
  }) || match[1].length > 253) {
    throw new Error('Укажите только адрес сервера https://api.example.com, без пути, пароля и параметров');
  }
  return 'https://' + match[1];
}

function getBaseUrl_() {
  return PropertiesService.getDocumentProperties().getProperty('apiBaseUrl') || '';
}

function showLogin_() {
  const html = HtmlService.createHtmlOutputFromFile('Login').setWidth(420).setHeight(360);
  SpreadsheetApp.getUi().showModalDialog(html, 'Вход в Finance Tracker');
}

/** Вызывается из Login.html. */
function getLoginContext() {
  return { baseUrl: normalizeBaseUrl_(getBaseUrl_()) };
}

// Expected business errors travel as values, so Apps Script does not record
// financial details as unhandled exceptions. Unexpected errors use fixed text.
function formResult_(fn) {
  try {
    return { ok: true, message: fn() };
  } catch (err) {
    if (err && err.expected === true) return { ok: false, message: err.message };
    throw new Error('Не удалось выполнить операцию. Повторите попытку позже');
  }
}

function expectedError_(message) {
  const err = new Error(message);
  err.expected = true;
  return err;
}

function login(email, password, register, expectedBaseUrl) {
  return formResult_(function () { return login_(email, password, register, expectedBaseUrl); });
}

function login_(email, password, register, expectedBaseUrl) {
  const base = normalizeBaseUrl_(getBaseUrl_());
  if (base !== expectedBaseUrl) throw expectedError_('Адрес API изменился. Закройте окно входа и откройте его заново');
  const path = register ? '/api/auth/register' : '/api/auth/login';
  const res = check_(request_('post', path, { auth: false, baseUrl: base, json: { email: email, password: password } }), [200, 201]);
  if (!res.body || typeof res.body.access_token !== 'string' || !res.body.access_token ||
      !res.body.user || typeof res.body.user.email !== 'string' || typeof res.body.expires_at !== 'string' ||
      !(new Date(res.body.expires_at).getTime() > Date.now())) {
    throw new Error('Сервер вернул некорректный ответ входа');
  }
  const props = PropertiesService.getUserProperties();
  clearSession_();
  props.setProperty('email', res.body.user.email);
  props.setProperty('expiresAt', res.body.expires_at);
  props.setProperty('tokenBaseUrl', base);
  props.setProperty('token', res.body.access_token);
  return (register ? 'Аккаунт создан. ' : '') + 'Вы вошли как ' + res.body.user.email +
    '; вход действует до ' + res.body.expires_at.replace('T', ' ').replace('Z', ' UTC');
}

/** Токен, если он есть и ещё действует. */
function getToken_(base) {
  const props = PropertiesService.getUserProperties();
  const token = props.getProperty('token');
  const expiresAt = props.getProperty('expiresAt');
  if (!token) return '';
  if (props.getProperty('tokenBaseUrl') !== base || !(new Date(expiresAt).getTime() > Date.now())) {
    clearSession_();
    return '';
  }
  return token;
}

function clearSession_() {
  const props = PropertiesService.getUserProperties();
  ['token', 'email', 'expiresAt', 'tokenBaseUrl'].forEach(function (key) { props.deleteProperty(key); });
}

function logout() {
  clearSession_();
  SpreadsheetApp.getActive().toast('Вход завершён', 'Finance Tracker');
}

// ---------- HTTP ----------

/**
 * Выполняет запрос к API. При options.auth === false токен не передаётся; options.json задаёт
 * тело JSON; options.csv задаёт тело text/csv. Возвращает {code, text, body, headers}.
 */
function request_(method, path, options) {
  options = options || {};
  const configured = options.baseUrl || getBaseUrl_();
  if (!configured) throw expectedError_('Сначала укажите адрес API: меню Finance Tracker → Подключение…');
  const base = normalizeBaseUrl_(configured);
  const headers = {};
  if (options.auth !== false) {
    const token = getToken_(base);
    if (!token) throw expectedError_('Сначала войдите: меню Finance Tracker → Войти или зарегистрироваться…');
    headers.Authorization = 'Bearer ' + token;
  }
  const params = { method: method, headers: headers, muteHttpExceptions: true, followRedirects: false };
  if (options.json !== undefined) {
    params.contentType = 'application/json';
    params.payload = JSON.stringify(options.json);
  } else if (options.csv !== undefined) {
    params.contentType = 'text/csv; charset=utf-8';
    params.payload = options.csv;
  }
  let resp;
  try {
    resp = UrlFetchApp.fetch(base + path, params);
  } catch (err) {
    throw new Error('Не удалось связаться с API');
  }
  const text = resp.getContentText();
  let body = null;
  try {
    body = JSON.parse(text);
  } catch (e) {
    // не JSON: /ping, CSV или страница-заглушка прокси
  }
  return { code: resp.getResponseCode(), text: text, body: body, headers: resp.getHeaders() };
}

/** Возвращает res, если код ожидаемый, иначе бросает понятную ошибку. */
function check_(res, okCodes) {
  if (okCodes.indexOf(res.code) >= 0) return res;
  if (res.code === 401 && res.body && res.body.error && res.body.error.code === 'unauthorized') {
    clearSession_();
  }
  if (res.code >= 400 && res.code < 500 && res.body && res.body.error) {
    throw expectedError_(describeError_(res));
  }
  throw new Error('Неожиданный ответ API');
}

/** Текст ошибки API для человека. */
function describeError_(res) {
  const e = res.body && res.body.error;
  if (!e) {
    const looksLikePage = /<html/i.test(res.text || '');
    return 'Сервер ответил ' + res.code + (looksLikePage
      ? ': вместо API пришла веб-страница, проверьте адрес и что сервер запущен'
      : (res.text ? ': ' + String(res.text).slice(0, 200) : ''));
  }
  switch (e.code) {
    case 'unauthorized':
      return 'Вход истёк или не выполнен: меню Finance Tracker → Войти или зарегистрироваться…';
    case 'invalid_credentials':
      return 'Неверный email или пароль';
    case 'email_taken':
      return 'Аккаунт с таким email уже есть, войдите';
    case 'validation_error':
      return 'Проверьте поля: ' + fieldProblems_(e.details);
    case 'budget_exceeded':
      return 'Превышен бюджет категории «' + e.details.category + '» за ' + e.details.period_label +
        ': потрачено ' + e.details.spent + ' из ' + e.details.limit + ', осталось ' + e.details.remaining;
    default:
      return e.message + ' (' + e.code + ')';
  }
}

function fieldProblems_(details) {
  if (!details || typeof details !== 'object') return '';
  return Object.keys(details).sort().map(function (k) { return k + ': ' + details[k]; }).join('; ');
}

/** Значение заголовка ответа без учёта регистра имени. */
function headerValue_(headers, name) {
  const wanted = name.toLowerCase();
  const key = Object.keys(headers || {}).filter(function (k) { return k.toLowerCase() === wanted; })[0];
  return key ? String(headers[key]) : '';
}

// ---------- Транзакции ----------

function showAddTransaction_() {
  const html = HtmlService.createHtmlOutputFromFile('AddTransaction').setWidth(420).setHeight(440);
  SpreadsheetApp.getUi().showModalDialog(html, 'Новая транзакция');
}

/**
 * Вызывается из AddTransaction.html: сохраняет транзакцию и записывает её в лист.
 * form.key содержит external_id, который окно создаёт один раз: повторное нажатие после
 * ошибки связи отправит тот же ключ, и сервер не сохранит транзакцию дважды.
 */
function addTransaction(form) {
  return formResult_(function () { return addTransaction_(form); });
}

function addTransaction_(form) {
  const key = String(form.key || '') || Utilities.getUuid();
  const res = request_('post', '/api/transactions', {
    json: {
      amount: formatAmount_(form.amount),
      category: form.category || '',
      description: form.description || '',
      date: form.date || '',
      external_id: key,
    },
  });
  let tx;
  let status = 'сохранена';
  const err = res.body && res.body.error;
  if (res.code === 409 && err && err.code === 'already_exists' && err.details) {
    // Прошлая попытка дошла до сервера: это та же транзакция.
    tx = { id: err.details.id, date: form.date, category: form.category, amount: formatAmount_(form.amount), description: form.description || '' };
    status = 'уже сохранена';
  } else {
    tx = check_(res, [201]).body;
  }
  const sheet = ensureSheet_(SHEET_TX, TX_HEADER);
  const values = readTable_(sheet, TX_HEADER.length);
  const row = literalRows_([[tx.date, tx.category, tx.amount, tx.description, tx.id, status, key]])[0];
  const existing = findRowByKey_(values, key);
  if (existing > 0) {
    sheet.getRange(existing + 1, 1, 1, row.length).setValues([row]);
  } else {
    sheet.appendRow(row);
  }
  return (status === 'сохранена' ? 'Сохранена' : 'Уже была сохранена') + ' транзакция №' + tx.id;
}

/** Индекс строки values (заголовок имеет индекс 0) с ключом key, или -1. */
function findRowByKey_(values, key) {
  for (let i = 1; i < values.length; i++) {
    if (String(values[i][TX.key]) === key) return i;
  }
  return -1;
}

function sendNewTransactions_() {
  // Два одновременных нажатия создали бы разные ключи для одних строк.
  const lock = LockService.getDocumentLock();
  if (!lock.tryLock(30000)) throw new Error('Отправка уже идёт, дождитесь её окончания');
  try {
    sendLocked_();
  } finally {
    lock.releaseLock();
  }
}

function sendLocked_() {
  const sheet = ensureSheet_(SHEET_TX, TX_HEADER);
  const tz = spreadsheetTimeZone_();
  const values = readTable_(sheet, TX_HEADER.length);
  const firstRowOfKey = Object.create(null);
  for (let i = 1; i < values.length; i++) {
    const k = String(values[i][TX.key]);
    if (k && !(k in firstRowOfKey)) firstRowOfKey[k] = i;
  }
  const pending = []; // строки без ID
  for (let i = 1; i < values.length; i++) {
    const row = values[i];
    if (row[TX.id] !== '' || isBlank_(row.slice(0, 4))) continue;
    // Ключ строки уникален в листе; у скопированной строки он чужой, нужен новый.
    if (!row[TX.key] || firstRowOfKey[String(row[TX.key])] !== i) row[TX.key] = Utilities.getUuid();
    pending.push({
      date: formatDate_(row[TX.date], tz),
      category: String(row[TX.category]).trim(),
      amount: formatAmount_(row[TX.amount]),
      description: String(row[TX.description]),
      key: String(row[TX.key]),
    });
  }
  if (!pending.length) {
    SpreadsheetApp.getActive().toast('Новых строк нет: у всех строк уже есть ID', 'Finance Tracker');
    return;
  }
  // Ключи записываются до отправки: при повторе после обрыва строки уйдут с теми же ключами.
  sheet.getRange(2, TX.key + 1, values.length - 1, 1).setValues(literalRows_(values.slice(1).map(function (r) { return [r[TX.key]]; })));
  SpreadsheetApp.flush();

  const res = check_(request_('post', '/api/transactions/import', { csv: buildImportCsv_(pending) }), [200]);

  // Пока шёл запрос, строки могли передвинуть: итоги ищут свою строку по ключу.
  const outcome = importOutcomes_(pending, res.body);
  const now = readTable_(sheet, TX_HEADER.length);
  for (let i = 1; i < now.length; i++) {
    const o = outcome[String(now[i][TX.key])];
    if (o && now[i][TX.id] === '') {
      now[i][TX.id] = o.id;
      now[i][TX.status] = o.status;
    }
  }
  if (now.length > 1) sheet.getRange(2, TX.id + 1, now.length - 1, 2).setValues(literalRows_(now.slice(1).map(function (r) { return [r[TX.id], r[TX.status]]; })));
  SpreadsheetApp.getActive().toast('Сохранено: ' + res.body.created + ', с ошибками: ' + res.body.failed, 'Finance Tracker');
}

/**
 * CSV для импорта. Строка CSV номер N соответствует pending[N-2], заголовок имеет номер 1. Поэтому
 * переводы строк в любом поле заменяются пробелами: иначе запись заняла бы
 * несколько строк файла и номера в ответе съехали бы.
 */
function buildImportCsv_(pending) {
  const lines = ['date,category,amount,description,external_id'];
  pending.forEach(function (p) {
    lines.push([p.date, csvText_(p.category), p.amount, csvText_(p.description), csvText_(p.key)].map(csvField_).join(','));
  });
  return lines.join('\n') + '\n';
}

// Match the API's reversible CSV escaping, including literal apostrophes.
function csvText_(value) {
  const text = String(value === null || value === undefined ? '' : value).replace(/[\r\n]+/g, ' ');
  return /^['=+@\-\t\r\n]/.test(text) ? "'" + text : text;
}

// Prefix text with a Sheets literal marker. It also preserves an existing
// apostrophe and prevents an API value from becoming an executable formula.
function literalRows_(rows) {
  return rows.map(function (row) {
    return row.map(function (value) { return typeof value === 'string' && value !== '' ? "'" + value : value; });
  });
}

function csvField_(value) {
  const s = String(value === null || value === undefined ? '' : value).replace(/[\r\n]+/g, ' ');
  return /[",]|^\s|\s$/.test(s) ? '"' + s.replace(/"/g, '""') + '"' : s;
}

/** Итоги импорта по ключам строк: {ключ: {id, status}}. */
function importOutcomes_(pending, result) {
  const byLine = Object.create(null);
  result.rows.forEach(function (r) { byLine[r.line] = r; });
  const out = Object.create(null);
  pending.forEach(function (p, i) {
    const r = byLine[i + 2];
    if (r && r.status === 'created') {
      out[p.key] = { id: r.id, status: 'сохранена' };
    } else if (r && r.error && r.error.code === 'already_exists' && r.error.details && r.error.details.id) {
      out[p.key] = { id: r.error.details.id, status: 'уже сохранена' }; // отправлялась раньше
    } else {
      out[p.key] = { id: '', status: 'ошибка: ' + (r && r.error ? describeError_({ code: 400, body: { error: r.error } }) : 'нет ответа по строке') };
    }
  });
  return out;
}

// ---------- Отчёт ----------

function loadReport_() {
  const sheet = ensureReportSheet_();
  const tz = spreadsheetTimeZone_();
  const from = formatDate_(sheet.getRange('B1').getValue(), tz);
  const to = formatDate_(sheet.getRange('B2').getValue(), tz);
  if (!from || !to) throw new Error('Укажите период в ячейках B1 (с) и B2 (по) листа «Отчёт»');
  const res = check_(request_('get', '/api/reports/summary?from=' + encodeURIComponent(from) + '&to=' + encodeURIComponent(to)), [200]);
  const table = reportTable_(res.body);
  sheet.getRange(4, 1, Math.max(sheet.getMaxRows() - 3, 1), 5).clearContent();
  sheet.getRange(4, 1, table.length, table[0].length).setValues(literalRows_(table));
  sheet.getRange('D1').setValue('Обновлено ' + Utilities.formatDate(new Date(), tz, 'yyyy-MM-dd HH:mm:ss'));
  sheet.getRange('D2').setValue('Кеш отчёта: ' + (headerValue_(res.headers, 'X-Cache') || 'выключен'));
}

function reportTable_(summary) {
  const rows = [['Категория', 'Потрачено', 'Лимит за период', 'Остаток', '% бюджета']];
  summary.categories.forEach(function (c) {
    const b = c.budget;
    rows.push([c.category, c.spent, b ? b.limit : '', b ? b.remaining : '', b ? b.percent : '']);
  });
  rows.push(['Итого', summary.total, '', '', '']);
  return rows;
}

// ---------- Бюджеты ----------

function saveBudgets_() {
  const sheet = ensureSheet_(SHEET_BUDGETS, BUDGET_HEADER);
  const values = readTable_(sheet, BUDGET_HEADER.length);
  let saved = 0;
  let stop = '';
  try {
    for (let i = 1; i < values.length; i++) {
      const row = values[i];
      row[3] = '';
      if (isBlank_(row.slice(0, 3))) continue;
      if (stop) {
        row[3] = 'не отправлен: ' + stop;
        continue;
      }
      try {
        const res = request_('post', '/api/budget', {
          json: { category: String(row[0]).trim(), limit: formatAmount_(row[1]), period: String(row[2] || '').trim() },
        });
        if (res.code === 200 || res.code === 201) {
          row[3] = res.code === 201 ? 'создан' : 'обновлён';
          saved++;
        } else {
          row[3] = 'ошибка: ' + describeError_(res);
          if (res.code === 401) {
            clearSession_();
            stop = 'нужно войти заново';
          }
        }
      } catch (e) {
        row[3] = 'ошибка: ' + e.message; // нет связи с сервером и т. п.
        stop = 'сервер недоступен';
      }
    }
  } finally {
    if (values.length > 1) sheet.getRange(2, 4, values.length - 1, 1).setValues(literalRows_(values.slice(1).map(function (r) { return [r[3]]; })));
  }
  SpreadsheetApp.getActive().toast('Сохранено бюджетов: ' + saved, 'Finance Tracker');
}

function loadBudgets_() {
  const sheet = ensureSheet_(SHEET_BUDGETS, BUDGET_HEADER);
  const res = check_(request_('get', '/api/budget'), [200]);
  const rows = res.body.items.map(function (b) { return [b.category, b.limit, b.period, 'загружен']; });
  if (sheet.getLastRow() > 1) sheet.getRange(2, 1, sheet.getLastRow() - 1, BUDGET_HEADER.length).clearContent();
  if (rows.length) sheet.getRange(2, 1, rows.length, BUDGET_HEADER.length).setValues(literalRows_(rows));
  SpreadsheetApp.getActive().toast('Загружено бюджетов: ' + rows.length, 'Finance Tracker');
}

// ---------- Листы и форматы ----------

function setupSheets_() {
  ensureSheet_(SHEET_TX, TX_HEADER);
  ensureReportSheet_();
  ensureSheet_(SHEET_BUDGETS, BUDGET_HEADER);
  SpreadsheetApp.getActive().toast('Листы готовы', 'Finance Tracker');
}

function ensureSheet_(name, header) {
  const book = SpreadsheetApp.getActive();
  let sheet = book.getSheetByName(name);
  if (!sheet) {
    sheet = book.insertSheet(name);
    sheet.getRange(1, 1, 1, header.length).setValues([header]).setFontWeight('bold');
    sheet.setFrozenRows(1);
  }
  return sheet;
}

function ensureReportSheet_() {
  const book = SpreadsheetApp.getActive();
  let sheet = book.getSheetByName(SHEET_REPORT);
  if (!sheet) {
    sheet = book.insertSheet(SHEET_REPORT);
    // Текущий месяц по часам таблицы; текстом, чтобы таблица сама прочла его как дату.
    const today = Utilities.formatDate(new Date(), spreadsheetTimeZone_(), 'yyyy-MM-dd').split('-');
    const lastDay = new Date(Date.UTC(+today[0], +today[1], 0)).getUTCDate();
    sheet.getRange('B1:B2').setNumberFormat('yyyy-mm-dd');
    sheet.getRange('A1:B2').setValues([
      ['С', today[0] + '-' + today[1] + '-01'],
      ['По', today[0] + '-' + today[1] + '-' + lastDay],
    ]);
  }
  return sheet;
}

/**
 * Часовой пояс таблицы (Файл → Настройки): в нём таблица хранит введённые даты.
 * Пояс скрипта из appsscript.json может отличаться, и дата съехала бы на день.
 */
function spreadsheetTimeZone_() {
  return SpreadsheetApp.getActive().getSpreadsheetTimeZone();
}

/** Дата ячейки как YYYY-MM-DD: из значения-даты, из текста YYYY-MM-DD или ДД.ММ.ГГГГ. */
function formatDate_(value, tz) {
  if (value === '' || value === null || value === undefined) return '';
  if (Object.prototype.toString.call(value) === '[object Date]') {
    return Utilities.formatDate(value, tz, 'yyyy-MM-dd');
  }
  const s = String(value).trim();
  const ru = /^(\d{1,2})\.(\d{1,2})\.(\d{4})$/.exec(s);
  if (ru) return ru[3] + '-' + ('0' + ru[2]).slice(-2) + '-' + ('0' + ru[1]).slice(-2);
  return s; // YYYY-MM-DD или то, что проверит сервер
}

/**
 * Сумма как текст с точкой: число из ячейки или текст вроде «1 250,50». Число
 * с лишними знаками (1.005, 100/3) уходит как есть, и сервер его отклонит, а не
 * округлит молча.
 */
function formatAmount_(value) {
  if (typeof value === 'number') {
    const cents = value * 100;
    return Math.abs(cents - Math.round(cents)) < 1e-6 ? (Math.round(cents) / 100).toFixed(2) : String(value);
  }
  return String(value === null || value === undefined ? '' : value).replace(/[\s ]/g, '').replace(',', '.');
}

/**
 * Значения листа от первой строки до последней заполненной, ровно width
 * колонок: getDataRange вернул бы только заполненные колонки, и пустой ID
 * выглядел бы как undefined, а не как пустая ячейка.
 */
function readTable_(sheet, width) {
  const last = sheet.getLastRow();
  return last < 1 ? [] : sheet.getRange(1, 1, last, width).getValues();
}

function isBlank_(cells) {
  return cells.every(function (c) { return c === '' || c === null || c === undefined; });
}
