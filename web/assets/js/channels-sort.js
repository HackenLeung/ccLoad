// ==================== 渠道排序功能 ====================
// 拖拽排序实现,优先级相差10
// 排序与本地方案按 channel_type 隔离：Claude / Codex / OpenAI / Gemini 各自独立

let sortChannels = []; // 存储排序中的渠道列表
let draggedItem = null; // 当前拖拽的元素
// activeSortPresetId 定义在 channels-state.js，这里按类型读写 active map
let sortChannelType = ''; // 当前排序弹窗锁定的渠道类型
let sortModalLoadRequestId = 0; // 仅允许最后一次打开请求更新弹窗状态
const CHANNEL_SORT_PRESETS_KEY = 'channels.sortPresets';
const CHANNEL_SORT_PRESET_ACTIVE_BY_TYPE_KEY = 'channels.sortPreset.activeByType';
const CHANNEL_SORT_PRESET_ACTIVE_KEY_LEGACY = 'channels.sortPreset.active';

function normalizeSortPresetId(id) {
  return String(id || '').trim();
}

function normalizeSortChannelType(type) {
  const value = String(type || '').trim().toLowerCase();
  if (!value || value === 'all') return '';
  return value;
}

function getCurrentSortChannelType() {
  if (typeof filters !== 'undefined' && filters && filters.channelType) {
    return normalizeSortChannelType(filters.channelType);
  }
  return '';
}

function createSortModalLoadContext() {
  return {
    requestId: ++sortModalLoadRequestId,
    channelType: getCurrentSortChannelType()
  };
}

function isCurrentSortModalLoad(loadContext) {
  return Boolean(
    loadContext
    && loadContext.requestId === sortModalLoadRequestId
    && loadContext.channelType
    && loadContext.channelType === getCurrentSortChannelType()
  );
}

function getSortChannelTypeLabel(type) {
  const normalized = normalizeSortChannelType(type);
  if (!normalized) {
    return (window.t && window.t('channels.allTypes')) || '全部类型';
  }
  if (Array.isArray(channelTypeTabList)) {
    const hit = channelTypeTabList.find((item) => normalizeSortChannelType(item && item.value) === normalized);
    if (hit && hit.display_name) return hit.display_name;
  }
  return normalized;
}

function loadActiveSortPresetMap() {
  try {
    const raw = localStorage.getItem(CHANNEL_SORT_PRESET_ACTIVE_BY_TYPE_KEY);
    const parsed = raw ? JSON.parse(raw) : {};
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return {};
    const map = {};
    Object.keys(parsed).forEach((key) => {
      const type = normalizeSortChannelType(key);
      const id = normalizeSortPresetId(parsed[key]);
      if (type && id) map[type] = id;
    });
    return map;
  } catch (_) {
    return {};
  }
}

function saveActiveSortPresetMap(map) {
  localStorage.setItem(
    CHANNEL_SORT_PRESET_ACTIVE_BY_TYPE_KEY,
    JSON.stringify(map && typeof map === 'object' ? map : {})
  );
}

