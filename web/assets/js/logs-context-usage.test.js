const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const assert = require('node:assert/strict');
const vm = require('node:vm');

const logsSource = fs.readFileSync(path.join(__dirname, 'logs.js'), 'utf8') + `
globalThis.__logsContextUsageTest = {
  buildContextUsageDisplay,
  resolveContextWindowTiers,
  fitContextWindow
};
`;

function loadContextHelpers() {
  const context = {
    URLSearchParams,
    console,
    clearTimeout,
    setTimeout,
    escapeHtml: (value) => String(value),
    // 与 ui.js 的 formatNumber 行为一致（K/M 缩写）
    formatNumber: (num) => {
      const n = Number(num);
      if (n >= 1000000) return (n / 1000000).toFixed(1) + 'M';
      if (n >= 1000) return (n / 1000).toFixed(1) + 'K';
      return String(n);
    },
    localStorage: {
      getItem: () => null,
      removeItem() {},
      setItem() {}
    },
    window: {
      addEventListener() {},
      initPageBootstrap() {},
      t: (key) => ({
        'logs.colContext': '上下文',
        'logs.ctxWindowGuess': '窗口按用量推测'
      })[key] || key
    },
    document: {
      addEventListener() {},
      getElementById: () => null,
      querySelectorAll: () => []
    }
  };
  vm.runInNewContext(logsSource, context, { filename: 'logs.js' });
  return context.__logsContextUsageTest;
}

// vm 里的数组/对象来自另一个 realm，搬回本 realm 才能 deepEqual
function windowTiersOf(resolveContextWindowTiers, model) {
  const result = resolveContextWindowTiers(model);
  return { tiers: Array.from(result.tiers), inferred: result.inferred };
}

test('已知系列给出确定档位，认不出的走兜底阶梯并标记为推测', () => {
  const { resolveContextWindowTiers } = loadContextHelpers();
  const tiers = (model) => windowTiersOf(resolveContextWindowTiers, model);

  // 带 1m 标记的直接按 1M 起算，没标记的按 200K 起算、超出后才升到 1M
  assert.deepEqual(tiers('claude-opus-5[1m]'), { tiers: [1000000], inferred: false });
  assert.deepEqual(tiers('claude-sonnet-5'), { tiers: [200000, 1000000], inferred: false });
  assert.deepEqual(tiers('gpt-5.4'), { tiers: [400000], inferred: false });
  assert.deepEqual(tiers('gpt-5-codex'), { tiers: [400000], inferred: false });
  assert.deepEqual(tiers('gemini-2.5-pro'), { tiers: [1048576, 2097152], inferred: false });

  // GLM/DeepSeek 这类窗口未知的模型走兜底阶梯，而不是放弃显示百分比
  const glm = tiers('glm-5.3-flash');
  assert.equal(glm.inferred, true);
  assert.equal(glm.tiers[0], 128000);
  assert.deepEqual(tiers('deepseek-v4-flash-vision-exp').inferred, true);

  // 连模型名都没有时不猜
  assert.deepEqual(tiers(''), { tiers: [], inferred: false });
});

test('fitContextWindow 取第一个装得下水位的档位，全装不下则以实测值为满格', () => {
  const { fitContextWindow } = loadContextHelpers();

  assert.equal(fitContextWindow(180000, [200000, 1000000]), 200000);
  // 超过 200K 说明客户端开了 context-1m，直接跳 1M 而不是逐档爬
  assert.equal(fitContextWindow(210000, [200000, 1000000]), 1000000);
  assert.equal(fitContextWindow(900000, [200000, 1000000]), 1000000);
  assert.equal(fitContextWindow(420000, [400000]), 420000);
  // 没有档位可用时返回 0，交给调用方走「只显示绝对值」分支
  assert.equal(fitContextWindow(500000, []), 0);
});

