const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const assert = require('node:assert/strict');
const vm = require('node:vm');

const logsSource = fs.readFileSync(path.join(__dirname, 'logs.js'), 'utf8') + `
globalThis.__logsCancelRequestTest = {
  buildActiveRequestInfoContent,
  cancelActiveRequest,
  shouldShowActiveRequestCancel,
  renderActiveRequests,
  setThreshold: (value) => { activeRequestCancelThresholdSeconds = value; }
};
`;

const LABELS = {
  'logs.skipChannel': 'Skip current channel',
  'logs.skipChannelTitle': 'Skip upstream attempt',
  'logs.skipChannelSwitching': 'Switching...',
  'logs.skipChannelFailed': 'Failed to skip current channel',
  'logs.cancelRequest': 'Cancel request',
  'logs.cancelRequestTitle': 'Cancel the whole request',
  'logs.cancelRequestConfirm': 'Cancel this request?',
  'logs.cancelRequestCanceling': 'Canceling...',
  'logs.cancelRequestFailed': 'Failed to cancel request'
};

function loadLogsHelpers(fetchAPIWithAuth, extraWindow = {}) {
  const context = {
    URLSearchParams,
    console,
    clearTimeout,
    setTimeout,
    escapeHtml: (value) => String(value),
    fetchAPIWithAuth,
    localStorage: {
      getItem: () => null,
      removeItem() {},
      setItem() {}
    },
    window: {
      addEventListener() {},
      initPageBootstrap() {},
      t: (key) => LABELS[key] || key,
      ...extraWindow
    },
    document: {
      addEventListener() {},
      getElementById: () => null,
      querySelectorAll: () => []
    }
  };
  vm.runInNewContext(logsSource, context, { filename: 'logs.js' });
  return context.__logsCancelRequestTest;
}

function makeButton(dataset) {
  const classNames = new Set();
  const attributes = new Map();
  return {
    button: {
      dataset,
      disabled: false,
      isConnected: true,
      textContent: 'Cancel request',
      classList: {
        add: (name) => classNames.add(name),
        remove: (name) => classNames.delete(name)
      },
      setAttribute: (name, value) => attributes.set(name, value),
      removeAttribute: (name) => attributes.delete(name)
    },
    classNames,
    attributes
  };
}

test('cancel button appears only once the current-channel elapsed time crosses the threshold', () => {
  const { buildActiveRequestInfoContent } = loadLogsHelpers(async () => ({ success: true }));

  const below = buildActiveRequestInfoContent({ id: 7, start_time: 1700000000000 }, 999);
  assert.doesNotMatch(below, /cancel-active-request-btn/);

  const above = buildActiveRequestInfoContent({ id: 7, start_time: 1700000000000 }, 1000);
  assert.match(above, /cancel-active-request-btn/);
  assert.match(above, /data-active-request-id="7"/);
  assert.match(above, /data-start-time="1700000000000"/);
  assert.match(above, /Cancel request/);
});

test('threshold 0 always shows the cancel button', () => {
  const helpers = loadLogsHelpers(async () => ({ success: true }));
  helpers.setThreshold(0);

  assert.equal(helpers.shouldShowActiveRequestCancel(1), true);
  assert.equal(helpers.shouldShowActiveRequestCancel(0), true);
  // 无有效耗时（尚未拿到 start_time）时不显示，避免出现无意义按钮
  assert.equal(helpers.shouldShowActiveRequestCancel(NaN), false);
});

test('cancel_requested keeps the button visible but disabled across re-renders', () => {
  const { buildActiveRequestInfoContent } = loadLogsHelpers(async () => ({ success: true }));

  // 即使耗时未到阈值，取消中的请求仍需展示禁用态按钮（该列每轮轮询整体重绘）
  const html = buildActiveRequestInfoContent({ id: 7, cancel_requested: true }, 5);
  assert.match(html, /cancel-active-request-btn is-pending/);
  assert.match(html, /disabled aria-disabled="true"/);
  assert.match(html, /Canceling\.\.\./);
});

test('cancel button posts start_time and stays pending after acceptance', async () => {
  let request;
  const { cancelActiveRequest } = loadLogsHelpers(async (url, options) => {
    request = { url, options };
    return { success: true };
  }, { showConfirmDialog: async () => true });

  const { button, classNames, attributes } = makeButton({ activeRequestId: '42', startTime: '1700000000000' });
  await cancelActiveRequest(button);

  assert.equal(request.url, '/admin/active-requests/42/cancel');
  assert.equal(request.options.method, 'POST');
  assert.equal(request.options.headers['Content-Type'], 'application/json');
  assert.equal(request.options.body, '{"start_time":1700000000000}');
  assert.equal(button.disabled, true);
  assert.equal(button.textContent, 'Canceling...');
  assert.equal(classNames.has('is-pending'), true);
  assert.equal(attributes.get('aria-disabled'), 'true');
});

test('declining the confirmation does not send a request', async () => {
  let called = false;
  const { cancelActiveRequest } = loadLogsHelpers(async () => {
    called = true;
    return { success: true };
  }, { showConfirmDialog: async () => false });

  const { button } = makeButton({ activeRequestId: '42' });
  await cancelActiveRequest(button);

  assert.equal(called, false);
  assert.equal(button.disabled, false);
});

// 回归：窄屏行没有 .logs-col-message，必须回退到 .active-request-info-slot，
// 否则该行创建后不再刷新，取消按钮永远不出现。
test('narrow layout rows keep refreshing the info cell', () => {
  const html = fs.readFileSync(path.join(__dirname, 'logs.js'), 'utf8');

  const narrowTemplate = html.slice(html.indexOf('if (totalCols < 8)'), html.indexOf('} else {', html.indexOf('if (totalCols < 8)')));
  assert.match(narrowTemplate, /active-request-info-slot/, '窄屏模板缺少可刷新的信息容器');

  const updateBlock = html.slice(html.indexOf('const msgCell ='), html.indexOf('\n', html.indexOf('const msgCell =')));
  assert.match(updateBlock, /logs-col-message/);
  assert.match(updateBlock, /active-request-info-slot/, '更新路径没有回退到窄屏容器');
});

test('cancel failure restores the button', async () => {
  let shown;
  const { cancelActiveRequest } = loadLogsHelpers(async () => ({ success: false, error: 'boom' }), {
    showConfirmDialog: async () => true,
    showError: (message) => { shown = message; }
  });

  const { button, classNames } = makeButton({ activeRequestId: '42' });
  await cancelActiveRequest(button);

  assert.equal(shown, 'boom');
  assert.equal(button.disabled, false);
  assert.equal(button.textContent, 'Cancel request');
  assert.equal(classNames.has('is-pending'), false);
});
