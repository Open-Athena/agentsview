import { defineConfig } from "@playwright/test";

// E2E for the snapshot/publishable-views stack (exporter -> D1 -> Worker ->
// SPA in VITE_SNAPSHOT mode), separate from the main app config. The
// webServer seeds a fixture, exports a gated view, loads it, and serves it on
// :8799; Playwright waits for view/meta=200 (i.e. after the data load).
export default defineConfig({
  testDir: "e2e-snapshot",
  timeout: 30_000,
  retries: 0,
  use: {
    baseURL: "http://127.0.0.1:8799",
    headless: true,
  },
  projects: [{ name: "chromium", use: { browserName: "chromium" } }],
  webServer: {
    command: "bash ../scripts/e2e-snapshot-server.sh",
    url: "http://127.0.0.1:8799/api/v1/view/meta",
    reuseExistingServer: false,
    timeout: 150_000,
  },
});
