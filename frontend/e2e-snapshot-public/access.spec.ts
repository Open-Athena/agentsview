import { test, expect } from "@playwright/test";

// Drives a PUBLIC, stats-only view (views/public.json) served by
// scripts/e2e-snapshot-server.sh with no token.

test("a public view loads with no access prompt", async ({ page }) => {
  await page.goto("/");
  await expect(page.locator(".auth-overlay")).toHaveCount(0);
  await expect(page.locator(".header-title")).toContainText("public stats");
});

test("stats-only view shows aggregate cost but redacts sessions + prompts", async ({
  page,
}) => {
  await page.goto("/usage");
  // No login prompt (public).
  await expect(page.locator(".auth-overlay")).toHaveCount(0);
  // Aggregate cost is present...
  await expect(page.getByText("Total Cost")).toBeVisible();
  // ...but no session-level content leaks.
  await expect(page.getByText("No sessions in range")).toBeVisible();
  await expect(page.getByText("No prompts in range")).toBeVisible();
});

test("stats-only view has an empty session sidebar", async ({ page }) => {
  await page.goto("/");
  await expect(page.locator(".auth-overlay")).toHaveCount(0);
  // The sidebar session count reflects zero raw sessions in the data.
  await expect(page.locator(".session-count")).toContainText("0 sessions");
});
