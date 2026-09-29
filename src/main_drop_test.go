package main

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/quarry/quarry-wails3/internal/session"
)

func TestBoundedDroppedFilesPayload(t *testing.T) {
	t.Parallel()
	files := make([]string, session.DefaultMaxOpenFiles+17)
	for i := range files {
		files[i] = fmt.Sprintf("file-%d.sql", i)
	}

	payload := boundedDroppedFilesPayload(files)
	if got, want := len(payload.Paths), session.DefaultMaxOpenFiles; got != want {
		t.Fatalf("path count = %d, want %d", got, want)
	}
	if payload.Omitted != 17 {
		t.Fatalf("omitted = %d, want 17", payload.Omitted)
	}
	if payload.Paths[0] != files[0] || payload.Paths[len(payload.Paths)-1] != files[session.DefaultMaxOpenFiles-1] {
		t.Fatalf("bounded payload did not preserve path order: %#v", payload.Paths)
	}

	// Event publication must not retain the native event's backing array.
	files[0] = "mutated.sql"
	if payload.Paths[0] == files[0] {
		t.Fatal("payload aliases the native dropped-file slice")
	}
}

func TestBoundedDroppedFilesPayloadUsesStableEnvelope(t *testing.T) {
	t.Parallel()
	payload := boundedDroppedFilesPayload([]string{"one.sql"})
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(encoded), `{"paths":["one.sql"],"omitted":0}`; got != want {
		t.Fatalf("JSON envelope = %s, want %s", got, want)
	}
}
