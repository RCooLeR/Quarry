import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import wails from "@wailsio/runtime/plugins/vite";

// https://vitejs.dev/config/
export default defineConfig({
  // Vite 8.3 resolves this explicitly, keeping transpilation aligned with tsc
  // when the desktop build is invoked from a different working directory.
  tsconfig: "./tsconfig.json",
  build: {
    rolldownOptions: {
      output: {
        // React changes on a different cadence from Quarry's application code.
        // Rolldown's native chunk groups keep that stable runtime cacheable and
        // prevent framework upgrades from inflating the application entry.
        codeSplitting: {
          groups: [
            {
              name: "react-vendor",
              test: /[\\/]node_modules[\\/](react|react-dom|scheduler)[\\/]/,
            },
          ],
        },
      },
    },
  },
  server: {
    host: "127.0.0.1",
    port: Number(process.env.WAILS_VITE_PORT) || 9245,
    strictPort: true,
    // React Fast Refresh injects an inline module preamble. Keep it disabled so
    // development exercises the same no-inline-script CSP as production.
    hmr: false,
  },
  plugins: [react(), wails("./bindings")],
});
