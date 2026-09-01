    // 统计数据管理
    let statsData = {
      total_requests: 0,
      success_requests: 0,
      error_requests: 0,
      active_channels: 0,
      active_models: 0,
      duration_seconds: 1,
      rpm_stats: null,
      today_tokens: 0,
      cumulative_tokens: 0,
      recent_tpm: 0,
      avg_response_seconds: 0,
      is_today: true
    };

    // 当前选中的时间范围
    let currentTimeRange = 'today';
    let currentCustomTimeRange = null;

    function buildSummaryURL(forceRefresh) {
      const query = typeof window.buildDateRangeQuery === 'function'
        ? window.buildDateRangeQuery(currentTimeRange, currentCustomTimeRange)
        : `range=${encodeURIComponent(currentTimeRange)}`;
      // 累计统计走后端 1 小时缓存；用户主动加载/切换范围时强制取最新值。
      const suffix = forceRefresh ? '&refresh_cumulative=1' : '';
      return `/dashboard/summary?${query}${suffix}`;
    }

    // 加载统计数据。forceRefresh=true 表示用户主动触发（首屏/切换范围），强制刷新累计值；
    // 自动刷新不传，读后端 1 小时缓存，避免频繁全表聚合。
    async function loadStats(forceRefresh = false) {
      try {
        // 添加加载状态
        document.querySelectorAll('.metric-number').forEach(el => {
          el.classList.add('animate-pulse');
        });

        const data = await fetchDataWithAuth(buildSummaryURL(forceRefresh));
        statsData = data || statsData;
        updateStatsDisplay();

      } catch (error) {
        console.error('Failed to load stats:', error);
        showError('无法加载统计数据');
      } finally {
        // 移除加载状态
        document.querySelectorAll('.metric-number').forEach(el => {
          el.classList.remove('animate-pulse');
        });
      }
    }

    // 更新统计显示
    function updateStatsDisplay() {
      const successRate = statsData.total_requests > 0
        ? ((statsData.success_requests / statsData.total_requests) * 100).toFixed(1)
        : '0.0';

      // 更新总体数字显示（成功/失败合并显示）
      document.getElementById('success-requests').textContent = formatNumber(statsData.success_requests || 0);
      document.getElementById('error-requests').textContent = formatNumber(statsData.error_requests || 0);
      document.getElementById('success-rate').textContent = successRate + '%';

      document.getElementById('overview-today-tokens').textContent = formatNumber(statsData.today_tokens || 0);
      document.getElementById('overview-cumulative-tokens').textContent = formatNumber(statsData.cumulative_tokens || 0);
      document.getElementById('overview-tpm').textContent = formatNumber(statsData.recent_tpm || 0);
      document.getElementById('overview-avg-response').textContent = formatResponseTime(statsData.avg_response_seconds);

      const rpmStats = statsData.rpm_stats || null;
      document.getElementById('overview-rpm').textContent = formatNumber(rpmStats ? (rpmStats.recent_rpm || 0) : 0);

      // 更新按渠道类型统计
      if (statsData.by_type) {
        updateTypeStats('anthropic', statsData.by_type.anthropic);
        updateTypeStats('codex', statsData.by_type.codex);
        updateTypeStats('openai', statsData.by_type.openai);
        updateTypeStats('gemini', statsData.by_type.gemini);
      }
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

      const successRate = totalRequests > 0
        ? ((successRequests / totalRequests) * 100).toFixed(1)
        : '0.0';

      // 更新基础统计（总请求、成功、失败、成功率）
      document.getElementById(`type-${type}-requests`).textContent = formatNumber(totalRequests);
      document.getElementById(`type-${type}-success`).textContent = formatNumber(successRequests);
      document.getElementById(`type-${type}-error`).textContent = formatNumber(errorRequests);
      document.getElementById(`type-${type}-rate`).textContent = successRate + '%';
      document.getElementById(`type-${type}-today-tokens`).textContent = formatNumber(data ? (data.today_tokens || 0) : 0);
      document.getElementById(`type-${type}-cumulative-tokens`).textContent = formatNumber(data ? (data.cumulative_tokens || 0) : 0);

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

      document.getElementById(`type-${type}-input`).textContent = formatNumber(inputTokens);
      document.getElementById(`type-${type}-output`).textContent = formatNumber(outputTokens);
      document.getElementById(`type-${type}-cost`).innerHTML = buildCostStackHtml(totalCost, effectiveCost, { tone: 'warning', inline: true });
      document.getElementById(`type-${type}-cumulative-cost`).innerHTML = buildCostStackHtml(cumulativeCost, cumulativeEffectiveCost, { tone: 'warning', inline: true });

      // Claude和Codex类型的缓存统计（缓存读+缓存创建）
      if (type === 'anthropic' || type === 'codex') {
        const cacheReadTokens = data ? (data.total_cache_read_tokens || 0) : 0;
        const cacheCreateTokens = data ? (data.total_cache_creation_tokens || 0) : 0;
        document.getElementById(`type-${type}-cache-read`).textContent = formatNumber(cacheReadTokens);
        document.getElementById(`type-${type}-cache-create`).textContent = formatNumber(cacheCreateTokens);
      }

      // OpenAI和Gemini类型的缓存统计（仅缓存读）
      if (type === 'openai' || type === 'gemini') {
        const cacheReadTokens = data ? (data.total_cache_read_tokens || 0) : 0;
        document.getElementById(`type-${type}-cache-read`).textContent = formatNumber(cacheReadTokens);
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
          loadStats(true);
        }
      });

      // 加载统计数据（首屏为用户主动动作，强制刷新累计值）
      loadStats(true);

      // 自动刷新（system_settings.auto_refresh_interval_seconds，0=禁用）
      if (typeof window.createAutoRefresh === 'function') {
        window.createAutoRefresh({ load: loadStats }).init();
      }

      // 添加页面动画
      document.querySelectorAll('.animate-slide-up').forEach((el, index) => {
        el.style.animationDelay = `${index * 0.1}s`;
      });
      }
    });
