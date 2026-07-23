(function () {
  'use strict';

  let allModels = [];

  function modal() {
    return document.getElementById('visionPoolModal');
  }

  function escape(value) {
    if (typeof escapeHtml === 'function') return escapeHtml(String(value ?? ''));
    const span = document.createElement('span');
    span.textContent = String(value ?? '');
    return span.innerHTML;
  }

  function render() {
    const tbody = document.getElementById('visionPoolTableBody');
    const filter = (document.getElementById('visionPoolFilter')?.value || '').trim().toLowerCase();
    if (!tbody) return;
    const rows = allModels.filter(item => {
      if (!filter) return true;
      return String(item.channel_name || '').toLowerCase().includes(filter) ||
        String(item.model || '').toLowerCase().includes(filter);
    });
    document.getElementById('visionPoolCount').textContent = window.t('channels.visionPoolCount', {
      enabled: allModels.filter(item => item.vision_pool_enabled).length,
      total: allModels.length
    });
    if (rows.length === 0) {
      tbody.innerHTML = `<tr><td colspan="5" class="redirect-empty-cell">${escape(window.t('channels.visionPoolEmpty'))}</td></tr>`;
      return;
    }
    tbody.innerHTML = rows.map(item => {
	  const itemIndex = allModels.indexOf(item);
      const channelState = item.channel_enabled ? '' : ` <span class="vision-pool-muted">${escape(window.t('common.disabled'))}</span>`;
      const target = item.vision_assist_enabled
        ? `<span class="vision-assist-target-badge">${escape(window.t('channels.visionAssistEnabledShort'))}</span>`
        : '<span class="vision-pool-muted">-</span>';
      return `<tr data-vision-index="${itemIndex}">
        <td class="vision-pool-enabled-cell"><input class="vision-pool-enabled" type="checkbox" ${item.vision_pool_enabled ? 'checked' : ''}></td>
        <td>${escape(item.channel_name)}${channelState}</td>
        <td>${escape(item.model)}</td>
        <td><input class="form-input vision-pool-priority-input" type="number" min="0" step="1" value="${Number(item.vision_priority) || 0}" ${item.vision_pool_enabled ? '' : 'disabled'}></td>
        <td>${target}</td>
      </tr>`;
    }).join('');
  }

  function syncRowsToState() {
    document.querySelectorAll('#visionPoolTableBody tr[data-vision-index]').forEach(row => {
	  const item = allModels[Number.parseInt(row.dataset.visionIndex, 10)];
      if (!item) return;
      item.vision_pool_enabled = !!row.querySelector('.vision-pool-enabled')?.checked;
      item.vision_priority = Math.max(0, Number.parseInt(row.querySelector('.vision-pool-priority-input')?.value, 10) || 0);
    });
  }

  async function open() {
    const target = modal();
    if (!target) return;
    target.classList.add('show');
    document.body.style.overflow = 'hidden';
    const tbody = document.getElementById('visionPoolTableBody');
    if (tbody) tbody.innerHTML = `<tr><td colspan="5" class="redirect-empty-cell">${escape(window.t('common.loading'))}</td></tr>`;
    try {
      const data = await fetchDataWithAuth('/admin/channels/vision-pool');
      allModels = Array.isArray(data?.models) ? data.models.map(item => ({ ...item })) : [];
      render();
    } catch (error) {
      allModels = [];
      if (tbody) tbody.innerHTML = `<tr><td colspan="5" class="redirect-empty-cell">${escape(error.message || window.t('channels.visionPoolLoadFailed'))}</td></tr>`;
    }
  }

  function close() {
    modal()?.classList.remove('show');
    const filter = document.getElementById('visionPoolFilter');
    if (filter) filter.value = '';
    document.body.style.overflow = '';
  }

  async function save() {
    syncRowsToState();
    const button = document.getElementById('saveVisionPoolBtn');
    if (button) button.disabled = true;
    try {
      const response = await fetchAPIWithAuth('/admin/channels/vision-pool', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          items: allModels.filter(item => item.vision_pool_enabled).map(item => ({
            channel_id: item.channel_id,
            model: item.model,
            priority: Math.max(0, Number.parseInt(item.vision_priority, 10) || 0)
          }))
        })
      });
      if (!response.success) throw new Error(response.error || window.t('channels.visionPoolSaveFailed'));
      window.showNotification?.(window.t('channels.visionPoolSaved'), 'success');
      close();
      if (typeof loadChannels === 'function') await loadChannels();
    } catch (error) {
      window.showError?.(error.message || window.t('channels.visionPoolSaveFailed'));
    } finally {
      if (button) button.disabled = false;
    }
  }

  document.addEventListener('DOMContentLoaded', () => {
    document.getElementById('visionPoolBtn')?.addEventListener('click', open);
    document.getElementById('saveVisionPoolBtn')?.addEventListener('click', save);
    document.getElementById('visionPoolFilter')?.addEventListener('input', () => {
      syncRowsToState();
      render();
    });
    document.getElementById('visionPoolTableBody')?.addEventListener('change', event => {
      if (event.target.classList.contains('vision-pool-enabled')) {
        const row = event.target.closest('tr');
        const priority = row?.querySelector('.vision-pool-priority-input');
        if (priority) priority.disabled = !event.target.checked;
      }
    });
    document.querySelectorAll('[data-action="close-vision-pool-modal"]').forEach(button => button.addEventListener('click', close));
  });
})();
