package main

import (
	"os"
	"strings"
	"testing"
)

func TestPublicProductMetadataUsesIntentionalQuarryValues(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path     string
		required []string
		forbid   []string
	}{
		{
			path: "build/config.yml",
			required: []string{
				`companyName: "RCooLeR"`,
				`productName: "Quarry"`,
				`productIdentifier: "com.rcooler.quarry"`,
				"fileAssociations: []",
			},
			forbid: []string{"My Other Data", "Wails Application File"},
		},
		{
			path:     "build/linux/nfpm/nfpm.yaml",
			required: []string{`name: "quarry"`, `vendor: "RCooLeR"`, `homepage: "https://github.com/RCooLeR/Quarry"`},
			forbid:   []string{"wails.io", "foobar", "not-foo"},
		},
		{
			path:     "build/android/settings.gradle",
			required: []string{`rootProject.name = "Quarry"`},
			forbid:   []string{"WailsApp"},
		},
		{
			path:     "build/android/app/src/main/res/values/strings.xml",
			required: []string{`<string name="app_name">Quarry</string>`},
			forbid:   []string{"Wails App"},
		},
		{
			path:     "build/android/app/build.gradle",
			required: []string{`applicationId "com.rcooler.quarry"`, `versionName "0.0.0-dev"`},
		},
		{
			path:     "build/ios/Info.plist",
			required: []string{"<string>Quarry</string>", "<string>com.rcooler.quarry</string>"},
		},
		{
			path:     "build/windows/info.json",
			required: []string{`"CompanyName": "RCooLeR"`, `"ProductName": "Quarry"`},
		},
		{
			path:     "frontend/index.html",
			required: []string{"<title>Quarry</title>"},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			data, err := os.ReadFile(tc.path)
			if err != nil {
				t.Fatalf("read product metadata: %v", err)
			}
			text := string(data)
			for _, value := range tc.required {
				if !strings.Contains(text, value) {
					t.Errorf("missing intentional metadata value %q", value)
				}
			}
			for _, value := range tc.forbid {
				if strings.Contains(text, value) {
					t.Errorf("contains template placeholder %q", value)
				}
			}
		})
	}
}
