// ==================== 渠道排序功能 ====================
// 拖拽排序实现,优先级相差10

let sortChannels = []; // 存储排序中的渠道列表
let draggedItem = null; // 当前拖拽的元素
const CHANNEL_SORT_PRESETS_KEY = 'channels.sortPresets';
const CHANNEL_SORT_PRESET_ACTIVE_KEY = 'channels.sortPreset.active';

function normalizeSortPresetId(id) {
  return String(id || '').trim();
}

function loadSortPresets() {
  try {
    const raw = localStorage.getItem(CHANNEL_SORT_PRESETS_KEY);
    const parsed = raw ? JSON.parse(raw) : [];
    if (!Array.isArray(parsed)) return [];
    return parsed
      .map((preset) => ({
        id: normalizeSortPresetId(preset.id),
        name: String(preset.name || '').trim(),
        channelOrder: Array.isArray(preset.channelOrder)
          ? preset.channelOrder.map((id) => Number(id)).filter((id) => Number.isFinite(id) && id > 0)
          : []
      }))
      .filter((preset) => preset.id && preset.name && preset.channelOrder.length > 0);
  } catch (_) {
    return [];
  }
}

function saveSortPresets(presets) {
  localStorage.setItem(CHANNEL_SORT_PRESETS_KEY, JSON.stringify(Array.isArray(presets) ? presets : []));
}

function buildSortPresetId() {
  return `preset_${Date.now()}_${Math.random().toString(36).slice(2, 8)}`;
}

function getActiveSortPreset() {
  const activeId = normalizeSortPresetId(activeSortPresetId);
  if (!activeId) return null;
  return loadSortPresets().find((preset) => preset.id === activeId) || null;
}

function setActiveSortPreset(id) {
  activeSortPresetId = normalizeSortPresetId(id);
  if (activeSortPresetId) {
    localStorage.setItem(CHANNEL_SORT_PRESET_ACTIVE_KEY, activeSortPresetId);
  } else {
    localStorage.removeItem(CHANNEL_SORT_PRESET_ACTIVE_KEY);
  }
}

function refreshSortPresetSelect() {
  const select = document.getElementById('sortPresetLoadSelect');

  const presets = loadSortPresets();
  const currentValue = normalizeSortPresetId(activeSortPresetId);
  if (select) {
    select.innerHTML = '';

    const placeholderOption = document.createElement('option');
    placeholderOption.value = '';
    placeholderOption.textContent = (window.t && window.t('channels.sortPresetLoadPlaceholder')) || '选择已保存的方案…';
    select.appendChild(placeholderOption);

    presets.forEach((preset) => {
      const option = document.createElement('option');
      option.value = preset.id;
      option.textContent = preset.name;
      select.appendChild(option);
    });

    select.value = presets.some((preset) => preset.id === currentValue) ? currentValue : '';
    if (select.value !== currentValue) {
      setActiveSortPreset('');
    }
  }

  syncSortPresetEditor();
}

function syncSortPresetEditor() {
  const preset = getActiveSortPreset();
  const input = document.getElementById('sortPresetNameInput');
  if (input) {
    input.value = preset ? preset.name : '';
    input.placeholder = preset ? preset.name : ((window.t && window.t('channels.sortPresetNamePlaceholder')) || '填写排序方案名称');
  }

  const hint = document.getElementById('sortPresetEditorHint');
  if (hint) {
    hint.textContent = preset
      ? window.t('channels.sortPresetUpdateHint')
      : window.t('channels.sortPresetCreateHint');
  }

  const saveBtn = document.getElementById('saveSortPresetModalBtn');
  if (saveBtn) {
    saveBtn.textContent = preset
      ? window.t('channels.updateSortPreset')
      : window.t('channels.createSortPreset');
    saveBtn.disabled = !preset && !(input && input.value.trim());
  }

  const deleteBtn = document.getElementById('deleteSortPresetModalBtn');
  if (deleteBtn) {
    deleteBtn.disabled = !preset;
  }
}

function getChannelIdOrder(list) {
  return (Array.isArray(list) ? list : [])
    .map((channel) => Number(channel && channel.id))
    .filter((id) => Number.isFinite(id) && id > 0);
}

