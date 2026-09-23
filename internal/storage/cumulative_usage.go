package storage

import (
	"context"
	"database/sql"
	"fmt"

	"ccLoad/internal/model"
)

type cumulativeUsageRow struct {
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

// cumUsageBackfillSelect 回填用的「日志 → 累计维度」聚合。
//
// 单条 GROUP BY 全表聚合，而不是按渠道逐个查询（避免 N+1 拖垮迁移超时）。
// 口径必须与 sql.upsertCumulativeUsage 严格一致，否则同一维度会被两条路径算出不同值：
//   - client_name 截断到 64 字节（历史 SQLite 该列为 TEXT，可能超长）
//   - cost_multiplier 负数归一为 1，0（免费渠道）保持不变
//
// SELECT 与 GROUP BY 的表达式必须逐字一致（MySQL ONLY_FULL_GROUP_BY 要求），因此两处重复书写。
const cumUsageBackfillSelect = `SELECT
	channel_id, COALESCE(model, ''), status_code, COALESCE(auth_token_id, 0),
	SUBSTR(COALESCE(client_name, ''), 1, 64),
	CASE WHEN log_source IN ('proxy', 'scheduled_check', 'manual_test', 'manual_chat', 'vision_assist') THEN log_source ELSE 'proxy' END,
	SUM(CASE WHEN status_code NOT IN (413, 499) THEN 1 ELSE 0 END),
	SUM(CASE WHEN status_code >= 200 AND status_code < 300 THEN 1 ELSE 0 END),
	SUM(CASE WHEN (status_code < 200 OR status_code >= 300) AND status_code NOT IN (413, 499) THEN 1 ELSE 0 END),
	SUM(COALESCE(input_tokens, 0)), SUM(COALESCE(output_tokens, 0)),
	SUM(COALESCE(cache_read_input_tokens, 0)), SUM(COALESCE(cache_creation_input_tokens, 0)),
	SUM(COALESCE(cost, 0)), SUM(COALESCE(cost, 0) * CASE WHEN COALESCE(cost_multiplier, 1) < 0 THEN 1 ELSE COALESCE(cost_multiplier, 1) END)
	FROM logs WHERE channel_id > 0
	GROUP BY channel_id, model, status_code, auth_token_id,
		SUBSTR(COALESCE(client_name, ''), 1, 64),
		CASE WHEN log_source IN ('proxy', 'scheduled_check', 'manual_test', 'manual_chat', 'vision_assist') THEN log_source ELSE 'proxy' END`

// cumUsageInsertSQL 回填落库语句。维度已由 GROUP BY 去重，迁移标记保证只跑一次，故用纯 INSERT。
const cumUsageInsertSQL = `INSERT INTO cumulative_usage (
	dimension_key, channel_id, model, status_code, auth_token_id, client_name, log_source,
	total_requests, success_requests, error_requests, input_tokens, output_tokens,
	cache_read_tokens, cache_creation_tokens, cost, effective_cost
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// backfillCumulativeUsage 从 logs 回填永久累计统计（迁移版本 v4）。
//
// 幂等：迁移标记已存在则直接跳过。聚合与写入在同一事务内完成，
// 失败则连同迁移标记一起回滚，下次启动重试。
func backfillCumulativeUsage(ctx context.Context, db *sql.DB, dialect Dialect) error {
	applied, err := isMigrationApplied(ctx, db, cumulativeUsageMigrationVersion)
	if err != nil || applied {
		return err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if err := backfillCumulativeUsageTx(ctx, tx); err != nil {
		return err
	}
	if err := recordMigrationTx(ctx, tx, cumulativeUsageMigrationVersion, dialect); err != nil {
		return err
	}
	return tx.Commit()
}

// backfillCumulativeUsageTx 在事务内聚合 logs 并逐维度写入 cumulative_usage。
func backfillCumulativeUsageTx(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, cumUsageBackfillSelect)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	stmt, err := tx.PrepareContext(ctx, cumUsageInsertSQL)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()

	for rows.Next() {
		var row cumulativeUsageRow
		if err := rows.Scan(
			&row.channelID, &row.model, &row.statusCode, &row.authTokenID, &row.clientName, &row.logSource,
			&row.totalRequests, &row.successRequests, &row.errorRequests, &row.inputTokens, &row.outputTokens,
			&row.cacheReadTokens, &row.cacheCreateTokens, &row.cost, &row.effectiveCost,
		); err != nil {
			return err
		}
		// SQL 侧已按 64 截断，此处再走一遍 Go 侧截断，确保与运行时写入使用同一维度输入。
		clientName := model.TruncateColumnValue(row.clientName, model.ClientNameMaxLen)
		key := model.CumulativeUsageDimensionKey(row.channelID, row.model, row.statusCode, row.authTokenID, clientName, row.logSource)
		if _, err := stmt.ExecContext(ctx,
			key, row.channelID, row.model, row.statusCode, row.authTokenID, clientName, row.logSource,
			row.totalRequests, row.successRequests, row.errorRequests, row.inputTokens, row.outputTokens,
			row.cacheReadTokens, row.cacheCreateTokens, row.cost, row.effectiveCost,
		); err != nil {
			return fmt.Errorf("insert cumulative usage for channel %d: %w", row.channelID, err)
		}
	}
	return rows.Err()
}