function getActiveSortPresetIdForType(type) {
  const normalized = normalizeSortChannelType(type);
  if (!normalized) return '';
  const map = loadActiveSortPresetMap();
  if (map[normalized]) return map[normalized];

  // 兼容旧版全局 active key：仅在当前类型下第一次读取时迁移
  try {
    const legacy = normalizeSortPresetId(localStorage.getItem(CHANNEL_SORT_PRESET_ACTIVE_KEY_LEGACY));
    if (!legacy) return '';
    const legacyPreset = loadSortPresets().find((preset) => preset.id === legacy);
    if (!legacyPreset) return '';
    if (legacyPreset.channelType && legacyPreset.channelType !== normalized) return '';
    map[normalized] = legacy;
    saveActiveSortPresetMap(map);
    return legacy;
  } catch (_) {
    return '';
  }
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
        channelType: normalizeSortChannelType(preset.channelType || preset.channel_type || ''),
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

function getSortPresetsForType(type, availableChannelIds = null) {
  const normalized = normalizeSortChannelType(type);
  const available = availableChannelIds instanceof Set
    ? availableChannelIds
    : (Array.isArray(availableChannelIds)
      ? new Set(availableChannelIds.map((id) => Number(id)).filter((id) => Number.isFinite(id) && id > 0))
      : null);

  return loadSortPresets().filter((preset) => {
    if (preset.channelType) {
      return preset.channelType === normalized;
    }
    // 旧版无类型方案：仅当顺序与当前类型渠道有交集时，才出现在该类型下
    if (!available || available.size === 0) return !normalized;
    return preset.channelOrder.some((id) => available.has(Number(id)));
  });
}

function getActiveSortPreset() {
  const activeId = normalizeSortPresetId(activeSortPresetId);
  if (!activeId) return null;
  return getSortPresetsForType(sortChannelType, getChannelIdOrder(sortChannels))
    .find((preset) => preset.id === activeId) || null;
}

function setActiveSortPreset(id) {
  activeSortPresetId = normalizeSortPresetId(id);
  const type = normalizeSortChannelType(sortChannelType || getCurrentSortChannelType());
  if (!type) return;

  const map = loadActiveSortPresetMap();
  if (activeSortPresetId) {
    map[type] = activeSortPresetId;
  } else {
    delete map[type];
  }
  saveActiveSortPresetMap(map);
}

function updateSortModalScopeUI() {
  const typeLabel = getSortChannelTypeLabel(sortChannelType);
  const title = document.getElementById('sortModalTitle');
  if (title) {
    const base = (window.t && window.t('channels.sortModalTitle')) || '渠道排序';
    title.textContent = sortChannelType ? `${base} · ${typeLabel}` : base;
    title.removeAttribute('data-i18n');
  }

  const scope = document.getElementById('sortModalScopeText');
  if (scope) {
    scope.textContent = sortChannelType
      ? ((window.t && window.t('channels.sortScopeType', { type: typeLabel })) || `当前仅排序：${typeLabel}`)
      : ((window.t && window.t('channels.sortScopeAll')) || '当前未按类型隔离');
    scope.removeAttribute('data-i18n');
  }
}

function refreshSortPresetSelect() {
  const select = document.getElementById('sortPresetLoadSelect');
  const presets = getSortPresetsForType(sortChannelType, getChannelIdOrder(sortChannels));
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
      option.textContent = preset.channelType
        ? preset.name
        : `${preset.name} (${(window.t && window.t('channels.sortPresetLegacy')) || '旧方案'})`;
      select.appendChild(option);
    });

    select.value = presets.some((preset) => preset.id === currentValue) ? currentValue : '';
    if (select.value !== currentValue) {
      setActiveSortPreset('');
    }
  }

  syncSortPresetEditor();
  updateSortModalScopeUI();
}

