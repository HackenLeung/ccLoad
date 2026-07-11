function highlightFromHash() {
  const m = (location.hash || '').match(/^#channel-(\d+)$/);
  if (!m) return;
  const el = document.getElementById(`channel-${m[1]}`);
  if (!el) return;
  el.scrollIntoView({ behavior: 'smooth', block: 'center' });
  const prev = el.style.boxShadow;
  el.style.transition = 'box-shadow 0.3s ease, background 0.3s ease';
  el.style.boxShadow = '0 0 0 3px rgba(59,130,246,0.35), 0 10px 25px rgba(59,130,246,0.20)';
  el.style.background = 'rgba(59,130,246,0.06)';
  setTimeout(() => {
    el.style.boxShadow = prev || '';
    el.style.background = '';
  }, 1600);
}

async function getTargetChannel() {
  const params = new URLSearchParams(location.search);
  const channelId = params.get('id');
  if (!channelId) return null;

  try {
    return await fetchDataWithAuth(`/admin/channels/${channelId}`);
  } catch (e) {
    console.error('Failed to get target channel:', e);
    return null;
  }
}

const CHANNELS_FILTER_KEY = 'channels.filters';

function saveChannelsFilters() {
  try {
    localStorage.setItem(CHANNELS_FILTER_KEY, JSON.stringify({
      channelType: filters.channelType,
      status: filters.status,
      model: filters.model,
      modelExact: filters.modelExact,
      search: filters.search,
      searchExact: filters.searchExact,
      page: channelsCurrentPage
    }));
  } catch (_) {}
}

function loadChannelsFilters() {
  try {
    const saved = localStorage.getItem(CHANNELS_FILTER_KEY);
    if (saved) return JSON.parse(saved);
  } catch (_) {}
  return null;
}

// ==================== 渠道类型标签切换器 ====================
// 渠道页专用（不复用共享 initChannelTypeFilter：后者含"全部"项，供 logs/stats/trend 使用）。
// 后端路由本就按类型隔离 priority，这里让 UI 只显示单一类型，使类型内排序真实可读。
let channelTypeTabList = [];       // [{value, display_name, description}]
let channelTypeTabOnChange = null; // 切换回调

function getFirstChannelTypeValue() {
  return (channelTypeTabList[0] && channelTypeTabList[0].value) || 'anthropic';
}

// 将历史遗留/非法的类型（含 'all'）归一到一个合法类型
function normalizeChannelTabType(type) {
  const t = String(type || '').trim();
  if (t && t !== 'all' && channelTypeTabList.some((x) => x.value === t)) return t;
  return getFirstChannelTypeValue();
}

async function renderChannelTypeTabs(containerId, activeType, onChange) {
  const container = document.getElementById(containerId);
  if (!container) return normalizeChannelTabType(activeType);
  channelTypeTabList = (await window.ChannelTypeManager.getChannelTypes()) || [];
  channelTypeTabOnChange = onChange;
  const active = normalizeChannelTabType(activeType);
  paintChannelTypeTabs(active);
  return active;
}

function paintChannelTypeTabs(activeType) {
  const container = document.getElementById('channelTypeTabs');
  if (!container) return;
  const active = normalizeChannelTabType(activeType);
  container.innerHTML = '';
  channelTypeTabList.forEach((type) => {
    const btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'channel-type-tab' + (type.value === active ? ' active' : '');
    btn.dataset.channelType = type.value;
    btn.textContent = type.display_name;
    if (type.description) btn.title = type.description;
    btn.setAttribute('role', 'tab');
    btn.setAttribute('aria-selected', type.value === active ? 'true' : 'false');
    btn.addEventListener('click', () => {
      if (btn.classList.contains('active')) return;
      setActiveChannelTypeTab(type.value);
      if (typeof channelTypeTabOnChange === 'function') channelTypeTabOnChange(type.value);
    });
    container.appendChild(btn);
  });
}

// 仅更新标签高亮态（不触发回调），供外部（如新增渠道后切换类型）调用
function setActiveChannelTypeTab(type) {
  const active = normalizeChannelTabType(type);
  const container = document.getElementById('channelTypeTabs');
  if (!container) return;
  container.querySelectorAll('.channel-type-tab').forEach((btn) => {
    const on = btn.dataset.channelType === active;
    btn.classList.toggle('active', on);
    btn.setAttribute('aria-selected', on ? 'true' : 'false');
  });
}

function resetChannelSearchFilter() {
  filters.search = '';
  filters.searchExact = false;
  channelsCurrentPage = 1;
  if (typeof channelNameCombobox !== 'undefined' && channelNameCombobox) {
    channelNameCombobox.setValue('', getChannelNameAllLabel());
  } else {
    const searchInputEl = document.getElementById('searchInput');
    if (searchInputEl) {
      const allLabel = (window.t && window.t('channels.channelNameAll')) || '所有渠道';
      searchInputEl.value = allLabel;
    }
  }
}

function updateChannelsPagination() {
  const currentPageEl = document.getElementById('channels_current_page');
  const totalPagesEl = document.getElementById('channels_total_pages');
  const firstBtn = document.getElementById('channels_first_page');
  const prevBtn = document.getElementById('channels_prev_page');
  const nextBtn = document.getElementById('channels_next_page');
  const lastBtn = document.getElementById('channels_last_page');

  if (currentPageEl) currentPageEl.textContent = String(channelsCurrentPage);
  if (totalPagesEl) totalPagesEl.textContent = String(channelsTotalPages);

  const disablePrev = channelsCurrentPage <= 1;
  const disableNext = channelsCurrentPage >= channelsTotalPages;
  if (firstBtn) firstBtn.disabled = disablePrev;
  if (prevBtn) prevBtn.disabled = disablePrev;
  if (nextBtn) nextBtn.disabled = disableNext;
  if (lastBtn) lastBtn.disabled = disableNext;
}

function firstChannelsPage() {
  if (channelsCurrentPage <= 1) return;
  channelsCurrentPage = 1;
  saveChannelsFilters();
  loadChannels(filters.channelType);
}

function prevChannelsPage() {
  if (channelsCurrentPage <= 1) return;
  channelsCurrentPage--;
  saveChannelsFilters();
  loadChannels(filters.channelType);
}

function nextChannelsPage() {
  if (channelsCurrentPage >= channelsTotalPages) return;
  channelsCurrentPage++;
  saveChannelsFilters();
  loadChannels(filters.channelType);
}

function lastChannelsPage() {
  if (channelsCurrentPage >= channelsTotalPages) return;
  channelsCurrentPage = channelsTotalPages;
  saveChannelsFilters();
  loadChannels(filters.channelType);
}

function jumpChannelsPage() {
  const input = document.getElementById('channels_jump_page');
  if (!input) return;
  const page = parseInt(input.value, 10);
  if (!Number.isFinite(page) || page < 1 || page > channelsTotalPages) {
    input.value = '';
    return;
  }
  if (page !== channelsCurrentPage) {
    channelsCurrentPage = page;
    saveChannelsFilters();
    loadChannels(filters.channelType);
  }
  input.value = '';
}

function initChannelsPageActions() {
  if (typeof initChannelEditorActions === 'function') {
    initChannelEditorActions();
  }

  if (typeof window.initDelegatedActions === 'function') {
    window.initDelegatedActions({
      boundKey: 'channelsPageActionsBound',
      click: {
        'show-add-modal': () => showAddModal(),
        'first-channels-page': () => firstChannelsPage(),
        'prev-channels-page': () => prevChannelsPage(),
        'next-channels-page': () => nextChannelsPage(),
        'last-channels-page': () => lastChannelsPage(),
        'batch-enable-channels': () => batchEnableSelectedChannels(),
        'batch-disable-channels': () => batchDisableSelectedChannels(),
        'batch-delete-channels': () => batchDeleteSelectedChannels(),
        'batch-refresh-channels-merge': () => batchRefreshSelectedChannelsMerge(),
        'batch-refresh-channels-replace': () => batchRefreshSelectedChannelsReplace(),
        'clear-selected-channels': () => clearSelectedChannels(),
        'close-test-modal': () => closeTestModal(),
        'run-channel-test': () => runChannelTest(),
        'run-batch-test': () => runBatchTest(),
        'show-upstream-detail': () => window.UpstreamDetailModal?.show(window._lastTestUpstreamData),
        'close-upstream-detail': () => window.UpstreamDetailModal?.close(),
        'close-sort-modal': () => closeSortModal(),
        'save-sort-order': () => saveSortOrder(),
        'save-sort-preset-from-modal': () => saveSortPresetFromModal(),
        'delete-sort-preset-from-modal': () => deleteActiveSortPreset(),
        'pin-sort-channel': (actionTarget) => pinSortChannel(actionTarget.dataset.channelId),
        'toggle-response': (actionTarget) => {
          const responseTarget = actionTarget.dataset.responseTarget;
          if (responseTarget && typeof window.toggleResponse === 'function') {
            window.toggleResponse(responseTarget);
          }
        }
      },
      change: {
        'update-test-url': () => updateTestURL()
      }
    });
  }

  const jumpPageInput = document.getElementById('channels_jump_page');
  if (jumpPageInput && !jumpPageInput.dataset.bound) {
    jumpPageInput.addEventListener('keydown', (event) => {
      if (event.key === 'Enter') {
        jumpChannelsPage();
      }
    });
    jumpPageInput.dataset.bound = '1';
  }

  // 每页显示数量选择器
  const pageSizeSelect = document.getElementById('channels_page_size');
  if (pageSizeSelect && !pageSizeSelect.dataset.bound) {
    pageSizeSelect.value = String(channelsPageSize);
    pageSizeSelect.addEventListener('change', (event) => {
      const newSize = parseInt(event.target.value, 10);
      if (newSize > 0) {
        channelsPageSize = newSize;
        localStorage.setItem('channels.pageSize', String(newSize));
        channelsCurrentPage = 1;
        saveChannelsFilters();
        loadChannels(filters.channelType);
      }
    });
    pageSizeSelect.dataset.bound = '1';
  }
}

window.initPageBootstrap({
  topbarKey: 'channels',
  run: async () => {
    initChannelsPageActions();
    setupFilterListeners();
    setupImportExport();
    setupKeyImportPreview();
    setupModelImportPreview();
    if (typeof initChannelFormDirtyTracking === 'function') {
      initChannelFormDirtyTracking();
    }
    if (typeof updateBatchChannelSelectionUI === 'function') {
      updateBatchChannelSelectionUI();
    }

    // 并行化第一批：渠道类型渲染、目标渠道查询、不依赖 initialType 的设置请求同时发起
    const savedFilters = loadChannelsFilters();
    channelsCurrentPage = Math.max(1, parseInt(savedFilters?.page, 10) || 1);
    const [, targetChannel] = await Promise.all([
      window.ChannelTypeManager.renderChannelTypeRadios('channelTypeRadios'),
      getTargetChannel(),
      loadDefaultTestContent(),
      loadChannelStatsRange()
    ]);
    const targetChannelType = targetChannel?.channel_type || null;
    // 原始候选类型（可能为 'all' 或历史遗留值）；实际生效类型在标签渲染后归一
    const initialTypeRaw = targetChannelType || (savedFilters?.channelType) || '';

    filters.channelType = initialTypeRaw;
    const urlChannelId = new URLSearchParams(location.search).get('id');
    if (urlChannelId) {
      filters.status = 'all';
      filters.model = 'all';
      filters.modelExact = false;
      filters.search = targetChannel?.name || '';
      filters.searchExact = Boolean(filters.search);
      channelsCurrentPage = 1;
      document.getElementById('statusFilter').value = 'all';
      if (typeof modelFilterCombobox !== 'undefined' && modelFilterCombobox) {
        modelFilterCombobox.setValue('all', modelFilterInputValueFromFilterValue('all'));
      } else {
        const modelFilterEl = document.getElementById('modelFilter');
        if (modelFilterEl) modelFilterEl.value = modelFilterInputValueFromFilterValue('all');
      }
      const searchInputEl = document.getElementById('searchInput');
      if (searchInputEl) {
        const allLabel = (window.t && window.t('channels.channelNameAll')) || '所有渠道';
        searchInputEl.value = filters.search || allLabel;
      }
    } else if (savedFilters) {
      filters.status = savedFilters.status || 'all';
      filters.model = savedFilters.model || 'all';
      filters.modelExact = filters.model !== 'all' && savedFilters.modelExact !== false;
      filters.search = savedFilters.search || '';
      filters.searchExact = savedFilters.searchExact === true;
      document.getElementById('statusFilter').value = filters.status;
      if (typeof modelFilterCombobox !== 'undefined' && modelFilterCombobox) {
        modelFilterCombobox.setValue(filters.model, modelFilterInputValueFromFilterValue(filters.model));
      } else {
        const modelFilterEl = document.getElementById('modelFilter');
        if (modelFilterEl) modelFilterEl.value = modelFilterInputValueFromFilterValue(filters.model);
      }
      if (typeof channelNameCombobox !== 'undefined' && channelNameCombobox) {
        const allLabel = (typeof getChannelNameAllLabel === 'function')
          ? getChannelNameAllLabel()
          : ((window.t && window.t('channels.channelNameAll')) || '所有渠道');
        channelNameCombobox.setValue(filters.search, filters.search || allLabel);
      } else {
        const searchInputEl = document.getElementById('searchInput');
        if (searchInputEl) {
          const allLabel = (window.t && window.t('channels.channelNameAll')) || '所有渠道';
          searchInputEl.value = filters.search || allLabel;
        }
      }
      saveChannelsFilters();
    }

    // 并行化第二批：渲染类型标签（含归一化）+ stats，之后用归一后的类型加载列表
    const [initialType] = await Promise.all([
      renderChannelTypeTabs('channelTypeTabs', initialTypeRaw, (type) => {
        filters.channelType = type;
        filters.model = 'all';
        filters.modelExact = false;
        filters.search = '';
        filters.searchExact = false;
        channelsCurrentPage = 1;
        if (typeof modelFilterCombobox !== 'undefined' && modelFilterCombobox) {
          modelFilterCombobox.setValue('all', modelFilterInputValueFromFilterValue('all'));
        } else {
          const modelFilterEl = document.getElementById('modelFilter');
          if (modelFilterEl) modelFilterEl.value = modelFilterInputValueFromFilterValue('all');
        }
        if (typeof channelNameCombobox !== 'undefined' && channelNameCombobox) {
          channelNameCombobox.setValue('', getChannelNameAllLabel());
        }
        saveChannelsFilters();
        loadChannelsFilterOptions(type, filters.status);
        loadChannels(type);
      }),
      loadChannelStats()
    ]);
    // initialType 已是归一后的合法类型（不含 'all'）
    filters.channelType = initialType;
    saveChannelsFilters();
    await Promise.all([
      loadChannelsFilterOptions(initialType, filters.status),
      loadChannels(initialType)
    ]);
    highlightFromHash();
    window.addEventListener('hashchange', highlightFromHash);

    window.i18n.onLocaleChange(() => {
      paintChannelTypeTabs(filters.channelType);
      renderChannels();
      updateModelOptions();
      updateChannelsPagination();
    });

    // 自动刷新（system_settings.auto_refresh_interval_seconds，0=禁用）
    // 通过 .modal.show 检测跳过编辑/批量/排序等对话框打开期间的刷新，避免丢失未保存内容
    if (typeof window.createAutoRefresh === 'function') {
      window.createAutoRefresh({
        load: () => Promise.all([
          loadChannels(filters.channelType || 'all'),
          loadChannelStats()
        ])
      }).init();
    }
  }
});

document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape') {
    const customRulesModal = document.getElementById('customRulesModal');
    const modelImportModal = document.getElementById('modelImportModal');
    const keyImportModal = document.getElementById('keyImportModal');
    const keyExportModal = document.getElementById('keyExportModal');
    const deleteModal = document.getElementById('deleteModal');
    const testModal = document.getElementById('testModal');
    const channelModal = document.getElementById('channelModal');

    if (customRulesModal && customRulesModal.classList.contains('show')) {
      closeCustomRulesModal();
    } else if (modelImportModal && modelImportModal.classList.contains('show')) {
      closeModelImportModal();
    } else if (keyImportModal && keyImportModal.classList.contains('show')) {
      closeKeyImportModal();
    } else if (keyExportModal && keyExportModal.classList.contains('show')) {
      closeKeyExportModal();
    } else if (deleteModal && deleteModal.classList.contains('show')) {
      closeDeleteModal();
    } else if (testModal && testModal.classList.contains('show')) {
      closeTestModal();
    } else if (channelModal && channelModal.classList.contains('show')) {
      closeModal();
    }
  }
});

window.addEventListener('pageshow', async (event) => {
  const urlChannelId = new URLSearchParams(location.search).get('id');
  if (!event.persisted || urlChannelId) return;

  resetChannelSearchFilter();
  if (typeof saveChannelsFilters === 'function') saveChannelsFilters();
  await loadChannels(filters.channelType || 'all');
});
