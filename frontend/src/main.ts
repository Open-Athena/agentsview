import { mount } from "svelte";
import App from "./App.svelte";
import { resolveViewToken } from "./lib/utils/viewToken.js";
import "./app.css";

// Resolve a snapshot view's ?t= grant token before any component (and its
// data fetches) mounts, so the very first requests carry the token.
resolveViewToken();

const target = document.getElementById("app");

if (!target) {
  throw new Error("Root element 'app' not found. Cannot mount application.");
}

mount(App, { target });
