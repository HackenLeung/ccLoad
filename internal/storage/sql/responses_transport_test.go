package sql_test

import (
	"ccLoad/internal/model"
	"context"
	"testing"
)

func TestConfigResponsesTransportRoundTrip(t *testing.T) {
	store := newTestStore(t, "responses-transport.db")
	ctx := context.Background()
	cfg, err := store.CreateConfig(ctx, &model.Config{Name: "ws", URL: "http://localhost", ChannelType: "codex", Enabled: true, ResponsesTransport: model.ResponsesTransportPreferWebsocket, ModelEntries: []model.ModelEntry{{Model: "test"}}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ResponsesTransport != model.ResponsesTransportPreferWebsocket {
		t.Fatalf("mode=%s", cfg.ResponsesTransport)
	}
	cfg.ResponsesTransport = model.ResponsesTransportWebsocketOnly
	if _, err = store.UpdateConfig(ctx, cfg.ID, cfg); err != nil {
		t.Fatal(err)
	}
	channels, err := store.GetEnabledChannelsByModelAndProtocol(ctx, "test", "codex")
	if err != nil {
		t.Fatal(err)
	}
	if len(channels) != 1 || channels[0].ResponsesTransport != model.ResponsesTransportWebsocketOnly {
		t.Fatalf("channels=%+v", channels)
	}
	if cfg.Clone().ResponsesTransport != cfg.ResponsesTransport {
		t.Fatal("clone lost mode")
	}
}
