package snapshot

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSessionMetaJSONRoundTripIncludesOptionalFields(t *testing.T) {
	want := SessionMeta{
		ID:               "session-1",
		CreatedAt:        "2026-01-02T03:04:05Z",
		ProjectPath:      "/project",
		ProjectHash:      "hash",
		LightcodeVersion: "0.3.0",
		State:            StateArchived,
		ArchivedAt:       123,
		LastActivity:     456,
		Provider:         "openrouter",
		Model:            "provider/model",
		ParentSessionID:  "parent-1",
	}

	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got SessionMeta
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("round-trip meta = %+v, want %+v", got, want)
	}
}

func TestSessionMetaOptionalFieldsOmittedWhenEmpty(t *testing.T) {
	data, err := json.Marshal(SessionMeta{ID: "session-1"})
	if err != nil {
		t.Fatal(err)
	}
	jsonText := string(data)
	for _, field := range []string{"provider", "model", "parent_session_id"} {
		if strings.Contains(jsonText, field) {
			t.Fatalf("json %s unexpectedly contains empty optional field %q", jsonText, field)
		}
	}
}

func TestSessionMetaBackwardsCompatibleWithOldMetaFiles(t *testing.T) {
	oldJSON := []byte(`{
		"id": "old-session",
		"created_at": "2026-01-02T03:04:05Z",
		"project_path": "/project",
		"project_hash": "hash",
		"state": "active",
		"archived_at": 0,
		"last_activity": 456
	}`)

	var got SessionMeta
	if err := json.Unmarshal(oldJSON, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != "old-session" || got.ProjectPath != "/project" || got.State != StateActive {
		t.Fatalf("old meta decode = %+v, want old fields preserved", got)
	}
	if got.LightcodeVersion != "" || got.Provider != "" || got.Model != "" || got.ParentSessionID != "" {
		t.Fatalf("new fields = version:%q provider:%q model:%q parent:%q, want zero values", got.LightcodeVersion, got.Provider, got.Model, got.ParentSessionID)
	}
}

func TestSnapshotMetaJSONRoundTrip(t *testing.T) {
	want := SnapshotMeta{OriginalPath: "/project/file.go", Existed: true}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got SnapshotMeta
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("round-trip snapshot meta = %+v, want %+v", got, want)
	}
}
