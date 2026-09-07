package app

import (
	"bytes"
	"context"
	"encoding/csv"
	"mime/multipart"
	"net/http"
	"testing"

	"ccLoad/internal/model"
)

func TestChannelRequestResponsesTransportValidation(t *testing.T) {
	for _, tc := range []struct {
		mode, channel, transform string
		extra                    []string
		valid                    bool
	}{
		{"", "codex", "upstream", nil, true},
		{"websocket_preferred", "codex", "upstream", nil, true},
		{"websocket_only", "openai", "upstream", []string{"codex"}, true},
		{"websocket_only", "openai", "local", []string{"codex"}, false},
		{"websocket_only", "anthropic", "upstream", nil, false},
		{"invalid", "codex", "upstream", nil, false},
	} {
		t.Run(tc.mode+tc.channel+tc.transform, func(t *testing.T) {
			req := ChannelRequest{Name: "test", URL: "http://localhost", APIKey: "fixture", ChannelType: tc.channel, ProtocolTransformMode: tc.transform, ProtocolTransforms: tc.extra, ResponsesTransport: tc.mode, Models: []model.ModelEntry{{Model: "test"}}}
			err := req.Validate()
			if (err == nil) != tc.valid {
				t.Fatalf("validation=%v", err)
			}
		})
	}
}

func TestResponsesWSTransportCSVRoundTrip(t *testing.T) {
	srv := newInMemoryServer(t)
	cfg := addResponsesFixtureChannel(t, srv, "csv", "http://localhost:9", model.ResponsesTransportPreferWebsocket, 10)
	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/channels/export", nil))
	srv.HandleExportChannelsCSV(c)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	exported := bytes.Clone(w.Body.Bytes())
	cfg.ResponsesTransport = model.ResponsesTransportHTTP
	if _, err := srv.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "channels.csv")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(exported); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := newRequest(http.MethodPost, "/admin/channels/import", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	c, w = newTestContext(t, req)
	srv.HandleImportChannelsCSV(c)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	restored, err := srv.store.GetConfig(context.Background(), cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if restored.ResponsesTransport != model.ResponsesTransportPreferWebsocket {
		t.Fatalf("transport=%q", restored.ResponsesTransport)
	}
	rows, err := csv.NewReader(bytes.NewReader(exported)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	columns := buildCSVColumnIndex(rows[0])
	for _, mode := range []string{"invalid", model.ResponsesTransportWebsocketOnly} {
		rows[1][columns["responses_transport"]] = mode
		rows[1][columns["channel_type"]] = "anthropic"
		if _, _, skip := srv.parseChannelImportRow(rows[1], columns, 2, true, true, nil, nil); !skip {
			t.Fatalf("invalid CSV accepted: %s", mode)
		}
	}
}
