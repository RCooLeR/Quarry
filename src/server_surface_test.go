//go:build !server

package main

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestRemoteServerDistributionSurfaceIsRemoved(t *testing.T) {
	for _, path := range []string{"Taskfile.yml", "build/Taskfile.yml"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		for _, forbidden := range []string{"build:server:", "run:server:", "Dockerfile.server", "WAILS_SERVER_HOST"} {
			if strings.Contains(text, forbidden) {
				t.Fatalf("%s still advertises unsafe server surface %q", path, forbidden)
			}
		}
	}
	if _, err := os.Stat("build/docker/Dockerfile.server"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("server container definition still exists: %v", err)
	}
	if !remoteServerSurfaceAllowed {
		t.Fatal("normal desktop build was unexpectedly disabled")
	}
}
