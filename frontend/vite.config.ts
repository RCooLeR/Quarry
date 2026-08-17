import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import wails from "@wailsio/runtime/plugins/vite";

// https://vitejs.dev/config/
export default defineConfig({
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
