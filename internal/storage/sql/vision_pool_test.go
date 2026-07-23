package sql_test

import (
	"context"
	"path/filepath"
	"testing"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
)

func TestReplaceVisionPoolOnlyChangesVisionFields(t *testing.T) {
	store, err := storage.CreateSQLiteStore(filepath.Join(t.TempDir(), "vision-pool.db"))
	if err != nil {
		t.Fatalf("create sqlite store: %v", err)
	}
	defer func() { _ = store.Close() }()

	ctx := context.Background()
	cfg, err := store.CreateConfig(ctx, &model.Config{
		Name:         "preserve-me",
		URL:          "https://example.test",
		ChannelType:  "openai",
		Priority:     42,
		Enabled:      true,
		ModelEntries: []model.ModelEntry{{Model: "vision-vl", VisionAssistEnabled: true}},
	})
	if err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}

	if err := store.ReplaceVisionPool(ctx, []model.VisionPoolUpdate{{ChannelID: cfg.ID, Model: "vision-vl", Priority: 7}}); err != nil {
		t.Fatalf("ReplaceVisionPool: %v", err)
	}
	got, err := store.GetConfig(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if got.Name != "preserve-me" || got.URL != "https://example.test" || got.Priority != 42 || !got.Enabled {
		t.Fatalf("non-vision fields changed: %+v", got)
	}
	if !got.ModelEntries[0].VisionAssistEnabled || !got.ModelEntries[0].VisionPoolEnabled || got.ModelEntries[0].VisionPriority != 7 {
		t.Fatalf("vision fields not updated: %+v", got.ModelEntries[0])
	}

	if err := store.ReplaceVisionPool(ctx, []model.VisionPoolUpdate{{ChannelID: cfg.ID, Model: "missing", Priority: 9}}); err == nil {
		t.Fatal("expected missing model update to fail")
	}
	rolledBack, err := store.GetConfig(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("GetConfig after rollback: %v", err)
	}
	if !rolledBack.ModelEntries[0].VisionPoolEnabled || rolledBack.ModelEntries[0].VisionPriority != 7 {
		t.Fatalf("failed update was not rolled back: %+v", rolledBack.ModelEntries[0])
	}
}
