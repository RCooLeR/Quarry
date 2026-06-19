package main

import (
	"embed"
	"log"

	"github.com/wailsapp/wails/v3/pkg/application"
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

func main() {
	app := application.New(application.Options{
		Name:        "Quarry",
		Description: "Streaming editor and toolkit for very large files",
		Icon:        appIcon,
		Services: []application.Service{
			application.NewService(NewFileService()),
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
	})

	// Black title bar (where the minimise/close buttons live) matching the app's
	// dark theme, forced dark regardless of the system light/dark setting.
	titleBar := colourRef(18, 22, 28)      // #12161c — same as the window background
	titleText := colourRef(205, 214, 228)  // #cdd6e4 — app foreground
	titleTextDim := colourRef(118, 128, 144)
	darkTitleBar := application.ThemeSettings{
		DarkModeActive:   &application.WindowTheme{TitleBarColour: titleBar, TitleTextColour: titleText, BorderColour: titleBar},
		DarkModeInactive: &application.WindowTheme{TitleBarColour: titleBar, TitleTextColour: titleTextDim, BorderColour: titleBar},
	}

	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:            "Quarry",
		Width:            1280,
		Height:           820,
		BackgroundColour: application.NewRGB(18, 22, 28),
		URL:              "/",
		Windows: application.WindowsWindow{
			Theme:       application.Dark,
			CustomTheme: darkTitleBar,
		},
	})

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
