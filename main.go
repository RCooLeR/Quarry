package main

import (
	"embed"
	"log"

	"github.com/quarry/quarry-wails3/internal/session"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// Wails embeds the built frontend (frontend/dist) into the binary.
//
//go:embed all:frontend/dist
var assets embed.FS

// appIcon is the Quarry window/taskbar icon, set in code so it never falls back
// to the stock Wails icon if the embedded exe resource can't be loaded.
//
//go:embed build/appicon.png
var appIcon []byte

// colourRef builds a Windows COLORREF (0x00BBGGRR) pointer for the title-bar theme.
func colourRef(r, g, b byte) *uint32 {
	c := uint32(r) | uint32(g)<<8 | uint32(b)<<16
	return &c
}

type droppedFilesPayload struct {
	Paths   []string `json:"paths"`
	Omitted int      `json:"omitted"`
}

func boundedDroppedFilesPayload(files []string) droppedFilesPayload {
	count := min(len(files), session.DefaultMaxOpenFiles)
	paths := make([]string, count)
	copy(paths, files[:count])
	return droppedFilesPayload{
		Paths:   paths,
		Omitted: len(files) - count,
	}
}

func main() {
	if !remoteServerSurfaceAllowed {
		log.Fatal("Quarry server mode is disabled: the desktop FileService must not be exposed over an unauthenticated network runtime")
	}
	lifecycle := newAppLifecycle()
	fileService := NewFileService()
	app := application.New(application.Options{
		Name:        "Quarry",
		Description: "Streaming editor and toolkit for very large files",
		Icon:        appIcon,
		Transport:   newBoundedWailsTransport(),
		Services: []application.Service{
			application.NewService(fileService),
			application.NewService(lifecycle),
		},
		ShouldQuit: lifecycle.interceptClose,
		Assets: application.AssetOptions{
			Handler:    application.AssetFileServerFS(assets),
			Middleware: securityHeaders,
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
	})

	// Black title bar (where the minimise/close buttons live) matching the app's
	// dark theme, forced dark regardless of the system light/dark setting.
	titleBar := colourRef(18, 22, 28)     // #12161c — same as the window background
	titleText := colourRef(205, 214, 228) // #cdd6e4 — app foreground
	titleTextDim := colourRef(118, 128, 144)
	darkTitleBar := application.ThemeSettings{
		DarkModeActive:   &application.WindowTheme{TitleBarColour: titleBar, TitleTextColour: titleText, BorderColour: titleBar},
		DarkModeInactive: &application.WindowTheme{TitleBarColour: titleBar, TitleTextColour: titleTextDim, BorderColour: titleBar},
	}

	win := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:              "Quarry",
		Width:              1280,
		Height:             820,
		MinWidth:           800,
		MinHeight:          600,
		BackgroundColour:   application.NewRGB(18, 22, 28),
		URL:                "/",
		ZoomControlEnabled: true,
		EnableFileDrop:     true, // drop files onto [data-file-drop-target] to open
		// The browser menu can expose navigation/save/print surfaces that Quarry
		// does not support inside its privileged bridge origin. Explicit app
		// controls retain bounded copy/inspection workflows.
		DefaultContextMenuDisabled: true,
		Permissions:                privilegedWebviewPermissions(),
		Windows: application.WindowsWindow{
			Theme:       application.Dark,
			CustomTheme: darkTitleBar,
			Permissions: privilegedWindowsWebviewPermissions(),
		},
	})
	lifecycle.bind(app.Quit, func() {
		win.EmitEvent(closeRequestedEvent)
	})
	lifecycle.bindShutdown(fileService.shutdown)
	win.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		if !lifecycle.interceptClose() {
			e.Cancel()
		}
	})

	// Bridge native file drops to the frontend: it opens each dropped path.
	win.OnWindowEvent(events.Common.WindowFilesDropped, func(e *application.WindowEvent) {
		files := e.Context().DroppedFiles()
		if len(files) > 0 {
			// Native drop sources are outside the renderer trust boundary. Never
			// serialize more paths than one complete application session can admit.
			win.EmitEvent("quarry:files-dropped", boundedDroppedFilesPayload(files))
		}
	})

	if err := app.Run(); err != nil {
		fileService.shutdown()
		log.Fatal(err)
	}
	fileService.shutdown()
}
