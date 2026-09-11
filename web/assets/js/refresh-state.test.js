const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

function element(value = '') {
  return {
    value, dataset: {}, style: {}, textContent: '', disabled: false,
    classList: { add() {}, remove() {}, toggle() {} },
    setAttribute() {}, addEventListener() {}, appendChild() {},
    querySelector() { return null; }, querySelectorAll() { return []; },
    closest() { return this; }
  };
}

function page(file, extras = {}) {
  const elements = new Map();
  const context = vm.createContext({
    console: { log() {}, error() {}, warn() {} }, URLSearchParams, Date,
    setTimeout, clearTimeout, setInterval, clearInterval,
    CustomEvent: function (type) { this.type = type; }, dispatchEvent() {},
    localStorage: { getItem() { return null; }, setItem() {} },
    t: (key, args = {}) => `${key}${args.time || ''}`,
    initPageBootstrap() {}, addEventListener() {}, updateRefreshStatus() {},
    showSuccess() {}, showError() {}, showNotification() {},
    document: {
      getElementById: id => elements.get(id) || null,
      querySelectorAll: () => [], querySelector: () => null,
      addEventListener() {}, removeEventListener() {},
      createElement: () => element(),
      documentElement: element(), head: element()
    },
    ...extras
  });
  context.window = context;
  vm.runInContext(fs.readFileSync(path.join(__dirname, file), 'utf8'), context);
  return { context, elements, run: code => vm.runInContext(code, context) };
}

test('channel pagination accepts only the newest response, including counts', async () => {
  const requests = [];
  const p = page('channels-data.js', {
    filters: {}, channelsPageSize: 20, channelsCurrentPage: 1, channelStatsRange: 'today',
    channelsReadURL: a => a, filterChannels() {}, updateChannelsPagination() {},
    fetchAPIWithAuth() { const d = deferred(); requests.push(d); return d.promise; }
  });
  const first = p.run('loadChannels()');
  p.run('channelsCurrentPage = 2');
  const second = p.run('loadChannels()');
  requests[1].resolve({ success: true, data: [{ id: 22 }], count: 60 });
  await second;
  requests[0].resolve({ success: true, data: [{ id: 11 }], count: 1 });
  await first;
  assert.equal(p.run('channels[0].id'), 22);
  assert.equal(p.run('channelsCurrentPage'), 2);
  assert.equal(p.run('channelsTotalCount'), 60);
});

test('outdated channel failure cannot replace the current page with an error', async () => {
  const requests = [];
  let errors = 0;
  const p = page('channels-data.js', {
    filters: {}, channelsPageSize: 20, channelsCurrentPage: 1, channelStatsRange: 'today',
    channelsReadURL: a => a, filterChannels() {}, showError() { errors++; },
    fetchAPIWithAuth() { const d = deferred(); requests.push(d); return d.promise; }
  });
  const first = p.run('loadChannels()');
  const second = p.run('loadChannels()');
  requests[1].resolve({ success: true, data: [{ id: 2 }], count: 1 });
  await second;
  requests[0].reject(new Error('late failure'));
  await first;
  assert.equal(errors, 0);
  assert.equal(p.run('channels[0].id'), 2);
});

test('saving settings preserves edits made in flight and prevents duplicate writes', async () => {
  const d = deferred();
  let calls = 0;
  const p = page('settings.js', { fetchDataWithAuth() { calls++; return d.promise; } });
  const input = element('20');
  const button = element();
  p.elements.set('timeout', input);
  p.elements.set('save-all-btn', button);
  p.context.document.querySelectorAll = () => [button];
  p.run('originalSettings = { timeout: "10" }');
  const saving = p.run('saveAllSettings()');
  assert.equal(button.disabled, true);
  input.value = '30';
  await p.run('saveAllSettings()');
  assert.equal(calls, 1);
  d.resolve({});
  await saving;
  assert.equal(input.value, '30');
  assert.equal(p.run('originalSettings.timeout'), '20');
  assert.notEqual(input.style.background, '');
  assert.equal(button.disabled, false);
});

test('failed setting saves keep edits and re-enable actions', async () => {
  const p = page('settings.js', { fetchDataWithAuth: async () => { throw new Error('offline'); } });
  const input = element('20');
  const button = element();
  p.elements.set('timeout', input);
  p.elements.set('save-all-btn', button);
  p.context.document.querySelectorAll = () => [button];
  p.run('originalSettings = { timeout: "10" }');
  await p.run('saveAllSettings()');
  assert.equal(input.value, '20');
  assert.equal(p.run('originalSettings.timeout'), '10');
  assert.equal(button.disabled, false);
});

test('resetting a setting also preserves edits made during its request', async () => {
  const d = deferred();
  const started = deferred();
  const p = page('settings.js', {
    showConfirmDialog: async () => true,
    fetchDataWithAuth: () => { started.resolve(); return d.promise; }
  });
  const input = element('20');
  p.elements.set('timeout', input);
  p.run('originalSettings = { timeout: "10" }');
  const resetting = p.run('resetSetting("timeout")');
  await started.promise;
  input.value = '30';
  d.resolve({ value: '5' });
  await resetting;
  assert.equal(input.value, '30');
  assert.equal(p.run('originalSettings.timeout'), '5');
});

