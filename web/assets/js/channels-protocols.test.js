const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

function loadProtocolConfig() {
  const window = {};
  const source = fs.readFileSync(path.join(__dirname, 'channels-protocols.js'), 'utf8');
  vm.runInNewContext(source, { window });
  return window.ChannelProtocolConfig;
}

test('Codex to OpenAI capability panel is limited to local OpenAI transforms', () => {
  const config = loadProtocolConfig();
  assert.equal(config.shouldShowCodexToOpenAICapabilities('openai', ['codex'], 'local'), true);
  assert.equal(config.shouldShowCodexToOpenAICapabilities('openai', ['codex'], 'upstream'), false);
  assert.equal(config.shouldShowCodexToOpenAICapabilities('openai', ['anthropic'], 'local'), false);
  assert.equal(config.shouldShowCodexToOpenAICapabilities('codex', ['openai'], 'local'), false);
});

test('missing capability config defaults every capability to enabled', () => {
  const config = loadProtocolConfig();
  const values = config.normalizeCodexToOpenAICapabilities(null);
  assert.equal(Object.keys(values).join(','), config.CODEX_TO_OPENAI_CAPABILITIES.join(','));
  assert.equal(Object.values(values).every(Boolean), true);
});

test('explicit capability values override defaults independently', () => {
  const config = loadProtocolConfig();
  const values = config.normalizeCodexToOpenAICapabilities({
    codex: { hosted_web_search: false, prompt_cache: false }
  });
  assert.equal(values.hosted_web_search, false);
  assert.equal(values.prompt_cache, false);
  assert.equal(values.function_tools, true);
  assert.equal(values.tool_search, true);
  assert.equal(values.reasoning, true);
});