function pinSortChannel(channelId) {
  const numericId = Number(channelId);
  if (!Number.isFinite(numericId) || numericId <= 0 || sortChannels.length <= 1) return;

  const index = sortChannels.findIndex((channel) => Number(channel && channel.id) === numericId);
  if (index <= 0) return;

  const [channel] = sortChannels.splice(index, 1);
  sortChannels.unshift(channel);
  renderSortList();
}

// 按方案的 channelOrder 重排列表（纯函数，不写后端）：命中的按方案顺序在前，
// 其余（方案未收录的新渠道）保持原相对顺序追加到末尾。
function orderListByPreset(list, preset) {
  if (!preset || !Array.isArray(list) || list.length <= 1) return Array.isArray(list) ? [...list] : [];

  const byId = new Map(list.map((channel) => [Number(channel.id), channel]));
  const usedIds = new Set();
  const ordered = [];

  preset.channelOrder.forEach((id) => {
    const numericId = Number(id);
    if (!byId.has(numericId) || usedIds.has(numericId)) return;
    ordered.push(byId.get(numericId));
    usedIds.add(numericId);
  });

  list.forEach((channel) => {
    const id = Number(channel && channel.id);
    if (usedIds.has(id)) return;
    ordered.push(channel);
  });

  return ordered;
}

function buildPriorityUpdatesFromOrder(list) {
  return (Array.isArray(list) ? list : []).map((channel, index) => ({
    id: channel.id,
    priority: (list.length - index) * 10
  }));
}

// 载入方案：仅在弹窗内把拖拽列表按方案顺序重排，不写后端。
function loadSortPresetIntoModal(id) {
  const normalizedId = normalizeSortPresetId(id);
  setActiveSortPreset(normalizedId);

  const preset = getActiveSortPreset();
  if (preset) {
    sortChannels = orderListByPreset(sortChannels, preset);
    renderSortList();
  }

  syncSortPresetEditor();
}

function getSortPresetEditorName(defaultName = '') {
  const input = document.getElementById('sortPresetNameInput');
  if (!input) return String(defaultName || '').trim();
  return String(input.value || defaultName || '').trim();
}

function saveSortPresetFromOrder(channelOrder, defaultName = '') {
  const order = Array.isArray(channelOrder)
    ? channelOrder.map((id) => Number(id)).filter((id) => Number.isFinite(id) && id > 0)
    : [];
  if (order.length === 0) {
    window.showNotification(window.t('channels.sortPresetNoChannels'), 'warning');
    return;
  }

  const name = getSortPresetEditorName(defaultName);
  if (!name) {
    window.showNotification(window.t('channels.sortPresetNameRequired'), 'warning');
    const input = document.getElementById('sortPresetNameInput');
    if (input) input.focus();
    return;
  }

  const presets = loadSortPresets();
  const activeId = normalizeSortPresetId(activeSortPresetId);
  let existingIndex = activeId
    ? presets.findIndex((preset) => preset.id === activeId)
    : -1;
  if (existingIndex < 0) {
    existingIndex = presets.findIndex((preset) => preset.name === name);
  }
  const nextPreset = {
    id: existingIndex >= 0 ? presets[existingIndex].id : buildSortPresetId(),
    name,
    channelOrder: order
  };

  if (existingIndex >= 0) {
    presets[existingIndex] = nextPreset;
  } else {
    presets.push(nextPreset);
  }

  saveSortPresets(presets);
  setActiveSortPreset(nextPreset.id);
  refreshSortPresetSelect();
  window.showSuccess(window.t('channels.sortPresetSaved'));
  // 保存方案是弹窗内的次要动作，不关闭弹窗，也不写后端——用户可继续拖拽或点「保存排序」应用。
}

function saveSortPresetFromModal() {
  if (sortChannels.length === 0) {
    window.showNotification(window.t('channels.sortPresetNoChannels'), 'warning');
    return;
  }
  saveSortPresetFromOrder(getChannelIdOrder(sortChannels));
}

