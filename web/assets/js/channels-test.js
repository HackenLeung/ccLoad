function getChannelTestModelNames(channel, protocolName) {
  const protocol = String(protocolName || channel?.channel_type || 'anthropic').trim().toLowerCase();
  const seen = new Set();
  const names = [];
  (channel?.models || []).forEach(entry => {
    if (typeof window !== 'undefined' && window.isGlobalModelDisabledName?.(entry?.model || entry)) return;
    let candidates;
    if (typeof entry === 'string') {
      candidates = [entry];
    } else if (entry?.redirect_enabled) {
      candidates = [...(entry.protocol_aliases?.[protocol] || []), entry.model];
    } else {
      candidates = [entry?.model];
    }
    candidates.forEach(candidate => {
      const modelName = String(candidate || '').trim();
      if (typeof window !== 'undefined' && window.isGlobalModelDisabledName?.(modelName)) return;
      const key = modelName.toLowerCase();
      if (!modelName || seen.has(key)) return;
      seen.add(key);
      names.push(modelName);
    });
  });
  return names;
}

function renderChannelTestModelOptions(channel, protocolName) {
  const modelSelect = document.getElementById('testModelSelect');
  if (!modelSelect) return;
  const previousValue = modelSelect.value;
  const modelNames = getChannelTestModelNames(channel, protocolName);
  modelSelect.innerHTML = '';
  modelNames.forEach(modelName => {
    const option = document.createElement('option');
    option.value = modelName;
    option.textContent = modelName;
    modelSelect.appendChild(option);
  });
  if (modelNames.includes(previousValue)) modelSelect.value = previousValue;
}

async function testChannel(id, name) {
  const channel = channels.find(c => c.id === id);
  if (!channel) return;

  testingChannelId = id;
  document.getElementById('testChannelName').textContent = name;

  const models = channel.models || [];
  if (models.length === 0) {
    if (window.showError) window.showError(window.t('channels.test.noModels') || 'No models configured for this channel');
    return;
  }

  let apiKeys = [];
  try {
    apiKeys = (await fetchDataWithAuth(`/admin/channels/${id}/keys`)) || [];
  } catch (e) {
    console.error('Failed to fetch API keys', e);
  }

  const keys = apiKeys.map(k => k.api_key || k);
  const keySelect = document.getElementById('testKeySelect');
  const keySelectGroup = document.getElementById('testKeySelectGroup');
  const batchTestBtn = document.getElementById('batchTestBtn');

  if (keys.length > 1) {
    keySelectGroup.classList.remove('hidden');
    batchTestBtn.classList.remove('hidden');

    keySelect.innerHTML = '';
    const maxKeys = Math.min(keys.length, 10);
    for (let i = 0; i < maxKeys; i++) {
      const option = document.createElement('option');
      option.value = i;
      option.textContent = `Key ${i + 1}: ${maskKey(keys[i])}`;
      keySelect.appendChild(option);
    }

    if (keys.length > 10) {
      const hintOption = document.createElement('option');
      hintOption.disabled = true;
      hintOption.textContent = window.t('channels.test.moreKeysHint', { count: keys.length - 10 });
      keySelect.appendChild(hintOption);
    }
  } else {
    keySelectGroup.classList.add('hidden');
    batchTestBtn.classList.add('hidden');
  }

  resetTestModal();

  const channelType = channel.channel_type || 'anthropic';
  await window.ChannelTypeManager.renderChannelTypeSelect('testChannelType', channelType);
  const channelTypeSelect = document.getElementById('testChannelType');
  renderChannelTestModelOptions(channel, channelTypeSelect.value || channelType);
  channelTypeSelect.onchange = () => renderChannelTestModelOptions(channel, channelTypeSelect.value);

  document.getElementById('testModal').classList.add('show');
}

function closeTestModal() {
  document.getElementById('testModal').classList.remove('show');
  testingChannelId = null;
}

function resetTestModal() {
  document.getElementById('testProgress').classList.remove('show');
  document.getElementById('batchTestProgress').classList.add('hidden');
  document.getElementById('testResult').classList.remove('show', 'success', 'error');
  document.getElementById('testUpstreamDetailBtn')?.classList.add('hidden');
  document.getElementById('runTestBtn').disabled = false;
  document.getElementById('batchTestBtn').disabled = false;
  document.getElementById('testContentInput').value = defaultTestContent;
  document.getElementById('testChannelType').value = 'anthropic';
  document.getElementById('testConcurrency').value = '10';
}

