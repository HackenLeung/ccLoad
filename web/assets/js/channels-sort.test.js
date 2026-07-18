const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const assert = require('node:assert/strict');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, 'channels-sort.js'), 'utf8');

function createSortContext() {
  const values = new Map();
  const context = vm.createContext({
    console,
    URLSearchParams,
    activeSortPresetId: '',
    channelTypeTabList: [],
    filters: { channelType: 'codex' },
    channels: [],
    filteredChannels: [],
    localStorage: {
      getItem(key) {
        return values.has(key) ? values.get(key) : null;
      },
      setItem(key, value) {
        values.set(key, String(value));
      },
      removeItem(key) {
        values.delete(key);
      }
    },
    document: {
      addEventListener() {},
      getElementById() {
        return null;
      }
    },
    window: {
      t(key) {
        return key;
      }
    }
  });
  vm.runInContext(source, context);
  return { context, values };
}

test('sort presets are isolated by protocol while legacy presets stay discoverable', () => {
  const { context, values } = createSortContext();
  values.set('channels.sortPresets', JSON.stringify([
    { id: 'claude', name: 'Claude order', channelType: 'anthropic', channelOrder: [1, 2] },
    { id: 'codex', name: 'Codex order', channelType: 'codex', channelOrder: [2, 3] },
    { id: 'legacy', name: 'Legacy order', channelOrder: [3, 4] }
  ]));

  const presets = vm.runInContext(
    'getSortPresetsForType("codex", new Set([2, 3, 5])).map((preset) => preset.id)',
    context
  );
  assert.deepEqual(Array.from(presets), ['codex', 'legacy']);
});

test('active sort preset is stored independently for each protocol', () => {
  const { context, values } = createSortContext();

  vm.runInContext('sortChannelType = "codex"; setActiveSortPreset("codex-order")', context);
  vm.runInContext('sortChannelType = "anthropic"; setActiveSortPreset("claude-order")', context);

  assert.deepEqual(JSON.parse(values.get('channels.sortPreset.activeByType')), {
    codex: 'codex-order',
    anthropic: 'claude-order'
  });
  assert.equal(vm.runInContext('getActiveSortPresetIdForType("codex")', context), 'codex-order');
  assert.equal(vm.runInContext('getActiveSortPresetIdForType("anthropic")', context), 'claude-order');
});

test('priority updates preserve the complete current protocol order', () => {
  const { context } = createSortContext();
  const updates = vm.runInContext(
    'buildPriorityUpdatesFromOrder([{ id: 9 }, { id: 4 }, { id: 7 }])',
    context
  );
  assert.deepEqual(Array.from(updates, (item) => ({ ...item })), [
    { id: 9, priority: 30 },
    { id: 4, priority: 20 },
    { id: 7, priority: 10 }
  ]);
});

test('only the latest sort modal load may update the selected protocol', () => {
  const { context } = createSortContext();
  const states = vm.runInContext(`
    filters.channelType = 'codex';
    const codexLoad = createSortModalLoadContext();
    filters.channelType = 'anthropic';
    const anthropicLoad = createSortModalLoadContext();
    [isCurrentSortModalLoad(codexLoad), isCurrentSortModalLoad(anthropicLoad)];
  `, context);

  assert.deepEqual(Array.from(states), [false, true]);
  vm.runInContext('closeSortModal()', context);
  assert.equal(vm.runInContext('isCurrentSortModalLoad(anthropicLoad)', context), false);
});