async function deleteActiveSortPreset() {
  const activeId = normalizeSortPresetId(activeSortPresetId);
  if (!activeId) return;
  const presets = loadSortPresets();
  const preset = presets.find((item) => item.id === activeId);
  if (!preset) return;
  const message = (window.t && window.t('channels.sortPresetDeleteConfirm', { name: preset.name }))
    || `删除排序方案 "${preset.name}"?`;
  const confirmed = await showConfirmDialog({
    title: window.t('channels.deleteSortPresetTitle'),
    message,
    okText: window.t('channels.deleteSortPreset'),
    danger: true
  });
  if (!confirmed) return;

  saveSortPresets(presets.filter((item) => item.id !== activeId));
  setActiveSortPreset('');
  refreshSortPresetSelect();
  window.showSuccess(window.t('channels.sortPresetDeleted'));
}

function updateSortPresetSaveButtonState() {
  const input = document.getElementById('sortPresetNameInput');
  const saveBtn = document.getElementById('saveSortPresetModalBtn');
  if (!input || !saveBtn) return;
  const preset = getActiveSortPreset();
  saveBtn.disabled = !preset && !input.value.trim();
}

// 打开排序模态框
function showSortModal() {
  const modal = document.getElementById('sortModal');
  if (!modal) return;

  // 获取当前渠道列表(使用筛选后的渠道)
  const sourceChannels = filteredChannels.length > 0 ? filteredChannels : channels;

  if (!sourceChannels || sourceChannels.length === 0) {
    window.showError(window.t('channels.loadChannelsFailed'));
    return;
  }

  // 每次打开都以当前后端顺序为准，清空已载入方案——方案是可选的起点，不是隐式状态。
  sortChannels = [...sourceChannels];
  setActiveSortPreset('');
  refreshSortPresetSelect();

  // 渲染排序列表
  renderSortList();

  // 显示模态框(使用show类实现居中)
  modal.classList.add('show');
}

// 关闭排序模态框
function closeSortModal() {
  const modal = document.getElementById('sortModal');
  if (modal) {
    modal.classList.remove('show');
  }
  sortChannels = [];
  draggedItem = null;
}

// 渲染排序列表
function renderSortList() {
  const container = document.getElementById('sortListContainer');
  if (!container) return;

  container.innerHTML = '';

  if (sortChannels.length === 0) {
    container.innerHTML = `<p class="sort-list-empty">${window.t('channels.noChannelsForSort')}</p>`;
    return;
  }

  sortChannels.forEach((channel, index) => {
    const item = createSortItem(channel, index);
    container.appendChild(item);
  });

  // 添加拖拽事件监听
  attachDragListeners();

  // Translate dynamically rendered elements
  if (window.i18n && window.i18n.translatePage) {
    window.i18n.translatePage();
  }
}

// 创建排序卡片
function createSortItem(channel, index) {
  const template = document.getElementById('tpl-sort-item');
  if (!template) return document.createElement('div');

  // 状态徽章
  let statusBadge = '';
  if (!channel.enabled) {
    statusBadge = `<span class="sort-item-status-badge sort-item-status-badge--disabled">${window.t('channels.statusDisabled')}</span>`;
  } else if (channel.cooldown_until && new Date(channel.cooldown_until) > new Date()) {
    statusBadge = `<span class="sort-item-status-badge sort-item-status-badge--cooldown">${window.t('channels.cooldownStatus')}</span>`;
  } else {
    statusBadge = `<span class="sort-item-status-badge sort-item-status-badge--normal">${window.t('channels.statusNormal')}</span>`;
  }

  const html = template.innerHTML
    .replace(/\{\{id\}\}/g, channel.id)
    .replace(/\{\{name\}\}/g, escapeHtml(channel.name))
    .replace(/\{\{priority\}\}/g, channel.priority)
    .replace(/\{\{\{statusBadge\}\}\}/g, statusBadge);

  const div = document.createElement('div');
  div.innerHTML = html;
  const item = div.firstElementChild;

  // 设置索引属性用于拖拽
  item.dataset.index = index;

  return item;
}