function syncSortPresetEditor() {
  const preset = getActiveSortPreset();
  const input = document.getElementById('sortPresetNameInput');
  if (input) {
    input.value = preset ? preset.name : '';
    input.placeholder = preset
      ? preset.name
      : ((window.t && window.t('channels.sortPresetNamePlaceholder')) || '填写排序方案名称');
  }

  const hint = document.getElementById('sortPresetEditorHint');
  if (hint) {
    const typeLabel = getSortChannelTypeLabel(sortChannelType);
    if (preset) {
      hint.textContent = (window.t && window.t('channels.sortPresetUpdateHintType', { type: typeLabel }))
        || window.t('channels.sortPresetUpdateHint');
    } else {
      hint.textContent = (window.t && window.t('channels.sortPresetCreateHintType', { type: typeLabel }))
        || window.t('channels.sortPresetCreateHint');
    }
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

function getChannelsForCurrentSortType() {
  // 降级路径：用当前 tab 已加载列表。
  // loadChannels(type)/排序全量接口已按“对该协议可用”返回（含协议转换渠道），
  // 这里不能再按底层 channel_type 过滤。
  const source = (Array.isArray(filteredChannels) && filteredChannels.length > 0)
    ? filteredChannels
    : (Array.isArray(channels) ? channels : []);
  return [...source];
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

  const channelType = normalizeSortChannelType(sortChannelType || getCurrentSortChannelType());
  if (!channelType) {
    window.showNotification(window.t('channels.sortTypeRequired'), 'warning');
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
    // 同类型下同名覆盖；跨类型允许重名
    existingIndex = presets.findIndex((preset) =>
      preset.name === name && normalizeSortChannelType(preset.channelType) === channelType
    );
  }

  const nextPreset = {
    id: existingIndex >= 0 ? presets[existingIndex].id : buildSortPresetId(),
    name,
    channelType,
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
  // 拉当前 tab 全量渠道（不受分页限制），确保排序覆盖全部可用渠道
  showSortModalAsync();
}

async function showSortModalAsync() {
  const modal = document.getElementById('sortModal');
  if (!modal) return;

  const loadContext = createSortModalLoadContext();
  if (!loadContext.channelType) {
    window.showNotification(window.t('channels.sortTypeRequired'), 'warning');
    return;
  }

  let sourceChannels = [];
  try {
    const params = new URLSearchParams();
    params.set('type', loadContext.channelType);
    params.set('limit', '1000');
    params.set('offset', '0');
    if (typeof channelStatsRange !== 'undefined' && channelStatsRange) {
      params.set('range', channelStatsRange);
    }
    const listBase = (typeof channelsReadURL === 'function')
      ? channelsReadURL('/admin/channels', '/dashboard/channels')
      : '/admin/channels';
    const resp = await fetchAPIWithAuth(listBase + '?' + params.toString());
    if (!resp || !resp.success) {
      throw new Error((resp && resp.error) || window.t('channels.loadChannelsFailed'));
    }
    sourceChannels = Array.isArray(resp.data) ? resp.data : [];
  } catch (error) {
    if (!isCurrentSortModalLoad(loadContext)) return;
    console.error('Load channels for sort failed:', error);
    // 降级：使用当前页列表
    sourceChannels = getChannelsForCurrentSortType();
  }

  // 请求等待期间若切换了 tab、再次打开或关闭弹窗，丢弃过期结果。
  if (!isCurrentSortModalLoad(loadContext)) return;

  if (!sourceChannels || sourceChannels.length === 0) {
    window.showError(window.t('channels.noChannelsForSort'));
    return;
  }

  // 每次打开以当前协议顺序为准；恢复该类型上次使用的本地方案（若有）
  sortChannelType = loadContext.channelType;
  sortChannels = [...sourceChannels];
  activeSortPresetId = getActiveSortPresetIdForType(sortChannelType);
  const activePreset = getActiveSortPreset();
  if (activePreset) {
    sortChannels = orderListByPreset(sortChannels, activePreset);
  } else {
    activeSortPresetId = '';
  }

  refreshSortPresetSelect();
  renderSortList();
  modal.classList.add('show');
}

// 关闭排序模态框
function closeSortModal() {
  sortModalLoadRequestId += 1;
  const modal = document.getElementById('sortModal');
  if (modal) {
    modal.classList.remove('show');
  }
  sortChannels = [];
  draggedItem = null;
  sortChannelType = '';
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

  // 排序卡片由 JS 直接使用 window.t 渲染；不要全页 translatePage，
  // 否则会覆盖弹窗标题/范围里的动态类型名称。
  updateSortModalScopeUI();
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

// 保存排序：只写当前类型渠道的 priority，不影响其他类型
async function saveSortOrder() {
  if (sortChannels.length === 0) {
    window.showNotification(window.t('channels.sortNoChanges'), 'warning');
    return;
  }

  const channelType = normalizeSortChannelType(sortChannelType || getCurrentSortChannelType());
  if (!channelType) {
    window.showNotification(window.t('channels.sortTypeRequired'), 'warning');
    return;
  }

  // sortChannels 打开弹窗时已按当前 tab 可用列表锁定；这里保存整表顺序。
  // 不要再按底层 channel_type 过滤，否则协议转换渠道会被丢掉。
  const updates = buildPriorityUpdatesFromOrder(sortChannels);
  if (updates.length === 0) {
    window.showNotification(window.t('channels.sortNoChanges'), 'warning');
    return;
  }

  try {
    await fetchDataWithAuth('/admin/channels/batch-priority', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json'
      },
      body: JSON.stringify({ protocol: channelType, updates })
    });

    window.showSuccess(window.t('channels.sortSaveSuccessType', { type: getSortChannelTypeLabel(channelType) })
      || window.t('channels.sortSaveSuccess'));
    closeSortModal();
    if (typeof loadChannels === 'function') await loadChannels(channelType);
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