async function runChannelTest() {
  if (!testingChannelId) return;

  const modelSelect = document.getElementById('testModelSelect');
  const contentInput = document.getElementById('testContentInput');
  const channelTypeSelect = document.getElementById('testChannelType');
  const keySelect = document.getElementById('testKeySelect');
  const streamCheckbox = document.getElementById('testStreamEnabled');
  const keySelectGroup = document.getElementById('testKeySelectGroup');
  const selectedModel = modelSelect.value;
  const testContent = contentInput.value.trim() || defaultTestContent;
  const channelType = channelTypeSelect.value;
  const streamEnabled = streamCheckbox.checked;

  if (!selectedModel) {
    if (window.showError) window.showError(window.t('channels.test.selectModelRequired'));
    return;
  }

  document.getElementById('testProgress').classList.add('show');
  document.getElementById('testResult').classList.remove('show');
  document.getElementById('runTestBtn').disabled = true;

  try {
    const testRequest = {
      model: selectedModel,
      stream: streamEnabled,
      content: testContent,
      channel_type: channelType
    };

    if (keySelect && keySelectGroup && !keySelectGroup.classList.contains('hidden')) {
      testRequest.key_index = parseInt(keySelect.value) || 0;
    }

    const testResult = await fetchDataWithAuth(`/admin/channels/${testingChannelId}/test`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(testRequest)
    });
    displayTestResult(testResult || { success: false, error: window.t('error.emptyResponse') });
  } catch (e) {
    console.error('Test failed', e);

    displayTestResult({
      success: false,
      error: window.t('channels.test.requestFailed') + e.message
    });
  } finally {
    document.getElementById('testProgress').classList.remove('show');
    document.getElementById('runTestBtn').disabled = false;

    await loadChannels(filters.channelType);
  }
}

async function runBatchTest() {
  if (!testingChannelId) return;

  const channel = channels.find(c => c.id === testingChannelId);
  if (!channel) return;

  let apiKeys = [];
  try {
    apiKeys = (await fetchDataWithAuth(`/admin/channels/${testingChannelId}/keys`)) || [];
  } catch (e) {
    console.error('Failed to fetch API keys', e);
  }

  const keys = apiKeys.map(k => k.api_key || k);
  if (keys.length === 0) {
    if (window.showError) window.showError(window.t('channels.test.noApiKey'));
    return;
  }

  const modelSelect = document.getElementById('testModelSelect');
  const contentInput = document.getElementById('testContentInput');
  const channelTypeSelect = document.getElementById('testChannelType');
  const streamCheckbox = document.getElementById('testStreamEnabled');
  const concurrencyInput = document.getElementById('testConcurrency');

  const selectedModel = modelSelect.value;
  const testContent = contentInput.value.trim() || defaultTestContent;
  const channelType = channelTypeSelect.value;
  const streamEnabled = streamCheckbox.checked;
  const concurrency = Math.max(1, Math.min(50, parseInt(concurrencyInput.value) || 10));

  if (!selectedModel) {
    if (window.showError) window.showError(window.t('channels.test.selectModelRequired'));
    return;
  }

  document.getElementById('runTestBtn').disabled = true;
  document.getElementById('batchTestBtn').disabled = true;

  const progressDiv = document.getElementById('batchTestProgress');
  const counterSpan = document.getElementById('batchTestCounter');
  const progressBar = document.getElementById('batchTestProgressBar');
  const statusDiv = document.getElementById('batchTestStatus');

  progressDiv.classList.remove('hidden');
  document.getElementById('testResult').classList.remove('show');

  let successCount = 0;
  let failedCount = 0;
  const failedKeys = [];
  let completedCount = 0;

  const updateProgress = () => {
    const progress = (completedCount / keys.length * 100).toFixed(0);
    counterSpan.textContent = `${completedCount} / ${keys.length}`;
    progressBar.style.width = `${progress}%`;
    statusDiv.textContent = window.t('channels.test.progressStatus', { completed: completedCount, total: keys.length, concurrency });
  };

  const testSingleKey = async (keyIndex) => {
    try {
      const testRequest = {
        model: selectedModel,
        stream: streamEnabled,
        content: testContent,
        channel_type: channelType,
        key_index: keyIndex
      };

      const testResult = await fetchDataWithAuth(`/admin/channels/${testingChannelId}/test`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(testRequest)
      });

      if (testResult.success) {
        successCount++;
      } else {
        failedCount++;
        failedKeys.push({ index: keyIndex, key: maskKey(keys[keyIndex]), error: testResult.error });
      }
    } catch (e) {
      failedCount++;
      failedKeys.push({ index: keyIndex, key: maskKey(keys[keyIndex]), error: e.message });
    } finally {
      completedCount++;
      updateProgress();
    }
  };

  const batches = [];
  for (let i = 0; i < keys.length; i += concurrency) {
    const batchIndexes = [];
    for (let j = i; j < Math.min(i + concurrency, keys.length); j++) {
      batchIndexes.push(j);
    }
    batches.push(batchIndexes);
  }

  updateProgress();

  for (const batch of batches) {
    const batchPromises = batch.map(keyIndex => testSingleKey(keyIndex));
    await Promise.all(batchPromises);
  }

  displayBatchTestResult(successCount, failedCount, keys.length, failedKeys);

  document.getElementById('runTestBtn').disabled = false;
  document.getElementById('batchTestBtn').disabled = false;

  await loadChannels(filters.channelType);
}