// 添加拖拽事件监听：采用 dragover 实时 DOM 重排，避免 drop 命中率低的问题
function attachDragListeners() {
  const container = document.getElementById('sortListContainer');
  if (!container) return;

  container.querySelectorAll('.sort-item').forEach(item => {
    item.addEventListener('dragstart', handleDragStart);
    item.addEventListener('dragend', handleDragEnd);
  });

  // 容器级 dragover：无论释放在卡片还是间隙，都能捕获
  container.addEventListener('dragover', handleContainerDragOver);
}

// 拖拽开始
function handleDragStart(e) {
  draggedItem = this;
  this.classList.add('is-dragging');
  e.dataTransfer.effectAllowed = 'move';
  // Firefox 要求必须 setData 才会触发后续拖拽事件
  try { e.dataTransfer.setData('text/plain', this.dataset.channelId || ''); } catch (_) { /* ignore */ }
}

// 拖拽结束：从当前 DOM 顺序同步回 sortChannels，然后重渲染刷新序号
function handleDragEnd() {
  this.classList.remove('is-dragging');

  const container = document.getElementById('sortListContainer');
  if (container) {
    const byId = new Map(sortChannels.map(c => [String(c.id), c]));
    const newOrder = [];
    container.querySelectorAll('.sort-item').forEach(el => {
      const ch = byId.get(el.dataset.channelId);
      if (ch) newOrder.push(ch);
    });
    if (newOrder.length === sortChannels.length) {
      sortChannels = newOrder;
    }
  }

  draggedItem = null;
  renderSortList();
}

// 容器级 dragover：按鼠标 Y 坐标实时插入到最近的兄弟节点前后
function handleContainerDragOver(e) {
  e.preventDefault();
  if (!draggedItem) return;
  e.dataTransfer.dropEffect = 'move';

  const container = e.currentTarget;
  const afterElement = getDragAfterElement(container, e.clientY);

  if (afterElement == null) {
    if (container.lastElementChild !== draggedItem) {
      container.appendChild(draggedItem);
    }
  } else if (afterElement !== draggedItem && afterElement !== draggedItem.nextElementSibling) {
    container.insertBefore(draggedItem, afterElement);
  }
}

// 找到鼠标 Y 坐标上方最接近的 sort-item，作为插入锚点
function getDragAfterElement(container, y) {
  const siblings = container.querySelectorAll('.sort-item:not(.is-dragging)');
  let closest = null;
  let closestOffset = Number.NEGATIVE_INFINITY;
  siblings.forEach(child => {
    const box = child.getBoundingClientRect();
    const offset = y - box.top - box.height / 2;
    if (offset < 0 && offset > closestOffset) {
      closestOffset = offset;
      closest = child;
    }
  });
  return closest;
}

// 保存排序
async function saveSortOrder() {
  if (sortChannels.length === 0) {
    window.showNotification(window.t('channels.sortNoChanges'), 'warning');
    return;
  }

  // 计算新的优先级(从高到低,相差10)
  const updates = buildPriorityUpdatesFromOrder(sortChannels);

  try {
    const result = await fetchDataWithAuth('/admin/channels/batch-priority', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json'
      },
      body: JSON.stringify({ updates })
    });

    window.showSuccess(window.t('channels.sortSaveSuccess'));
    closeSortModal();
    const currentType = (filters && filters.channelType) ? filters.channelType : 'all';
    if (typeof loadChannels === 'function') await loadChannels(currentType);
  } catch (error) {
    console.error('Save sort order failed:', error);
    window.showError(error.message || window.t('channels.sortSaveFailed'));
  }
}

// 初始化排序按钮事件
document.addEventListener('DOMContentLoaded', function() {
  const sortBtn = document.getElementById('btn_sort');
  if (sortBtn) {
    sortBtn.addEventListener('click', showSortModal);
  }

  refreshSortPresetSelect();

  const sortPresetLoadSelect = document.getElementById('sortPresetLoadSelect');
  if (sortPresetLoadSelect) {
    sortPresetLoadSelect.addEventListener('change', (event) => {
      loadSortPresetIntoModal(event.target.value);
    });
  }

  const sortPresetNameInput = document.getElementById('sortPresetNameInput');
  if (sortPresetNameInput) {
    sortPresetNameInput.addEventListener('input', updateSortPresetSaveButtonState);
  }

});
