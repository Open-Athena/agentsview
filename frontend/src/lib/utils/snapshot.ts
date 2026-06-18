// True when the app is built as a published, read-only snapshot
// (VITE_SNAPSHOT=1 at build time). Snapshots are served by the
// Cloudflare Worker in snapshot-site/ and have no live backend, so
// sync/SSE/mutation affordances and not-yet-supported pages are hidden.
export const SNAPSHOT: boolean = import.meta.env.VITE_SNAPSHOT === "1";
