package main

import (
	"net/http"

	"github.com/wailsapp/wails/v3/pkg/application"
)

const contentSecurityPolicy = "default-src 'self'; base-uri 'none'; object-src 'none'; frame-src 'none'; child-src 'none'; frame-ancestors 'none'; form-action 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self' data:; connect-src 'self'; worker-src 'none'; media-src 'none'; manifest-src 'none'"

// Quarry only needs clipboard-write for explicit user copy actions. All other
// browser capabilities below are denied at the embedded-document boundary so
// a compromised renderer cannot turn on unrelated device or capture surfaces.
const permissionsPolicy = "accelerometer=(), autoplay=(), bluetooth=(), camera=(), clipboard-read=(), display-capture=(), encrypted-media=(), fullscreen=(), gamepad=(), geolocation=(), gyroscope=(), hid=(), idle-detection=(), magnetometer=(), microphone=(), midi=(), payment=(), picture-in-picture=(), publickey-credentials-create=(), publickey-credentials-get=(), screen-wake-lock=(), serial=(), speaker-selection=(), storage-access=(), usb=(), web-share=(), window-management=(), xr-spatial-tracking=()"

// privilegedWebviewPermissions denies every cross-platform capability exposed
// by Wails. Quarry needs clipboard write for explicit copy actions, but it does
// not need clipboard read, capture, location, or notification permission. The
// native policy is intentionally independent of the HTTP Permissions-Policy:
// an omitted Wails policy is permissive on some supported WebView backends.
func privilegedWebviewPermissions() map[application.PermissionType]application.Permission {
	return map[application.PermissionType]application.Permission{
		application.PermissionMicrophone:    application.PermissionDeny,
		application.PermissionCamera:        application.PermissionDeny,
		application.PermissionGeolocation:   application.PermissionDeny,
		application.PermissionNotifications: application.PermissionDeny,
		application.PermissionClipboardRead: application.PermissionDeny,
	}
}

// privilegedWindowsWebviewPermissions also covers WebView2-specific kinds
// that have no cross-platform Wails equivalent, notably other sensors and the
// unknown/future bucket.
func privilegedWindowsWebviewPermissions() map[application.CoreWebView2PermissionKind]application.CoreWebView2PermissionState {
	return map[application.CoreWebView2PermissionKind]application.CoreWebView2PermissionState{
		application.CoreWebView2PermissionKindUnknownPermission: application.CoreWebView2PermissionStateDeny,
		application.CoreWebView2PermissionKindMicrophone:        application.CoreWebView2PermissionStateDeny,
		application.CoreWebView2PermissionKindCamera:            application.CoreWebView2PermissionStateDeny,
		application.CoreWebView2PermissionKindGeolocation:       application.CoreWebView2PermissionStateDeny,
		application.CoreWebView2PermissionKindNotifications:     application.CoreWebView2PermissionStateDeny,
		application.CoreWebView2PermissionKindOtherSensors:      application.CoreWebView2PermissionStateDeny,
		application.CoreWebView2PermissionKindClipboardRead:     application.CoreWebView2PermissionStateDeny,
	}
}

// securityHeaders hardens every response served inside the privileged desktop
// WebView. Quarry intentionally has no remote content surface; bridge calls use
// same-origin POST requests to /wails/runtime.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers := w.Header()
		headers.Set("Content-Security-Policy", contentSecurityPolicy)
		headers.Set("Permissions-Policy", permissionsPolicy)
		headers.Set("Referrer-Policy", "no-referrer")
		headers.Set("Cross-Origin-Opener-Policy", "same-origin")
		headers.Set("Cross-Origin-Resource-Policy", "same-origin")
		headers.Set("X-Content-Type-Options", "nosniff")
		headers.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}