function displayBatchTestResult(successCount, failedCount, totalCount, failedKeys) {
  const testResultDiv = document.getElementById('testResult');
  const contentDiv = document.getElementById('testResultContent');
  const detailsDiv = document.getElementById('testResultDetails');
  const statusDiv = document.getElementById('batchTestStatus');

  testResultDiv.classList.remove('success', 'error');
  testResultDiv.classList.add('show');

  statusDiv.textContent = window.t('channels.test.completed', { success: successCount, failed: failedCount });

  // 使用模板渲染头部
  const renderHeader = (icon, message) => {
    const header = TemplateEngine.render('tpl-test-result-header', { icon, message });
    contentDiv.innerHTML = '';
    if (header) contentDiv.appendChild(header);
  };

  // 构建失败详情列表
  const buildFailDetails = () => {
    const items = failedKeys.map(({ index, key, error }) => {
      const item = TemplateEngine.render('tpl-batch-fail-item', {
        keyNum: index + 1,
        keyMask: key,
        error: escapeHtml(error)
      });
      return item ? item.outerHTML : '';
    }).join('');
    return `<ul class="batch-test-fail-list">${items}</ul>`;
  };

  if (failedCount === 0) {
    testResultDiv.classList.add('success');
    renderHeader('✅', window.t('channels.test.batchAllSuccess', { count: totalCount }));
    detailsDiv.innerHTML = '';
  } else if (successCount === 0) {
    testResultDiv.classList.add('error');
    renderHeader('❌', window.t('channels.test.batchAllFailed', { count: totalCount }));
    detailsDiv.innerHTML = `<h4 class="batch-test-fail-title">${window.t('channels.test.failDetails')}</h4>${buildFailDetails()}<p class="batch-test-fail-note">${window.t('channels.test.failedKeysAutoCooldown')}</p>`;
  } else {
    testResultDiv.classList.add('success');
    renderHeader('⚠️', window.t('channels.test.batchPartial', { success: successCount, failed: failedCount }));
    detailsDiv.innerHTML = `<p class="batch-test-success-note">✅ ${window.t('channels.test.keysAvailable', { count: successCount })}</p><h4 class="batch-test-fail-title">${window.t('channels.test.failDetails')}</h4>${buildFailDetails()}<p class="batch-test-fail-note">${window.t('channels.test.failedKeysAutoCooldown')}</p>`;
  }
}

