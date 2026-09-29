//go:build server

package main

// Quarry's FileService is a desktop-local capability surface. It must not be
// exposed by Wails' unauthenticated HTTP server runtime.
const remoteServerSurfaceAllowed = false
