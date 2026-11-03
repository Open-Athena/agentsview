import { test, expect } from "@playwright/test";

// Drives the gated snapshot view served by scripts/e2e-snapshot-server.sh.
// A deterministic grant token ("e2e-token") is minted into the view's D1.
const TOKEN = "e2e-token";
const TITLE = "E2E Gated View";

test("tokenless access is denied with a private-view prompt", async ({ page }) => {
  await page.goto("/");
  await expect(page.locator(".auth-overlay")).toBeVisible();
  await expect(page.getByText("This view is private")).toBeVisible();
  await expect(page.getByPlaceholder("Paste access token")).toBeVisible();
  // The app shell (header nav) must NOT render behind the overlay.
  await expect(page.locator(".header-title")).toHaveCount(0);
});

test("a ?t= grant link loads the view and scrubs the token from the URL", async ({ page }) => {
  await page.goto(`/?t=${TOKEN}`);
  await expect(page.locator(".auth-overlay")).toHaveCount(0);
  await expect(page.locator(".header-title")).toContainText(TITLE);
  // Token removed from the address bar / history.
  expect(new URL(page.url()).searchParams.get("t")).toBeNull();
});

test("the grant persists in localStorage across a reload", async ({ page }) => {
  await page.goto(`/?t=${TOKEN}`);
  await expect(page.locator(".header-title")).toContainText(TITLE);

  await page.reload(); // no ?t= this time — must re-auth from localStorage
  await expect(page.locator(".auth-overlay")).toHaveCount(0);
  await expect(page.locator(".header-title")).toContainText(TITLE);
});

test("an invalid token is rejected and falls back to the prompt", async ({ page }) => {
  await page.goto("/?t=not-a-real-token");
  await expect(page.locator(".auth-overlay")).toBeVisible();
  await expect(page.locator(".header-title")).toHaveCount(0);
});