function statsPage() {
  const requests = [];
  const p = page('stats.js', {
    query: 'a', rendered: [], loadingCount: 0, errorCount: 0,
    fetchDataWithAuth() { const d = deferred(); requests.push(d); return d.promise; }
  });
  p.run(`
    buildStatsRequestParams = () => new URLSearchParams({ query });
    renderStatsLoading = () => { loadingCount++; };
    renderStatsError = () => { errorCount++; };
    renderStatsTable = () => { rendered.push(statsData.stats[0]?.model); };
    populateStatsComboboxOptions = applyDefaultSorting = updateStatsCount = updateRpmHeader = () => {};
  `);
  return { ...p, requests };
}

test('statistics ignore stale responses after filters change', async () => {
  const p = statsPage();
  const first = p.run('loadStats()');
  p.run('query = "b"');
  const second = p.run('loadStats()');
  p.requests[1].resolve({ stats: [{ model: 'new' }] });
  await second;
  p.requests[0].resolve({ stats: [{ model: 'old' }] });
  await first;
  assert.equal(p.run('statsData.stats[0].model'), 'new');
  assert.equal(p.context.rendered.length, 1);
});

test('statistics retain the table on background refresh failure and skip overlapping polls', async () => {
  const p = statsPage();
  const first = p.run('loadStats()');
  p.requests[0].resolve({ stats: [{ model: 'kept' }] });
  await first;
  const refresh = p.run('loadStats(true)');
  await p.run('loadStats(true)');
  assert.equal(p.requests.length, 2);
  p.requests[1].reject(new Error('offline'));
  await refresh;
  assert.equal(p.run('statsData.stats[0].model'), 'kept');
  assert.equal(p.context.loadingCount, 1);
  assert.equal(p.context.errorCount, 0);
});

test('homepage date changes reuse cumulative cache; explicit refresh opts out', async () => {
  const urls = [];
  const p = page('index.js', {
    fetchDataWithAuth: async url => { urls.push(url); return {}; }
  });
  p.run('updateStatsDisplay = () => {}');
  await p.run('loadStats()');
  p.run('currentTimeRange = "yesterday"');
  await p.run('loadStats()');
  await p.run('loadStats(true)');
  assert.equal(urls[0].includes('refresh_cumulative'), false);
  assert.equal(urls[1].includes('refresh_cumulative'), false);
  assert.match(urls[2], /refresh_cumulative=1/);
});

function trendPage() {
  const requests = [];
  const p = page('trend.js', {
    query: 'a', loadingCount: 0, errorCount: 0,
    fetchAPIWithAuthRaw() { const d = deferred(); requests.push(d); return d.promise; }
  });
  p.run(`
    buildTrendRequestParams = () => new URLSearchParams({ query });
    getTrendRangeHours = () => 24;
    renderTrendLoading = () => { loadingCount++; };
    renderTrendError = () => { errorCount++; };
    updateChannelFilter = renderChart = () => {};
  `);
  return { ...p, requests };
}

function metrics(data) {
  return { payload: { success: true, data }, res: { headers: { get: () => '' } } };
}

test('trend filters preserve saved channel selections through an empty range', async () => {
  const p = trendPage();
  p.run('visibleChannels.add("chosen")');
  const first = p.run('loadData()');
  p.requests[0].resolve(metrics([]));
  await first;
  assert.equal(p.run('visibleChannels.has("chosen")'), true);
  const next = p.run('loadData()');
  p.requests[1].resolve(metrics([{ channels: { chosen: { success: 1 } } }]));
  await next;
  assert.equal(p.run('visibleChannels.has("chosen") && hasChannelData("chosen", trendData)'), true);
});

test('trend keeps current data when an older response arrives or refresh fails', async () => {
  const p = trendPage();
  const first = p.run('loadData()');
  p.run('query = "b"');
  const second = p.run('loadData()');
  p.requests[1].resolve(metrics([{ ts: 'new', channels: {} }]));
  await second;
  p.requests[0].resolve(metrics([{ ts: 'old', channels: {} }]));
  await first;
  assert.equal(p.run('trendData[0].ts'), 'new');
  const refresh = p.run('loadData(true)');
  p.requests[2].reject(new Error('offline'));
  await refresh;
  assert.equal(p.run('trendData[0].ts'), 'new');
  assert.equal(p.context.loadingCount, 2);
  assert.equal(p.context.errorCount, 0);
});

test('shared auto refresh skips hidden pages, dialogs, and pending requests', async () => {
  const ticks = [];
  const events = {};
  const p = page('ui.js', {
    setInterval(fn) { ticks.push(fn); return ticks.length; }, clearInterval() {},
    matchMedia: () => ({ matches: false, addEventListener() {} }),
    getComputedStyle: () => ({ getPropertyValue: () => '' })
  });
  p.context.isAPITokenRole = () => false;
  p.context.fetchDataWithAuth = async () => [{ key: 'auto_refresh_interval_seconds', value: '5' }];
  p.context.document.addEventListener = (name, fn) => { events[name] = fn; };
  let calls = 0;
  const d = deferred();
  p.context.refresh = p.context.createAutoRefresh({ load: () => { calls++; return d.promise; } });
  await p.context.refresh.init();
  const tick = ticks.at(-1);
  p.context.document.hidden = true;
  await tick();
  assert.equal(calls, 0);
  p.context.document.hidden = false;
  p.context.document.querySelector = () => element();
  await tick();
  assert.equal(calls, 0);
  p.context.document.querySelector = () => null;
  const running = tick();
  await tick();
  events.visibilitychange();
  assert.equal(calls, 1);
  d.resolve();
  await running;
  await tick();
  assert.equal(calls, 2);
  p.context.refresh.stop();
});
