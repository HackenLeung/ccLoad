package sql

import (
	"context"
	"fmt"
	"strings"
	"time"

	"ccLoad/internal/model"
)

func disabledModelKey(modelName string) string {
	return strings.ToLower(strings.TrimSpace(modelName))
}

func (s *SQLStore) ListGlobalDisabledModels(ctx context.Context) ([]model.GlobalDisabledModel, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT model, note, created_at FROM global_disabled_models ORDER BY created_at ASC, model ASC`)
	if err != nil {
		return nil, fmt.Errorf("query global_disabled_models: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make([]model.GlobalDisabledModel, 0)
	for rows.Next() {
		var entry model.GlobalDisabledModel
		if err := rows.Scan(&entry.Model, &entry.Note, &entry.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan global_disabled_models: %w", err)
		}
		result = append(result, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate global_disabled_models: %w", err)
	}
	return result, nil
}

func (s *SQLStore) UpsertGlobalDisabledModel(ctx context.Context, entry model.GlobalDisabledModel) error {
	entry.Model = strings.TrimSpace(entry.Model)
	entry.Note = strings.TrimSpace(entry.Note)
	key := disabledModelKey(entry.Model)
	if key == "" {
		return fmt.Errorf("model cannot be empty")
	}
	createdAt := entry.CreatedAt
	if createdAt == 0 {
		createdAt = timeToUnix(time.Now())
	}

	var query string
	if s.IsSQLite() {
		query = `INSERT INTO global_disabled_models(model_key, model, note, created_at) VALUES(?, ?, ?, ?)
			ON CONFLICT(model_key) DO UPDATE SET model = excluded.model, note = excluded.note`
	} else {
		query = `INSERT INTO global_disabled_models(model_key, model, note, created_at) VALUES(?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE model = VALUES(model), note = VALUES(note)`
	}
	if _, err := s.db.ExecContext(ctx, query, key, entry.Model, entry.Note, createdAt); err != nil {
		return fmt.Errorf("upsert global_disabled_models: %w", err)
	}
	return nil
}

func (s *SQLStore) DeleteGlobalDisabledModel(ctx context.Context, modelName string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM global_disabled_models WHERE model_key = ?`, disabledModelKey(modelName)); err != nil {
		return fmt.Errorf("delete global_disabled_models: %w", err)
	}
	return nil
}
