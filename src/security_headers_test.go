package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/wailsapp/wails/v3/pkg/application"
)

func TestSecurityHeadersPreserveHandlerAndRestrictPrivilegedWebview(t *testing.T) {
	called := false
	handler := securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("ok"))
	}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "https://wails.localhost/", nil))
	if !called || recorder.Code != http.StatusCreated || recorder.Body.String() != "ok" {
		t.Fatalf("downstream response = called %v, status %d, body %q", called, recorder.Code, recorder.Body.String())
	}
	csp := recorder.Header().Get("Content-Security-Policy")
	for _, directive := range []string{"default-src 'self'", "script-src 'self'", "object-src 'none'", "frame-src 'none'", "child-src 'none'", "frame-ancestors 'none'", "connect-src 'self'", "worker-src 'none'", "manifest-src 'none'"} {
		if !strings.Contains(csp, directive) {
			t.Errorf("CSP %q is missing %q", csp, directive)
		}
	}
	if strings.Contains(csp, "script-src 'self' 'unsafe-inline'") || strings.Contains(csp, "unsafe-eval") {
		t.Fatalf("CSP weakens script execution: %q", csp)
	}
	if recorder.Header().Get("X-Content-Type-Options") != "nosniff" || recorder.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatalf("security headers = %#v", recorder.Header())
	}
	if recorder.Header().Get("Cross-Origin-Opener-Policy") != "same-origin" || recorder.Header().Get("Cross-Origin-Resource-Policy") != "same-origin" {
		t.Fatalf("cross-origin isolation headers = %#v", recorder.Header())
	}
	permissions := recorder.Header().Get("Permissions-Policy")
	for _, denied := range []string{"camera=()", "clipboard-read=()", "display-capture=()", "geolocation=()", "microphone=()", "publickey-credentials-get=()", "serial=()", "usb=()"} {
		if !strings.Contains(permissions, denied) {
			t.Errorf("Permissions-Policy %q is missing %q", permissions, denied)
		}
	}
	if strings.Contains(permissions, "clipboard-write=()") {
		t.Fatalf("Permissions-Policy disables the explicit copy feature: %q", permissions)
	}
}

func TestPrivilegedWebviewNativePermissionsFailClosed(t *testing.T) {
	crossPlatform := privilegedWebviewPermissions()
	wantCrossPlatform := []application.PermissionType{
		application.PermissionMicrophone,
		application.PermissionCamera,
		application.PermissionGeolocation,
		application.PermissionNotifications,
		application.PermissionClipboardRead,
	}
	if len(crossPlatform) != len(wantCrossPlatform) {
		t.Fatalf("cross-platform permission policy has %d entries, want %d", len(crossPlatform), len(wantCrossPlatform))
	}
	for _, permission := range wantCrossPlatform {
		if got := crossPlatform[permission]; got != application.PermissionDeny {
			t.Errorf("cross-platform permission %d = %d, want deny", permission, got)
		}
	}

	windowsPolicy := privilegedWindowsWebviewPermissions()
	wantWindows := []application.CoreWebView2PermissionKind{
		application.CoreWebView2PermissionKindUnknownPermission,
		application.CoreWebView2PermissionKindMicrophone,
		application.CoreWebView2PermissionKindCamera,
		application.CoreWebView2PermissionKindGeolocation,
		application.CoreWebView2PermissionKindNotifications,
		application.CoreWebView2PermissionKindOtherSensors,
		application.CoreWebView2PermissionKindClipboardRead,
	}
	if len(windowsPolicy) != len(wantWindows) {
		t.Fatalf("Windows permission policy has %d entries, want %d", len(windowsPolicy), len(wantWindows))
	}
	for _, permission := range wantWindows {
		if got := windowsPolicy[permission]; got != application.CoreWebView2PermissionStateDeny {
			t.Errorf("Windows permission %d = %d, want deny", permission, got)
		}
	}
}

// This checks requested Wails options only. Backend support and actual keyboard
// zoom/context-menu behavior require packaged per-platform smoke coverage.
func TestPrivilegedWindowWiresRequestedNativeSecurityOptions(t *testing.T) {
	data, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	mainSource := string(data)
	for _, required := range []string{
		`DefaultContextMenuDisabled:\s*true`,
		`ZoomControlEnabled:\s*true`,
		`Permissions:\s*privilegedWebviewPermissions\(\)`,
		`Permissions:\s*privilegedWindowsWebviewPermissions\(\)`,
	} {
		if !regexp.MustCompile(required).MatchString(mainSource) {
			t.Errorf("privileged window is missing %q", required)
		}
	}
}
