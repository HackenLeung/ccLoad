package sql_test

import (
	"context"
	"testing"

	"ccLoad/internal/model"
)

func TestGlobalDisabledModelsCRUD(t *testing.T) {
	t.Parallel()
	store := newTestStore(t, "global-disabled-models.db")
	disabledStore := store.(interface {
		ListGlobalDisabledModels(context.Context) ([]model.GlobalDisabledModel, error)
		UpsertGlobalDisabledModel(context.Context, model.GlobalDisabledModel) error
		DeleteGlobalDisabledModel(context.Context, string) error
	})
	ctx := context.Background()

	if err := disabledStore.UpsertGlobalDisabledModel(ctx, model.GlobalDisabledModel{Model: " GPT-5.5 ", Note: "test"}); err != nil {
		t.Fatalf("upsert disabled model: %v", err)
	}
	if err := disabledStore.UpsertGlobalDisabledModel(ctx, model.GlobalDisabledModel{Model: "gpt-5.5", Note: "updated"}); err != nil {
		t.Fatalf("case-insensitive upsert disabled model: %v", err)
	}
	entries, err := disabledStore.ListGlobalDisabledModels(ctx)
	if err != nil {
		t.Fatalf("list disabled models: %v", err)
	}
	if len(entries) != 1 || entries[0].Model != "gpt-5.5" || entries[0].Note != "updated" {
		t.Fatalf("unexpected disabled models: %#v", entries)
	}
	if err := disabledStore.DeleteGlobalDisabledModel(ctx, "GPT-5.5"); err != nil {
		t.Fatalf("delete disabled model: %v", err)
	}
	entries, err = disabledStore.ListGlobalDisabledModels(ctx)
	if err != nil || len(entries) != 0 {
		t.Fatalf("disabled model not deleted: %#v err=%v", entries, err)
	}
}
