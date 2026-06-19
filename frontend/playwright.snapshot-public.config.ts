import { defineConfig } from "@playwright/test";

// E2E for a PUBLIC, stats-only view (views/public.json: allStats, no token).
// Verifies open access (no prompt) and the redaction UI (aggregates shown,
// sessions/prompts absent). Runs on :8798 to coexist with the gated config.
export default defineConfig({
  testDir: "e2e-snapshot-public",
  timeout: 30_000,
  retries: 0,
  use: {
    baseURL: "http://127.0.0.1:8798",
    headless: true,
  },
  projects: [{ name: "chromium", use: { browserName: "chromium" } }],
  webServer: {
    command: "bash ../scripts/e2e-snapshot-server.sh public 8798 none",
    url: "http://127.0.0.1:8798/api/v1/view/meta",
    reuseExistingServer: false,
    timeout: 150_000,
  },
});