function displayTestResult(result) {
  const testResultDiv = document.getElementById('testResult');
  const contentDiv = document.getElementById('testResultContent');
  const detailsDiv = document.getElementById('testResultDetails');
  const upstreamDetailBtn = document.getElementById('testUpstreamDetailBtn');

  testResultDiv.classList.remove('success', 'error');
  testResultDiv.classList.add('show');

  // 使用模板渲染头部
  const renderHeader = (icon, message) => {
    const header = TemplateEngine.render('tpl-test-result-header', { icon, message });
    contentDiv.innerHTML = '';
    if (header) contentDiv.appendChild(header);
  };

  // 渲染响应区块
  const renderResponseSection = (title, content, display = 'none', hasToggle = true) => {
    const contentId = `response-${Date.now()}-${Math.random().toString(36).substr(2, 9)}`;
    const toggleBtn = hasToggle
      ? `<button type="button" class="toggle-btn" data-action="toggle-response" data-response-target="${contentId}">${window.t('channels.test.toggleResponse')}</button>`
      : '';
    const section = TemplateEngine.render('tpl-response-section', {
      title,
      toggleBtn,
      contentId,
      display,
      content: escapeHtml(content)
    });
    return section ? section.outerHTML : '';
  };

  if (result.success) {
    testResultDiv.classList.add('success');
    renderHeader('✅', result.message || window.t('channels.test.apiTestSuccess'));

    let details = `${window.t('channels.test.responseTime')}: ${result.duration_ms}ms`;
    if (result.status_code) {
      details += ` | ${window.t('channels.test.statusCode')}: ${result.status_code}`;
    }

    if (result.response_text) {
      details += renderResponseSection(window.t('channels.test.apiResponseContent'), result.response_text, 'block', false);
    }

    if (result.api_response) {
      details += renderResponseSection(window.t('channels.test.fullApiResponse'), JSON.stringify(result.api_response, null, 2));
    } else if (result.raw_response) {
      details += renderResponseSection(window.t('channels.test.rawResponse'), result.raw_response);
    }

    detailsDiv.innerHTML = details;
  } else {
    testResultDiv.classList.add('error');
    renderHeader('❌', window.t('channels.msg.testFailed'));

    // [FIX] Escape result.error to prevent XSS
    let details = escapeHtml(result.error || window.t('error.unknown'));
    if (result.duration_ms) {
      details += `<br>${window.t('channels.test.responseTime')}: ${result.duration_ms}ms`;
    }
    if (result.status_code) {
      details += ` | ${window.t('channels.test.statusCode')}: ${result.status_code}`;
    }

    if (result.api_error) {
      details += renderResponseSection(window.t('channels.test.fullErrorResponse'), JSON.stringify(result.api_error, null, 2), 'block');
    }
    if (typeof result.raw_response !== 'undefined') {
      details += renderResponseSection(window.t('channels.test.rawErrorResponse'), result.raw_response || window.t('channels.test.noResponseBody'), 'block');
    }
    if (result.response_headers) {
      details += renderResponseSection(window.t('channels.test.responseHeaders'), JSON.stringify(result.response_headers, null, 2), 'block');
    }

    detailsDiv.innerHTML = details;
  }

  // 缓存上游详情数据供 Modal 使用
  window._lastTestUpstreamData = result.upstream_request_url ? {
    method: 'POST',
    url: result.upstream_request_url,
    requestHeaders: result.upstream_request_headers,
    requestBody: result.upstream_request_body,
    statusCode: result.status_code,
    responseHeaders: result.response_headers,
    responseBody: result.upstream_response_body || result.raw_response
  } : null;
  upstreamDetailBtn?.classList.toggle('hidden', !window._lastTestUpstreamData);
}

