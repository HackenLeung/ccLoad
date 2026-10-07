    // 统计数据管理
    let statsData = {
      total_requests: 0,
      success_requests: 0,
      error_requests: 0,
      active_channels: 0,
      active_models: 0,
      duration_seconds: 1,
      rpm_stats: null,
      range_tokens: 0,
      cumulative_tokens: 0,
      recent_tpm: 0,
      avg_response_seconds: 0,
      is_today: true
    };

    // 当前选中的时间范围
    let currentTimeRange = 'today';
    let currentCustomTimeRange = null;
    let indexLoadSequence = 0;
    let indexLoadPending = false;
    let cumulativeRefreshPending = false;
    const indexMetricSelector = '.index-overview-grid .summary-value, .channel-card .metric-value, .channel-token-value, .cost-value, .token-value, .index-summary-grid .summary-value';

    function buildSummaryURL(forceRefresh) {
      const query = typeof window.buildDateRangeQuery === 'function'
        ? window.buildDateRangeQuery(currentTimeRange, currentCustomTimeRange)
        : `range=${encodeURIComponent(currentTimeRange)}`;
      // 累计统计不随日期筛选变化，仅专用刷新按钮绕过缓存。
      const suffix = forceRefresh ? '&refresh_cumulative=1' : '';
      return `/dashboard/summary?${query}${suffix}`;
    }

    async function loadStats(forceRefresh = false, background = false) {
      if (background && indexLoadPending) return;
      if (forceRefresh && cumulativeRefreshPending) return;
      const requestID = ++indexLoadSequence;
      indexLoadPending = true;
      const refreshButton = document.getElementById('refresh-cumulative');
      if (forceRefresh) {
        cumulativeRefreshPending = true;
        if (refreshButton) refreshButton.disabled = true;
        window.updateRefreshStatus('cumulative-refresh-status', 'loading');
      }
      try {
        window.updateRefreshStatus('index-refresh-status', 'loading');
        // 后台轮询只更新状态文字，避免整页数字闪动。
        if (!background) {
          document.querySelectorAll(indexMetricSelector).forEach(el => {
            el.classList.add('animate-pulse');
          });
        }
        document.querySelector('.index-main-content')?.setAttribute('aria-busy', 'true');

        const data = await fetchDataWithAuth(buildSummaryURL(forceRefresh));
        if (requestID !== indexLoadSequence) return;
        statsData = data || statsData;
        updateStatsDisplay();
        window.updateRefreshStatus('index-refresh-status', 'success', Date.now());
        window.updateRefreshStatus('cumulative-refresh-status', 'success', data?.cumulative_updated_at);

      } catch (error) {
        if (requestID !== indexLoadSequence) return;
        console.error('Failed to load stats:', error);
        window.updateRefreshStatus('index-refresh-status', 'error');
        if (forceRefresh) window.updateRefreshStatus('cumulative-refresh-status', 'error');
      } finally {
        if (forceRefresh) {
          cumulativeRefreshPending = false;
          if (refreshButton) refreshButton.disabled = false;
        }
        if (requestID === indexLoadSequence) {
          indexLoadPending = false;
          // 移除加载状态
          document.querySelectorAll(indexMetricSelector).forEach(el => {
            el.classList.remove('animate-pulse');
          });
          document.querySelector('.index-main-content')?.setAttribute('aria-busy', 'false');
        }
      }
    }

    // 更新统计显示
    function updateStatsDisplay() {
      const range = statsData.range || currentTimeRange;
      const isToday = statsData.is_today === true;
      const rangeLabel = window.getRangeLabel(range);
      document.querySelectorAll('[data-index-range-tokens]').forEach(el => {
        el.textContent = t('index.metrics.rangeTokens', { range: rangeLabel });
        el.title = range === 'custom' ? (currentCustomTimeRange?.label || rangeLabel) : rangeLabel;
      });
      document.getElementById('overview-performance-label').textContent = t(isToday
        ? 'index.metrics.recentPerformance' : 'index.metrics.averagePerformance');

      // 更新总体数字显示（成功/失败合并显示）
      displayNumber('success-requests', statsData.success_requests);
      displayNumber('error-requests', statsData.error_requests);
      document.getElementById('success-rate').textContent = formatSuccessRate(statsData.success_requests, statsData.total_requests);

      displayNumber('overview-range-tokens', statsData.range_tokens);
      displayNumber('overview-cumulative-tokens', statsData.cumulative_tokens);
      const averageTPM = (Number(statsData.range_tokens) || 0) * 60 / Math.max(1, Number(statsData.duration_seconds) || 1);
      displayNumber('overview-tpm', isToday ? statsData.recent_tpm : averageTPM, true);
      document.getElementById('overview-avg-response').textContent = formatResponseTime(statsData.avg_response_seconds);

      const rpmStats = statsData.rpm_stats || null;
      displayNumber('overview-rpm', isToday ? rpmStats?.recent_rpm : rpmStats?.avg_rpm, true);

      // 更新按渠道类型统计
      for (const type of ['anthropic', 'codex', 'openai', 'gemini']) {
        updateTypeStats(type, statsData.by_type?.[type]);
      }
    }

    function displayNumber(id, value, rate = false) {
      const el = document.getElementById(id);
      const parsed = Number(value);
      const number = Number.isFinite(parsed) ? parsed : 0;
      el.textContent = formatNumber(rate ? Number(number.toFixed(1)) : number);
      el.title = number.toLocaleString(undefined, { maximumFractionDigits: rate ? 3 : 0 });
    }

    function formatSuccessRate(success, total) {
      return total > 0 ? `${((success / total) * 100).toFixed(1)}%` : '--';
    }

    function formatResponseTime(seconds) {
      const value = Number(seconds);
      if (!Number.isFinite(value) || value <= 0) return '--';
      if (value < 1) return `${Math.round(value * 1000)}ms`;
      return `${value < 10 ? value.toFixed(2) : value.toFixed(1)}s`;
    }

    // 更新单个渠道类型的统计
    function updateTypeStats(type, data) {
      // 始终显示所有卡片，保持界面完整性
      const card = document.getElementById(`type-${type}-card`);
      if (card) card.style.display = 'block';

      // 如果没有数据，显示默认值
      const totalRequests = data ? (data.total_requests || 0) : 0;
      const successRequests = data ? (data.success_requests || 0) : 0;
      const errorRequests = data ? (data.error_requests || 0) : 0;

      // 更新基础统计（总请求、成功、失败、成功率）
      displayNumber(`type-${type}-requests`, totalRequests);
      displayNumber(`type-${type}-success`, successRequests);
      displayNumber(`type-${type}-error`, errorRequests);
      document.getElementById(`type-${type}-rate`).textContent = formatSuccessRate(successRequests, totalRequests);
      displayNumber(`type-${type}-range-tokens`, data?.range_tokens);
      displayNumber(`type-${type}-cumulative-tokens`, data?.cumulative_tokens);

      // 所有渠道类型的Token和成本统计
      const inputTokens = data ? (data.total_input_tokens || 0) : 0;
      const outputTokens = data ? (data.total_output_tokens || 0) : 0;
      const totalCost = data ? (data.total_cost || 0) : 0;
      const effectiveCost = data && data.effective_cost !== undefined && data.effective_cost !== null
        ? Number(data.effective_cost) || 0
        : totalCost;
      const cumulativeCost = data ? (data.cumulative_cost || 0) : 0;
      const cumulativeEffectiveCost = data && data.cumulative_effective_cost !== undefined && data.cumulative_effective_cost !== null
        ? Number(data.cumulative_effective_cost) || 0
        : cumulativeCost;

      displayNumber(`type-${type}-input`, inputTokens);
      displayNumber(`type-${type}-output`, outputTokens);
      document.getElementById(`type-${type}-cost`).innerHTML = buildCostStackHtml(totalCost, effectiveCost, { tone: 'warning', inline: true });
      document.getElementById(`type-${type}-cumulative-cost`).innerHTML = buildCostStackHtml(cumulativeCost, cumulativeEffectiveCost, { tone: 'warning', inline: true });

      // Claude和Codex类型的缓存统计（缓存读+缓存创建）
      if (type === 'anthropic' || type === 'codex') {
        const cacheReadTokens = data ? (data.total_cache_read_tokens || 0) : 0;
        const cacheCreateTokens = data ? (data.total_cache_creation_tokens || 0) : 0;
        displayNumber(`type-${type}-cache-read`, cacheReadTokens);
        displayNumber(`type-${type}-cache-create`, cacheCreateTokens);
      }

      // OpenAI和Gemini类型的缓存统计（仅缓存读）
      if (type === 'openai' || type === 'gemini') {
        const cacheReadTokens = data ? (data.total_cache_read_tokens || 0) : 0;
        displayNumber(`type-${type}-cache-read`, cacheReadTokens);
      }
    }

    // 通知系统统一由 ui.js 提供（showSuccess/showError/showNotification）

    // 注销功能（已由 ui.js 的 onLogout 统一处理）

    // 自动刷新由 createAutoRefresh 统一管理（system_settings.auto_refresh_interval_seconds）

    // 页面初始化
    window.initPageBootstrap({
      topbarKey: 'index',
      run: () => {
      window.bindTimeRangeSelector({
        containerId: 'index-time-range',
        values: ['today', 'yesterday', 'day_before_yesterday', 'this_week', 'last_week', 'this_month', 'last_month', 'custom'],
        initialValue: currentTimeRange,
        customRange: currentCustomTimeRange,
        onChange: (range, customRange) => {
          currentTimeRange = range;
          if (range === 'custom') currentCustomTimeRange = customRange;
          loadStats();
        }
      });

      document.getElementById('refresh-cumulative')?.addEventListener('click', () => loadStats(true));
      window.i18n.onLocaleChange(updateStatsDisplay);
      loadStats();

      // 自动刷新（system_settings.auto_refresh_interval_seconds，0=禁用）
      if (typeof window.createAutoRefresh === 'function') {
        window.createAutoRefresh({ load: () => loadStats(false, true) }).init();
      }

      // 添加页面动画
      document.querySelectorAll('.animate-slide-up').forEach((el, index) => {
        el.style.animationDelay = `${index * 0.1}s`;
      });
      }
    });
