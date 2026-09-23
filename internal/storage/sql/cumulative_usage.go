package sql

import (
	"context"
	"database/sql"
	"strings"

	"ccLoad/internal/model"
)

type cumulativeUsageWriter interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

type cumulativeUsageDelta struct {
	channelID         int64
	model             string
	statusCode        int
	authTokenID       int64
	clientName        string
	logSource         string
	totalRequests     int64
	successRequests   int64
	errorRequests     int64
	inputTokens       int64
	outputTokens      int64
	cacheReadTokens   int64
	cacheCreateTokens int64
	cost              float64
	effectiveCost     float64
}

// upsertCumulativeUsage 在日志写入的同一事务内累积更新累计统计维度。
//
// 口径（必须与 storage.backfillCumulativeUsageTx 保持一致）：
//   - 413/499 不计入 total/error（与统计页一致）
//   - cost_multiplier 负数归一为 1，0（免费渠道）保持不变
//   - client_name 截断到 model.ClientNameMaxLen，保证维度 key 稳定
func upsertCumulativeUsage(ctx context.Context, exec cumulativeUsageWriter, sqlite bool, entries []*model.LogEntry) error {
	deltas := make(map[string]*cumulativeUsageDelta)
	for _, entry := range entries {
		if entry == nil || entry.ChannelID <= 0 {
			continue
		}
		source := model.NormalizeStoredLogSource(entry.LogSource)
		clientName := model.TruncateColumnValue(entry.ClientName, model.ClientNameMaxLen)
		key := model.CumulativeUsageDimensionKey(entry.ChannelID, entry.Model, entry.StatusCode, entry.AuthTokenID, clientName, source)
		delta := deltas[key]
		if delta == nil {
			delta = &cumulativeUsageDelta{
				channelID: entry.ChannelID, model: entry.Model, statusCode: entry.StatusCode,
				authTokenID: entry.AuthTokenID, clientName: clientName, logSource: source,
			}
			deltas[key] = delta
		}
		if entry.StatusCode != 413 && entry.StatusCode != 499 {
			delta.totalRequests++
		}
		if entry.StatusCode >= 200 && entry.StatusCode < 300 {
			delta.successRequests++
		} else if (entry.StatusCode < 200 || entry.StatusCode >= 300) && entry.StatusCode != 413 && entry.StatusCode != 499 {
			delta.errorRequests++
		}
		delta.inputTokens += int64(entry.InputTokens)
		delta.outputTokens += int64(entry.OutputTokens)
		delta.cacheReadTokens += int64(entry.CacheReadInputTokens)
		delta.cacheCreateTokens += int64(entry.CacheCreationInputTokens)
		delta.cost += entry.Cost
		delta.effectiveCost += entry.Cost * normalizeCostMultiplier(entry.CostMultiplier)
	}

	if len(deltas) == 0 {
		return nil
	}
	query := `INSERT INTO cumulative_usage (
		dimension_key, channel_id, model, status_code, auth_token_id, client_name, log_source,
		total_requests, success_requests, error_requests, input_tokens, output_tokens,
		cache_read_tokens, cache_creation_tokens, cost, effective_cost
	) VALUES `
	updateClause := ` ON DUPLICATE KEY UPDATE
		total_requests = total_requests + VALUES(total_requests),
		success_requests = success_requests + VALUES(success_requests),
		error_requests = error_requests + VALUES(error_requests),
		input_tokens = input_tokens + VALUES(input_tokens),
		output_tokens = output_tokens + VALUES(output_tokens),
		cache_read_tokens = cache_read_tokens + VALUES(cache_read_tokens),
		cache_creation_tokens = cache_creation_tokens + VALUES(cache_creation_tokens),
		cost = cost + VALUES(cost), effective_cost = effective_cost + VALUES(effective_cost)`
	if sqlite {
		updateClause = ` ON CONFLICT(dimension_key) DO UPDATE SET
			total_requests = cumulative_usage.total_requests + excluded.total_requests,
			success_requests = cumulative_usage.success_requests + excluded.success_requests,
			error_requests = cumulative_usage.error_requests + excluded.error_requests,
			input_tokens = cumulative_usage.input_tokens + excluded.input_tokens,
			output_tokens = cumulative_usage.output_tokens + excluded.output_tokens,
			cache_read_tokens = cumulative_usage.cache_read_tokens + excluded.cache_read_tokens,
			cache_creation_tokens = cumulative_usage.cache_creation_tokens + excluded.cache_creation_tokens,
			cost = cumulative_usage.cost + excluded.cost,
			effective_cost = cumulative_usage.effective_cost + excluded.effective_cost`
	}
	items := make([]struct {
		key   string
		delta *cumulativeUsageDelta
	}, 0, len(deltas))
	for key, delta := range deltas {
		items = append(items, struct {
			key   string
			delta *cumulativeUsageDelta
		}{key: key, delta: delta})
	}
	const batchSize = 50
	for offset := 0; offset < len(items); offset += batchSize {
		end := min(offset+batchSize, len(items))
		args := make([]any, 0, (end-offset)*16)
		values := make([]string, 0, end-offset)
		for _, item := range items[offset:end] {
			values = append(values, "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
			delta := item.delta
			args = append(args, item.key, delta.channelID, delta.model, delta.statusCode, delta.authTokenID, delta.clientName, delta.logSource,
				delta.totalRequests, delta.successRequests, delta.errorRequests, delta.inputTokens, delta.outputTokens,
				delta.cacheReadTokens, delta.cacheCreateTokens, delta.cost, delta.effectiveCost)
		}
		if _, err := exec.ExecContext(ctx, query+strings.Join(values, ",")+updateClause, args...); err != nil {
			return err
		}
	}
	return nil
}

// GetCumulativeStats 按渠道+模型聚合永久累计统计（不受日志保留期影响）。
// filter 支持渠道类型/名称/模型等条件，与 logs 表的筛选语义保持一致。
func (s *SQLStore) GetCumulativeStats(ctx context.Context, filter *model.LogFilter) ([]model.StatsEntry, error) {
	base := `SELECT channel_id, COALESCE(model, ''),
		SUM(success_requests), SUM(error_requests), SUM(total_requests),
		SUM(input_tokens), SUM(output_tokens), SUM(cache_read_tokens), SUM(cache_creation_tokens),
		SUM(cost), SUM(effective_cost)
		FROM cumulative_usage`
	qb := NewQueryBuilder(base).Where("channel_id > 0")
	if _, isEmpty, err := s.applyChannelFilter(ctx, qb, filter); err != nil {
		return nil, err
	} else if isEmpty {
		return []model.StatsEntry{}, nil
	}
	qb.ApplyFilter(filter)
	query, args := qb.BuildWithSuffix("GROUP BY channel_id, model ORDER BY channel_id, model")
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	stats := make([]model.StatsEntry, 0)
	for rows.Next() {
		var entry model.StatsEntry
		var channelID int64
		var input, output, cacheRead, cacheCreate int64
		var cost, effectiveCost float64
		if err := rows.Scan(&channelID, &entry.Model, &entry.Success, &entry.Error, &entry.Total,
			&input, &output, &cacheRead, &cacheCreate, &cost, &effectiveCost); err != nil {
			return nil, err
		}
		id := int(channelID)
		entry.ChannelID = &id
		entry.TotalInputTokens = &input
		entry.TotalOutputTokens = &output
		entry.TotalCacheReadInputTokens = &cacheRead
		entry.TotalCacheCreationInputTokens = &cacheCreate
		entry.TotalCost = &cost
		entry.EffectiveCost = &effectiveCost
		stats = append(stats, entry)
	}
	return stats, rows.Err()
}

// ReplaceCumulativeUsage 用给定维度全量覆盖本表（混合模式下从 MySQL 主库恢复）。
func (s *SQLStore) ReplaceCumulativeUsage(ctx context.Context, rows []CumulativeUsageRecord) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "DELETE FROM cumulative_usage"); err != nil {
		return err
	}
	for _, row := range rows {
		if _, err := tx.ExecContext(ctx, `INSERT INTO cumulative_usage (
			dimension_key, channel_id, model, status_code, auth_token_id, client_name, log_source,
			total_requests, success_requests, error_requests, input_tokens, output_tokens,
			cache_read_tokens, cache_creation_tokens, cost, effective_cost
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			row.DimensionKey, row.ChannelID, row.Model, row.StatusCode, row.AuthTokenID, row.ClientName, row.LogSource,
			row.TotalRequests, row.SuccessRequests, row.ErrorRequests, row.InputTokens, row.OutputTokens,
			row.CacheReadTokens, row.CacheCreationTokens, row.Cost, row.EffectiveCost); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// CumulativeUsageRecord 是累计统计维度行，用于跨库（MySQL → SQLite）传输。
type CumulativeUsageRecord struct {
	DimensionKey        string
	ChannelID           int64
	Model               string
	StatusCode          int
	AuthTokenID         int64
	ClientName          string
	LogSource           string
	TotalRequests       int64
	SuccessRequests     int64
	ErrorRequests       int64
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	Cost                float64
	EffectiveCost       float64
}

// ListCumulativeUsage 读取本表全部维度行（混合模式恢复时从 MySQL 主库导出）。
func (s *SQLStore) ListCumulativeUsage(ctx context.Context) ([]CumulativeUsageRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT dimension_key, channel_id, model, status_code, auth_token_id,
		client_name, log_source, total_requests, success_requests, error_requests, input_tokens, output_tokens,
		cache_read_tokens, cache_creation_tokens, cost, effective_cost FROM cumulative_usage`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	result := make([]CumulativeUsageRecord, 0)
	for rows.Next() {
		var row CumulativeUsageRecord
		if err := rows.Scan(&row.DimensionKey, &row.ChannelID, &row.Model, &row.StatusCode, &row.AuthTokenID,
			&row.ClientName, &row.LogSource, &row.TotalRequests, &row.SuccessRequests, &row.ErrorRequests,
			&row.InputTokens, &row.OutputTokens, &row.CacheReadTokens, &row.CacheCreationTokens,
			&row.Cost, &row.EffectiveCost); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}
