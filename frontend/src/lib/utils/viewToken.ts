import { setAuthToken } from "../api/runtime.js";
import { SNAPSHOT } from "./snapshot.js";

// Snapshot views pass a grant token as ?t=<token>. Move it into the
// auth-token store (sent as Authorization: Bearer) and scrub it from the URL
// before the app mounts, so a shared link authenticates every request and the
// token is not left in the address bar / history.
export function resolveViewToken(): void {
  if (!SNAPSHOT || typeof window === "undefined") return;
  const url = new URL(window.location.href);
  const t = url.searchParams.get("t");
  if (!t) return;
  setAuthToken(t);
  url.searchParams.delete("t");
  window.history.replaceState(
    null,
    "",
    url.pathname + url.search + url.hash,
  );
}
