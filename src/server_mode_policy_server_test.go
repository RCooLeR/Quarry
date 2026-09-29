//go:build server

package main

import "testing"

func TestServerBuildCannotStartRemoteRuntime(t *testing.T) {
	if remoteServerSurfaceAllowed {
		t.Fatal("server-tag build can expose the desktop FileService")
	}
}
