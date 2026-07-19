const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, 'channels-urls.js'), 'utf8');

function renderURLRow(rawURL) {
  const exactCheckbox = { checked: false };
  const row = {
    querySelector(selector) {
      if (selector === '.inline-url-exact-checkbox') return exactCheckbox;
      return null;
    }
  };
  const context = vm.createContext({
    console,
    inlineURLTableData: [rawURL],
    selectedURLIndices: new Set(),
    urlStatsMap: {},
    TemplateEngine: {
      render() {
        return row;
      }
    },
    window: {
      t(key) {
        return key;
      }
    }
  });

  vm.runInContext(source, context);
  vm.runInContext('createURLRow(0)', context);
  return exactCheckbox;
}

test('exact URL checkbox reflects a saved trailing marker', () => {
  assert.equal(renderURLRow('https://api.example.com/v1/chat/completions#').checked, true);
});

test('regular URL checkbox remains unchecked', () => {
  assert.equal(renderURLRow('https://api.example.com').checked, false);
});
