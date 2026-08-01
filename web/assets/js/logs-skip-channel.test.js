const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const assert = require('node:assert/strict');
const vm = require('node:vm');

const logsSource = fs.readFileSync(path.join(__dirname, 'logs.js'), 'utf8') + `
globalThis.__logsSkipChannelTest = {
  buildActiveRequestInfoContent,
  skipActiveRequestChannel
};
`;

function loadLogsHelpers(fetchAPIWithAuth) {
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
      t: (key) => ({
        'logs.skipChannel': 'Skip current channel',
        'logs.skipChannelTitle': 'Skip upstream attempt',
        'logs.skipChannelSwitching': 'Switching...',
        'logs.skipChannelFailed': 'Failed to skip current channel'
      })[key] || key
    },
    document: {
      addEventListener() {},
      getElementById: () => null,
      querySelectorAll: () => []
    }
  };
  vm.runInNewContext(logsSource, context, { filename: 'logs.js' });
  return context.__logsSkipChannelTest;
}

test('active request skip button carries the request and attempt IDs', () => {
  const { buildActiveRequestInfoContent } = loadLogsHelpers(async () => ({ success: true }));

  const html = buildActiveRequestInfoContent({
    id: 42,
    attempt_id: 99,
    can_skip: true,
    bytes_received: 0
  });

  assert.match(html, /skip-active-channel-btn/);
  assert.match(html, /data-active-request-id="42"/);
  assert.match(html, /data-attempt-id="99"/);
  assert.match(html, /Skip current channel/);

  const unavailable = buildActiveRequestInfoContent({ id: 42, attempt_id: 99, can_skip: false });
  assert.doesNotMatch(unavailable, /skip-active-channel-btn/);
});

test('skip button posts the current attempt ID and remains pending after acceptance', async () => {
  let request;
  const { skipActiveRequestChannel } = loadLogsHelpers(async (url, options) => {
    request = { url, options };
    return { success: true };
  });
  const classNames = new Set();
  const attributes = new Map();
  const button = {
    dataset: { activeRequestId: '42', attemptId: '99' },
    disabled: false,
    isConnected: true,
    textContent: 'Skip current channel',
    classList: {
      add: (name) => classNames.add(name),
      remove: (name) => classNames.delete(name)
    },
    setAttribute: (name, value) => attributes.set(name, value),
    removeAttribute: (name) => attributes.delete(name)
  };

  await skipActiveRequestChannel(button);

  assert.equal(request.url, '/admin/active-requests/42/skip');
  assert.equal(request.options.method, 'POST');
  assert.equal(request.options.headers['Content-Type'], 'application/json');
  assert.equal(request.options.body, '{"attempt_id":99}');
  assert.equal(button.disabled, true);
  assert.equal(button.textContent, 'Switching...');
  assert.equal(classNames.has('is-pending'), true);
  assert.equal(attributes.get('aria-disabled'), 'true');
});
