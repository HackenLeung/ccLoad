package app

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"ccLoad/internal/model"
)

func TestGlobalDisabledModelsAdminAndCandidateFiltering(t *testing.T) {
	server := newInMemoryServer(t)

	postC, postW := newTestContext(t, newRequest(http.MethodPost, "/admin/channels/disabled-models", bytes.NewBufferString(`{"model":"grok-4.5"}`)))
	postC.Request.Header.Set("Content-Type", "application/json")
	server.HandleGlobalDisabledModels(postC)
	if postW.Code != http.StatusOK {
		t.Fatalf("disable model status=%d body=%s", postW.Code, postW.Body.String())
	}
	if !server.isGlobalModelDisabled("GROK-4.5") {
		t.Fatal("disabled model cache was not refreshed")
	}

	candidates := []*model.Config{
		{ID: 1, ModelEntries: []model.ModelEntry{{
			Model:           "grok-4.5",
			RedirectEnabled: true,
			ProtocolAliases: map[string][]string{"codex": {"gpt-5.5"}},
		}}},
		{ID: 2, ModelEntries: []model.ModelEntry{{Model: "gpt-5.5"}}},
	}
	filtered, blocked := server.filterGlobalDisabledModelCandidates(candidates, "gpt-5.5", "codex")
	if !blocked || len(filtered) != 1 || filtered[0].ID != 2 {
		t.Fatalf("unexpected filtered candidates: blocked=%v candidates=%#v", blocked, filtered)
	}

	deleteC, deleteW := newTestContext(t, newRequest(http.MethodDelete, "/admin/channels/disabled-models?model=grok-4.5", nil))
	server.HandleDeleteGlobalDisabledModel(deleteC)
	if deleteW.Code != http.StatusOK || server.isGlobalModelDisabled("grok-4.5") {
		t.Fatalf("restore model status=%d body=%s", deleteW.Code, deleteW.Body.String())
	}

	entries, err := server.store.(globalDisabledModelStore).ListGlobalDisabledModels(context.Background())
	if err != nil || len(entries) != 0 {
		t.Fatalf("disabled model persisted after restore: %#v err=%v", entries, err)
	}
}

func TestGlobalDisabledModelCandidateFilteringUsesResolvedAlias(t *testing.T) {
	server := &Server{globalDisabledModels: map[string]model.GlobalDisabledModel{
		"grok-4.5": {Model: "grok-4.5"},
	}}
	candidate := &model.Config{ID: 1, ModelEntries: []model.ModelEntry{{
		Model:           "grok-4.5",
		RedirectEnabled: true,
		ProtocolAliases: map[string][]string{"codex": {"gpt-5.5"}},
	}}}
	filtered, blocked := server.filterGlobalDisabledModelCandidates([]*model.Config{candidate}, "gpt-5.5", "codex")
	if !blocked || len(filtered) != 0 {
		t.Fatalf("resolved disabled upstream should be filtered: blocked=%v candidates=%#v", blocked, filtered)
	}
}

func TestGlobalDisabledModelCreatedAtPreservedOnUpsert(t *testing.T) {
	server := newInMemoryServer(t)

	firstC, firstW := newTestContext(t, newRequest(http.MethodPost, "/admin/channels/disabled-models", bytes.NewBufferString(`{"model":"grok-4.5","note":"first"}`)))
	firstC.Request.Header.Set("Content-Type", "application/json")
	server.HandleGlobalDisabledModels(firstC)
	if firstW.Code != http.StatusOK {
		t.Fatalf("first disable status=%d body=%s", firstW.Code, firstW.Body.String())
	}
	firstCreatedAt := server.listCachedGlobalDisabledModels()[0].CreatedAt
	if firstCreatedAt <= 0 {
		t.Fatal("expected created_at to be set")
	}

	secondC, secondW := newTestContext(t, newRequest(http.MethodPost, "/admin/channels/disabled-models", bytes.NewBufferString(`{"model":"GROK-4.5","note":"updated"}`)))
	secondC.Request.Header.Set("Content-Type", "application/json")
	server.HandleGlobalDisabledModels(secondC)
	if secondW.Code != http.StatusOK {
		t.Fatalf("second disable status=%d body=%s", secondW.Code, secondW.Body.String())
	}

	entries := server.listCachedGlobalDisabledModels()
	if len(entries) != 1 || entries[0].Note != "updated" || entries[0].CreatedAt != firstCreatedAt {
		t.Fatalf("created_at drifted after upsert: %#v", entries)
	}
	persisted, err := server.store.(globalDisabledModelStore).ListGlobalDisabledModels(context.Background())
	if err != nil || len(persisted) != 1 || persisted[0].CreatedAt != firstCreatedAt || persisted[0].Note != "updated" {
		t.Fatalf("persisted created_at drifted: %#v err=%v", persisted, err)
	}
}

