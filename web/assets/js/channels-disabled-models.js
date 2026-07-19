let globalDisabledModels = [];
let globalDisabledModelOptions = [];
let globalDisabledModelCombobox = null;

function globalDisabledModelOptionRows() {
  const disabled = new Set(globalDisabledModels.map(entry => String(entry.model || '').toLowerCase()));
  return globalDisabledModelOptions
    .filter(modelName => !disabled.has(String(modelName || '').toLowerCase()))
    .map(modelName => ({ value: modelName, label: modelName }));
}

function updateGlobalDisabledModelsCount() {
  const count = document.getElementById('globalDisabledModelsCount');
  if (count) count.textContent = String(globalDisabledModels.length);
}

window.isGlobalModelDisabledName = function isGlobalModelDisabledName(modelName) {
  const key = String(modelName || '').trim().toLowerCase();
  return key !== '' && globalDisabledModels.some(entry => String(entry.model || '').trim().toLowerCase() === key);
};

function renderGlobalDisabledModels() {
  const list = document.getElementById('globalDisabledModelsList');
  const empty = document.getElementById('globalDisabledModelsEmpty');
  if (!list || !empty) return;
  list.innerHTML = '';
  empty.classList.toggle('hidden', globalDisabledModels.length > 0);

  globalDisabledModels.forEach(entry => {
    const row = document.createElement('div');
    row.className = 'global-disabled-model-row';

    const main = document.createElement('div');
    main.className = 'global-disabled-model-main';
    const name = document.createElement('strong');
    name.className = 'global-disabled-model-name';
    name.textContent = entry.model;
    const status = document.createElement('span');
    status.className = 'global-disabled-model-status';
    status.textContent = window.t('channels.disabledGlobally');
    main.append(name, status);

    const restore = document.createElement('button');
    restore.type = 'button';
    restore.className = 'btn btn-secondary btn-sm';
    restore.textContent = window.t('channels.restoreModel');
    restore.addEventListener('click', () => removeGlobalDisabledModel(entry.model));
    row.append(main, restore);
    list.appendChild(row);
  });
  updateGlobalDisabledModelsCount();
  globalDisabledModelCombobox?.refresh();
}

async function loadGlobalDisabledModels() {
  globalDisabledModels = (await fetchDataWithAuth('/admin/channels/disabled-models')) || [];
  renderGlobalDisabledModels();
}

async function loadGlobalDisabledModelOptions() {
  try {
    const options = await fetchDataWithAuth('/admin/channels/filter-options');
    globalDisabledModelOptions = Array.isArray(options?.models) ? options.models : [];
  } catch (_) {
    globalDisabledModelOptions = [];
  }
  globalDisabledModelCombobox?.refresh();
}

function ensureGlobalDisabledModelCombobox() {
  if (globalDisabledModelCombobox || typeof createSearchableCombobox !== 'function') return;
  globalDisabledModelCombobox = createSearchableCombobox({
    attachMode: true,
    inputId: 'globalDisabledModelInput',
    dropdownId: 'globalDisabledModelDropdown',
    allowCustomInput: true,
    browseAllOnOpen: true,
    getOptions: globalDisabledModelOptionRows,
    onSelect: () => {}
  });
}

async function openGlobalDisabledModels() {
  ensureGlobalDisabledModelCombobox();
  document.getElementById('globalDisabledModelsModal')?.classList.add('show');
  await Promise.all([loadGlobalDisabledModels(), loadGlobalDisabledModelOptions()]);
  globalDisabledModelCombobox?.setValue('', '');
  setTimeout(() => document.getElementById('globalDisabledModelInput')?.focus(), 50);
}

function closeGlobalDisabledModels() {
  document.getElementById('globalDisabledModelsModal')?.classList.remove('show');
}

async function addGlobalDisabledModel() {
  const input = document.getElementById('globalDisabledModelInput');
  const modelName = String(input?.value || '').trim();
  if (!modelName) {
    window.showWarning?.(window.t('channels.enterDisabledModel'));
    return;
  }
  try {
    await fetchDataWithAuth('/admin/channels/disabled-models', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ model: modelName })
    });
    globalDisabledModelCombobox?.setValue('', '');
    await loadGlobalDisabledModels();
    window.showSuccess?.(window.t('channels.modelDisabledSuccess', { model: modelName }));
  } catch (error) {
    window.showError?.(window.t('channels.modelDisabledFailed', { error: error.message }));
  }
}

async function removeGlobalDisabledModel(modelName) {
  try {
    await fetchDataWithAuth(`/admin/channels/disabled-models?model=${encodeURIComponent(modelName)}`, { method: 'DELETE' });
    await loadGlobalDisabledModels();
    window.showSuccess?.(window.t('channels.modelRestoredSuccess', { model: modelName }));
  } catch (error) {
    window.showError?.(window.t('channels.modelRestoreFailed', { error: error.message }));
  }
}

function initGlobalDisabledModels() {
  document.getElementById('globalDisabledModelsBtn')?.addEventListener('click', openGlobalDisabledModels);
  document.getElementById('closeGlobalDisabledModelsBtn')?.addEventListener('click', closeGlobalDisabledModels);
  document.getElementById('doneGlobalDisabledModelsBtn')?.addEventListener('click', closeGlobalDisabledModels);
  document.getElementById('addGlobalDisabledModelBtn')?.addEventListener('click', addGlobalDisabledModel);
  document.getElementById('globalDisabledModelInput')?.addEventListener('keydown', event => {
    if (event.key === 'Enter') {
      event.preventDefault();
      event.stopImmediatePropagation();
      addGlobalDisabledModel();
    }
  });
  document.getElementById('globalDisabledModelsModal')?.addEventListener('mousedown', event => {
    if (event.target.id === 'globalDisabledModelsModal') closeGlobalDisabledModels();
  });
  ensureGlobalDisabledModelCombobox();
  loadGlobalDisabledModels().catch(error => console.error('Load disabled models failed', error));
}

if (document.readyState === 'loading') {
  document.addEventListener('DOMContentLoaded', initGlobalDisabledModels);
} else {
  initGlobalDisabledModels();
}
