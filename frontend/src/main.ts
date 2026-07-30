import { mount } from "svelte";
import App from "./App.svelte";
import { resolveViewToken } from "./lib/utils/viewToken.js";
import { SNAPSHOT } from "./lib/utils/snapshot.js";
import { viewMeta } from "./lib/stores/viewMeta.svelte.js";
import "./app.css";
import { initI18n } from "./lib/i18n/index.js";
import { installPerfFetchInstrumentation } from "./lib/stores/perf.svelte.js";

// Resolve a snapshot view's ?t= grant token before any component (and its
// data fetches) mounts, so the very first requests carry the token.
resolveViewToken();

// Snapshots default to dark; apply it before mount to avoid a light flash.
// An explicit stored preference still wins.
if (
  SNAPSHOT &&
  typeof localStorage !== "undefined" &&
  !localStorage.getItem("theme")
) {
  document.documentElement.classList.add("dark");
}

const target = document.getElementById("app");

if (!target) {
  throw new Error("Root element 'app' not found. Cannot mount application.");
}

installPerfFetchInstrumentation();
initI18n();

async function start(): Promise<void> {
  // Load the view's metadata (title + org color theme) before mount in
  // snapshot mode, so charts color projects by org on the first render.
  if (SNAPSHOT) await viewMeta.load();
  mount(App, { target: target! });
}

void start();