test('上下文水位取输入+缓存读+缓存建，不受 input_tokens 偏小影响', () => {
  const { buildContextUsageDisplay } = loadContextHelpers();

  const html = buildContextUsageDisplay({
    model: 'claude-opus-5[1m]',
    input_tokens: 4,
    cache_read_input_tokens: 178402,
    cache_creation_input_tokens: 2048
  });
  const used = 4 + 178402 + 2048;

  assert.match(html, /ctx-usage ctx-low/);
  assert.doesNotMatch(html, /窗口按用量推测/);
  assert.match(html, /--ctx-pct: 18\.0%/);
  assert.match(html, />18%</);
  assert.match(html, />180\.5K</);
  assert.ok(html.includes(`${used.toLocaleString()} / ${(1000000).toLocaleString()}`));
});

test('窗口未知的模型仍给出百分比，推测说明只出现在 hover 里', () => {
  const { buildContextUsageDisplay } = loadContextHelpers();

  // 真实数据形状：glm-5.3-flash，输入 24825 + 缓存读 7296 = 32121，兜底装进 128K
  const html = buildContextUsageDisplay({
    model: 'glm-5.3-flash',
    input_tokens: 24825,
    cache_read_input_tokens: 7296,
    cache_creation_input_tokens: 0
  });

  assert.match(html, /ctx-usage ctx-low/);
  assert.match(html, />25%</);
  assert.match(html, />32\.1K</);
  assert.ok(html.includes(`${(32121).toLocaleString()} / ${(128000).toLocaleString()}`));
  assert.match(html, /title="[^"]*窗口按用量推测/);
});

test('占用分级配色，且水位打穿档位时不会显示超过 100%', () => {
  const { buildContextUsageDisplay } = loadContextHelpers();
  const entry = (model, used) => ({
    model,
    input_tokens: used,
    cache_read_input_tokens: 0,
    cache_creation_input_tokens: 0
  });

  assert.match(buildContextUsageDisplay(entry('claude-sonnet-5', 140000)), /ctx-mid/);
  assert.match(buildContextUsageDisplay(entry('claude-sonnet-5', 190000)), /ctx-high/);

  // 没有 1m 标记却超过 200K：说明客户端开了 context-1m，按 1M 重算而不是显示 >100%
  const upgraded = buildContextUsageDisplay(entry('claude-sonnet-5', 250000));
  assert.ok(upgraded.includes(`/ ${(1000000).toLocaleString()}`));
  assert.match(upgraded, /ctx-low/);
  assert.match(upgraded, />25%</);

  // 单档系列被打穿时以实测值为满格，显示 100% 而不是 105%
  const saturated = buildContextUsageDisplay(entry('gpt-5.4', 420000));
  assert.match(saturated, /ctx-high/);
  assert.match(saturated, />100%</);
});

test('没有 token 数据不渲染，没有模型名则退回绝对值', () => {
  const { buildContextUsageDisplay } = loadContextHelpers();

  assert.equal(buildContextUsageDisplay({ model: 'claude-opus-5' }), '');
  assert.equal(buildContextUsageDisplay(null), '');

  const noModel = buildContextUsageDisplay({ model: '', input_tokens: 4200 });
  assert.match(noModel, /token-metric-value/);
  assert.match(noModel, /4\.2K/);
  assert.doesNotMatch(noModel, /%/);
});

test('配了模型重定向时按 actual_model 判断窗口，而不是对外的 model 名', () => {
  const { buildContextUsageDisplay } = loadContextHelpers();

  // 对外叫 claude-sonnet-4-5，实际转发到 glm：窗口未知，应该走兜底阶梯并带 ~
  const redirected = buildContextUsageDisplay({
    model: 'claude-sonnet-4-5',
    actual_model: 'glm-5.3-flash',
    input_tokens: 32121,
    cache_read_input_tokens: 0,
    cache_creation_input_tokens: 0
  });
  assert.match(redirected, />25%</);
  assert.ok(redirected.includes(`/ ${(128000).toLocaleString()}`));

  // 没有重定向时（actual_model 为空）仍按 model 判断
  const direct = buildContextUsageDisplay({
    model: 'claude-sonnet-4-5',
    actual_model: '',
    input_tokens: 32121,
    cache_read_input_tokens: 0,
    cache_creation_input_tokens: 0
  });
  assert.match(direct, />16%</);
  assert.ok(direct.includes(`/ ${(200000).toLocaleString()}`));
});