if (typeof module !== 'undefined' && module.exports) {
  const test = require('node:test');
  const assert = require('node:assert/strict');
  const fs = require('node:fs');
  const vm = require('node:vm');

  function replaceGlobals(values) {
    const previous = new Map();
    for (const [key, value] of Object.entries(values)) {
      previous.set(key, Object.getOwnPropertyDescriptor(global, key));
      Object.defineProperty(global, key, {
        configurable: true,
        enumerable: true,
        writable: true,
        value
      });
    }
    return () => {
      for (const [key, descriptor] of previous) {
        if (descriptor) Object.defineProperty(global, key, descriptor);
        else delete global[key];
      }
    };
  }

  function loadChannelsData({ role }) {
    const urls = [];
    const storage = {
      getItem: (key) => key === 'ccload_web_role' ? role : null
    };
    const restoreGlobals = replaceGlobals({
      window: {
        WebAuth: {
          isAPITokenRole: (currentStorage) => currentStorage === storage && role === 'api_token'
        },
        t: (key) => key,
        showError: () => {}
      },
      localStorage: storage,
      filters: {
        search: '',
        searchExact: false,
        status: 'all',
        model: 'all',
        modelExact: false
      },
      channelsReadURL: (adminPath, dashboardPath) => (
        global.window.WebAuth.isAPITokenRole(global.localStorage) ? dashboardPath : adminPath
      ),
      channels: [],
      channelsTotalCount: 0,
      channelsTotalPages: 1,
      channelsCurrentPage: 1,
      channelsPageSize: 20,
      allAvailableChannelNames: [],
      allAvailableModels: [],
      channelStatsRange: 'today',
      channelStatsById: {},
      fetchAPIWithAuth: async (url) => {
        urls.push(url);
        return { success: true, data: [], count: 0 };
      },
      fetchDataWithAuth: async (url) => {
        urls.push(url);
        if (url.includes('filter-options')) return { channel_names: [], models: [] };
        return { stats: [], channel_health: {} };
      },
      filterChannels: () => {},
      updateChannelsPagination: () => {},
      updateModelOptions: () => {},
      updateChannelNameOptions: () => {}
    });
    const modulePath = require.resolve('./channels-data.js');
    delete require.cache[modulePath];

    try {
      return {
        mod: require('./channels-data.js'),
        urls,
        restore() {
          delete require.cache[modulePath];
          restoreGlobals();
        }
      };
    } catch (error) {
      restoreGlobals();
      throw error;
    }
  }

  test('api token channels use dashboard read endpoints', async () => {
    const runtime = loadChannelsData({ role: 'api_token' });
    try {
      await runtime.mod.loadChannels('all');
      await runtime.mod.loadChannelsFilterOptions('all', 'all');
      await runtime.mod.loadChannelStats('today');
      assert.match(runtime.urls[0], /^\/dashboard\/channels\?/);
      assert.match(runtime.urls[1], /^\/dashboard\/channels\/filter-options\?/);
      assert.match(runtime.urls[2], /^\/dashboard\/stats\?/);
      assert.ok(runtime.urls.every((url) => !url.includes('auth_token_id=')));
    } finally {
      runtime.restore();
    }
  });

  test('admin channels keep admin endpoints', async () => {
    const runtime = loadChannelsData({ role: 'admin' });
    try {
      await runtime.mod.loadChannels('all');
      await runtime.mod.loadChannelsFilterOptions('all', 'all');
      await runtime.mod.loadChannelStats('today');
      assert.match(runtime.urls[0], /^\/admin\/channels\?/);
      assert.match(runtime.urls[1], /^\/admin\/channels\/filter-options\?/);
      assert.match(runtime.urls[2], /^\/admin\/stats\?/);
    } finally {
      runtime.restore();
    }
  });

  function priorityInput(channelId = '1', value = '2') {
    const classList = {
      add: () => {},
      remove: () => {},
      toggle: () => {}
    };
    const editor = {
      classList,
      querySelectorAll: () => [input]
    };
    const input = {
      dataset: { channelId, originalPriority: '1' },
      value,
      classList,
      closest: (selector) => {
        if (selector === '.ch-priority-input') return input;
        if (selector === '.ch-priority-editor-wrap') return editor;
        return null;
      }
    };
    return input;
  }

  function loadTokenPriorityWorkflow() {
    const listeners = {};
    const requests = [];
    const storage = {
      getItem: (key) => key === 'ccload_web_role' ? 'api_token' : null
    };
    const container = {
      dataset: {},
      addEventListener: (type, listener) => {
        listeners[type] = listener;
      }
    };
    const context = vm.createContext({
      window: {
        ChannelProtocolConfig: {},
        WebAuth: { isAPITokenRole: (currentStorage) => currentStorage === storage },
        t: (key) => key,
        showSuccess: () => {},
        showError: () => {}
      },
      localStorage: storage,
      document: {
        getElementById: (id) => id === 'channels-container' ? container : null,
        addEventListener: () => {},
        querySelectorAll: () => []
      },
      fetchDataWithAuth: async (url, options) => {
        requests.push({ url, options });
        return {};
      },
      setTimeout: (callback) => {
        callback();
        return 1;
      },
      clearTimeout: () => {},
      console
    });
    const sourceDir = __dirname;
    vm.runInContext(fs.readFileSync(`${sourceDir}/channels-state.js`, 'utf8'), context);
    vm.runInContext(fs.readFileSync(`${sourceDir}/channels-render.js`, 'utf8'), context);
    context.initChannelEventDelegation();

    return {
      dispatch(type, event) {
        listeners[type](event);
      },
      requests
    };
  }

  test('api token priority events never submit channel writes', async () => {
    const runtime = loadTokenPriorityWorkflow();
    const inputEvent = priorityInput('1', '2');
    const enterEvent = priorityInput('2', '3');
    const focusoutEvent = priorityInput('3', '4');
    const forgedInput = priorityInput('999', '77');

    runtime.dispatch('input', { target: inputEvent });
    runtime.dispatch('keydown', { target: enterEvent, key: 'Enter', preventDefault: () => {} });
    runtime.dispatch('focusout', { target: focusoutEvent });
    runtime.dispatch('input', { target: forgedInput });
    await Promise.resolve();

    assert.deepEqual(runtime.requests, []);
  });

  function loadChannelEditor({ channelType = 'codex', protocolTransforms = [], rows = [], fetchResult } = {}) {
    const tableBody = {
      dataset: {},
      innerHTML: '',
      addEventListener: () => {},
      appendChild: () => {}
    };
    const redirectCount = { textContent: '' };
    const elements = new Map();
    const body = {
      appendChild(element) {
        elements.set(element.id, element);
      }
    };
    const restoreGlobals = replaceGlobals({
      window: {
        ChannelProtocolConfig: {
          normalizeProtocolTransformsForChannel: (_type, values) => values
        },
        t: (key) => key,
        showSuccess: () => {},
        showWarning: () => {}
      },
      document: {
        querySelector: (selector) => selector === 'input[name="channelType"]:checked'
          ? { value: channelType }
          : null,
		querySelectorAll: (selector) => selector === 'input[name="protocolTransform"]:checked'
		  ? protocolTransforms.map(value => ({ value }))
		  : [],
        getElementById: (id) => {
          if (id === 'redirectTableBody') return tableBody;
          if (id === 'redirectCount') return redirectCount;
		  return elements.get(id) || null;
        },
		createElement: (tagName) => ({
		  tagName,
		  id: '',
		  value: '',
		  children: [],
		  appendChild(child) { this.children.push(child); }
		}),
		body,
        createDocumentFragment: () => ({ appendChild: () => {} })
      },
      redirectTableData: rows.map((row) => ({ ...row })),
      selectedModelIndices: new Set(),
      currentModelFilter: '',
      TemplateEngine: {
        render: (templateName) => templateName === 'tpl-redirect-row'
          ? { querySelector: () => null }
          : null
      },
      fetchDataWithAuth: async () => typeof fetchResult === 'function' ? fetchResult() : fetchResult,
      syncChannelEditorTableSizing: () => {},
      markChannelFormDirty: () => {},
      alert: () => {}
    });
    const modulePath = require.resolve('./channels-modals.js');
    delete require.cache[modulePath];

    try {
      return {
        mod: require('./channels-modals.js'),
        restore() {
          delete require.cache[modulePath];
          restoreGlobals();
        }
      };
    } catch (error) {
      restoreGlobals();
      throw error;
    }
  }

  function submittedModelRows() {
    return global.redirectTableData.map(({ model, redirect_enabled, protocol_aliases }) => ({
      model,
      redirect_enabled: !!redirect_enabled,
      protocol_aliases: protocol_aliases || {}
    }));
  }

  function loadChannelProxyControl() {
    const checkbox = {
      checked: false,
      dataset: {},
      listeners: {},
      addEventListener(type, listener) { this.listeners[type] = listener; }
    };
    const input = {
      value: '',
      disabled: false,
      attributes: {},
      setAttribute(name, value) { this.attributes[name] = value; }
    };
    const restoreGlobals = replaceGlobals({
      window: { ChannelProtocolConfig: {} },
      document: {
        getElementById(id) {
          if (id === 'channelUseProxy') return checkbox;
          if (id === 'channelProxyURL') return input;
          return null;
        }
      }
    });
    const modulePath = require.resolve('./channels-modals.js');
    delete require.cache[modulePath];
    try {
      return {
        mod: require('./channels-modals.js'),
        checkbox,
        input,
        restore() {
          delete require.cache[modulePath];
          restoreGlobals();
        }
      };
    } catch (error) {
      restoreGlobals();
      throw error;
    }
  }

  test('新渠道默认关闭本地代理', () => {
    const runtime = loadChannelProxyControl();
    try {
      runtime.mod.setNewChannelProxyDefaults();
      assert.equal(runtime.checkbox.checked, false);
      assert.equal(runtime.input.value, 'http://127.0.0.1:7890');
      assert.equal(runtime.input.disabled, true);
      assert.equal(runtime.mod.getSubmittedChannelProxyURL(), '');
    } finally {
      runtime.restore();
    }
  });

  test('关闭代理开关时保存空代理，重新打开保留自定义地址', () => {
    const runtime = loadChannelProxyControl();
    try {
      runtime.mod.setChannelProxyFormValue('socks5://127.0.0.1:1080');
      assert.equal(runtime.checkbox.checked, true);
      assert.equal(runtime.input.disabled, false);

      runtime.checkbox.checked = false;
      runtime.mod.syncChannelProxyInputState();
      assert.equal(runtime.input.disabled, true);
      assert.equal(runtime.input.value, 'socks5://127.0.0.1:1080');
      assert.equal(runtime.mod.getSubmittedChannelProxyURL(), '');

      runtime.checkbox.checked = true;
      runtime.mod.syncChannelProxyInputState({ fillDefault: true });
      assert.equal(runtime.input.value, 'socks5://127.0.0.1:1080');
      assert.equal(runtime.mod.getSubmittedChannelProxyURL(), 'socks5://127.0.0.1:1080');
    } finally {
      runtime.restore();
    }
  });

  test('编辑无代理渠道保持关闭状态', () => {
    const runtime = loadChannelProxyControl();
    try {
      runtime.mod.setChannelProxyFormValue('');
      assert.equal(runtime.checkbox.checked, false);
      assert.equal(runtime.input.disabled, true);
      assert.equal(runtime.mod.getSubmittedChannelProxyURL(), '');
    } finally {
      runtime.restore();
    }
  });

  test('addCommonModels 只添加手动维护的 Codex 常用模型', async () => {
    const runtime = loadChannelEditor({
      rows: [{ model: 'GPT-5.4', redirect_enabled: false, protocol_aliases: {} }],
      fetchResult: {
        models: ['gpt-realtime-2.1'],
        source: 'models.dev',
        fetched_at: '2026-07-11T00:00:00Z'
      }
    });
    try {
      await runtime.mod.addCommonModels();
      assert.deepEqual(submittedModelRows(), [
        { model: 'GPT-5.4', redirect_enabled: false, protocol_aliases: {} },
        { model: 'gpt-5.4-mini', redirect_enabled: false, protocol_aliases: {} },
        { model: 'gpt-5.5', redirect_enabled: false, protocol_aliases: {} },
        { model: 'gpt-5.6-sol', redirect_enabled: false, protocol_aliases: {} },
        { model: 'gpt-5.6-luna', redirect_enabled: false, protocol_aliases: {} },
        { model: 'gpt-5.6-terra', redirect_enabled: false, protocol_aliases: {} }
      ]);
    } finally {
      runtime.restore();
    }
  });

	test('开启重定向会为已选多个协议生成独立对外模型，关闭后保留配置', () => {
	  const runtime = loadChannelEditor({
		channelType: 'codex',
		protocolTransforms: ['anthropic'],
		rows: [{ model: 'grok-4.5', redirect_enabled: false, protocol_aliases: {} }]
	  });
	  try {
		const row = global.redirectTableData[0];
		runtime.mod.setModelRedirectEnabled(row, true);
		assert.equal(row.redirect_enabled, true);
		assert.deepEqual(row.protocol_aliases, {
		  codex: ['grok-4.5'],
		  anthropic: ['grok-4.5']
		});

		row.protocol_aliases.codex = ['gpt-5.5'];
		runtime.mod.setModelRedirectEnabled(row, false);
		assert.equal(row.redirect_enabled, false);
		assert.deepEqual(row.protocol_aliases.codex, ['gpt-5.5']);
	  } finally {
		runtime.restore();
	  }
	});

	test('对外模型下拉按协议提供完整常用模型列表', () => {
	  const runtime = loadChannelEditor();
	  try {
		const options = runtime.mod.getCommonPublicModelOptions('codex');
		assert.deepEqual(options.map(option => option.value), runtime.mod.COMMON_MODELS.codex);
		assert.ok(options.some(option => option.value === 'gpt-5.5'));
	  } finally {
		runtime.restore();
	  }
	});

	test('添加模型支持单独模式和批量模式', () => {
	  const runtime = loadChannelEditor();
	  try {
		assert.deepEqual(
		  runtime.mod.parseModelAddEntries('single', ' grok-4.5 ', ''),
		  [{ model: 'grok-4.5', redirect_enabled: false, protocol_aliases: {}, vision_assist_enabled: false, vision_pool_enabled: false, vision_priority: 0 }]
		);
		assert.deepEqual(
		  runtime.mod.parseModelAddEntries('batch', '', 'gpt-5.5, claude-opus-4-8'),
		  [
			{ model: 'gpt-5.5', redirect_enabled: false, protocol_aliases: {}, vision_assist_enabled: false, vision_pool_enabled: false, vision_priority: 0 },
			{ model: 'claude-opus-4-8', redirect_enabled: false, protocol_aliases: {}, vision_assist_enabled: false, vision_pool_enabled: false, vision_priority: 0 }
		  ]
		);
	  } finally {
		runtime.restore();
	  }
	});

	test('对外模型不能与另一个真实上游模型同名', () => {
	  const runtime = loadChannelEditor();
	  try {
		assert.deepEqual(runtime.mod.findPublicModelUpstreamConflicts([
		  { model: 'gpt-5.5', redirect_enabled: false, protocol_aliases: {} },
		  { model: 'grok-4.5', redirect_enabled: true, protocol_aliases: { codex: ['gpt-5.5'] } }
		]), ['gpt-5.5']);
		assert.deepEqual(runtime.mod.findPublicModelUpstreamConflicts([
		  { model: 'grok-4.5', redirect_enabled: true, protocol_aliases: { codex: ['grok-4.5'] } }
		]), []);
	  } finally {
		runtime.restore();
	  }
	});

	test('同一协议的对外模型不能重复', () => {
	  const runtime = loadChannelEditor();
	  try {
		assert.deepEqual(runtime.mod.findDuplicatePublicModels([
		  { model: 'grok-4.5', redirect_enabled: true, protocol_aliases: { codex: ['gpt-5.5'] } },
		  { model: 'other', redirect_enabled: true, protocol_aliases: { codex: ['GPT-5.5'] } }
		]), ['GPT-5.5']);
		assert.deepEqual(runtime.mod.findDuplicatePublicModels([
		  { model: 'grok-4.5', redirect_enabled: true, protocol_aliases: { codex: ['gpt-5.5'], anthropic: ['claude-opus-4-8'] } },
		  { model: 'other', redirect_enabled: true, protocol_aliases: { anthropic: ['gpt-5.5'] } }
		]), []);
	  } finally {
		runtime.restore();
	  }
	});

	test('渠道测试模型随客户端协议使用对外模型，关闭重定向后改用上游模型', () => {
	  const channel = {
		channel_type: 'codex',
		models: [{
		  model: 'grok-4.5',
		  redirect_enabled: true,
		  protocol_aliases: {
			codex: ['gpt-5.5'],
			anthropic: ['claude-opus-4-8']
		  }
		}]
	  };
	  assert.deepEqual(getChannelTestModelNames(channel, 'codex'), ['gpt-5.5', 'grok-4.5']);
	  assert.deepEqual(getChannelTestModelNames(channel, 'anthropic'), ['claude-opus-4-8', 'grok-4.5']);
	  channel.models[0].redirect_enabled = false;
	  assert.deepEqual(getChannelTestModelNames(channel, 'codex'), ['grok-4.5']);
	});

	test('渠道测试模型列表排除全局禁用的请求名和实际上游模型', () => {
	  const previousWindow = global.window;
	  global.window = {
		isGlobalModelDisabledName: modelName => ['grok-4.5'].includes(String(modelName).toLowerCase())
	  };
	  try {
		assert.deepEqual(getChannelTestModelNames({
		  models: [{
			model: 'grok-4.5',
			redirect_enabled: true,
			protocol_aliases: { codex: ['gpt-5.5'] }
		  }, { model: 'gpt-5.6-sol' }]
		}, 'codex'), ['gpt-5.6-sol']);
	  } finally {
		global.window = previousWindow;
	  }
	});

}
