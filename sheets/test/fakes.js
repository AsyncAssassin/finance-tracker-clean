// Fakes of the Apps Script services Code.gs uses, and a loader that runs
// Code.gs in a vm context with them.
'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const SOURCE = fs.readFileSync(path.join(__dirname, '..', 'Code.gs'), 'utf8');


function propertyStore(initial) {
  const map = new Map(Object.entries(initial || {}));
  return {
    getProperty: (k) => (map.has(k) ? map.get(k) : null),
    setProperty: (k, v) => { map.set(k, String(v)); },
    deleteProperty: (k) => { map.delete(k); },
    map,
  };
}

// A sheet as a growing 2D array, rows and columns 1-based like Apps Script.
function fakeSheet(name, rows) {
  const data = (rows || []).map((r) => r.slice());
  const formulaWrites = [];
  const literal = (v) => {
    if (typeof v !== 'string') return v;
    if (v.startsWith('=')) formulaWrites.push(v);
    return v.startsWith("'") ? v.slice(1) : v;
  };
  const cell = (r, c) => {
    while (data.length < r) data.push([]);
    const row = data[r - 1];
    while (row.length < c) row.push('');
    return row;
  };
  const a1 = (ref) => {
    const m = /^([A-Z])(\d+)(?::([A-Z])(\d+))?$/.exec(ref);
    const col = (l) => l.charCodeAt(0) - 64;
    const r1 = +m[2], c1 = col(m[1]);
    const r2 = m[3] ? +m[4] : r1, c2 = m[3] ? col(m[3]) : c1;
    return [r1, c1, r2 - r1 + 1, c2 - c1 + 1];
  };
  const range = (r, c, nr, nc) => ({
    getValues: () => Array.from({ length: nr }, (_, i) => Array.from({ length: nc }, (_, j) => {
      const row = data[r - 1 + i] || [];
      const v = row[c - 1 + j];
      return v === undefined ? '' : v;
    })),
    getValue() { return this.getValues()[0][0]; },
    setValues(values) {
      assert.equal(values.length, nr, 'setValues rows');
      values.forEach((vr, i) => { assert.equal(vr.length, nc, 'setValues columns'); vr.forEach((v, j) => { cell(r + i, c + j)[c - 1 + j] = literal(v); }); });
      return this;
    },
    setValue(v) { cell(r, c)[c - 1] = literal(v); return this; },
    clearContent() { for (let i = 0; i < nr; i++) for (let j = 0; j < nc; j++) if (data[r - 1 + i]) data[r - 1 + i][c - 1 + j] = ''; return this; },
    setFontWeight() { return this; },
    setNumberFormat() { return this; },
  });
  return {
    name,
    data,
    formulaWrites,
    getRange: (r, c, nr, nc) => (typeof r === 'string' ? range(...a1(r)) : range(r, c, nr || 1, nc || 1)),
    getLastRow: () => {
      for (let i = data.length; i > 0; i--) if (data[i - 1].some((v) => v !== '' && v !== undefined)) return i;
      return 0;
    },
    getMaxRows: () => Math.max(data.length, 100),
    appendRow(row) { data.splice(this.getLastRow(), 0, row.map(literal)); },
    setFrozenRows() {},
  };
}

function environment(opts) {
  opts = opts || {};
  const sheets = new Map((opts.sheets || []).map((s) => [s.name, s]));
  const requests = [];
  const toasts = [];
  const alerts = [];
  let uuid = 0;
  const state = { locked: !!opts.locked, flushes: 0 };
  const context = {
    PropertiesService: {
      docs: propertyStore(opts.docProps === undefined ? { apiBaseUrl: 'https://api.example' } : opts.docProps),
      user: propertyStore(opts.userProps === undefined ? { token: 'tok', expiresAt: '2999-01-01T00:00:00Z', tokenBaseUrl: 'https://api.example' } : opts.userProps),
      getDocumentProperties() { return this.docs; },
      getUserProperties() { return this.user; },
    },
    UrlFetchApp: {
      fetch(url, params) {
        requests.push({ url, params });
        const r = opts.respond(url, params, sheets);
        return {
          getResponseCode: () => r.code,
          getContentText: () => (typeof r.body === 'string' ? r.body : JSON.stringify(r.body)),
          getHeaders: () => r.headers || {},
        };
      },
    },
    Utilities: {
      getUuid: () => 'uuid-' + ++uuid,
      // Fixed offsets are enough for the tests.
      formatDate: (d, tz, pattern) => {
        const offsets = { UTC: 0, 'Europe/Moscow': 3, 'Asia/Novosibirsk': 7, 'America/New_York': -4 };
        assert.ok(tz in offsets, 'unknown time zone ' + tz);
        const iso = new Date(d.getTime() + offsets[tz] * 3600e3).toISOString();
        return pattern === 'yyyy-MM-dd' ? iso.slice(0, 10) : iso.slice(0, 19).replace('T', ' ');
      },
    },
    // The script's zone differs from the spreadsheet's on purpose: dates must
    // follow the spreadsheet.
    Session: { getScriptTimeZone: () => 'Europe/Moscow' },
    LockService: {
      getDocumentLock: () => ({
        tryLock: () => { if (state.locked) return false; state.locked = true; return true; },
        releaseLock: () => { state.locked = false; },
      }),
    },
    SpreadsheetApp: {
      flush: () => { state.flushes++; },
      getActive: () => ({
        getSpreadsheetTimeZone: () => opts.spreadsheetTz || 'UTC',
        getSheetByName: (n) => sheets.get(n) || null,
        insertSheet: (n) => { const s = fakeSheet(n); sheets.set(n, s); return s; },
        toast: (text) => toasts.push(text),
      }),
      getUi: () => ({
        alert: (...args) => alerts.push(args),
        ButtonSet: { OK: 'OK', OK_CANCEL: 'OK_CANCEL' },
        Button: { OK: 'OK' },
      }),
    },
  };
  vm.createContext(context);
  vm.runInContext(SOURCE, context);
  return { gs: context, requests, toasts, alerts, sheets, state };
}

module.exports = { environment, fakeSheet };
