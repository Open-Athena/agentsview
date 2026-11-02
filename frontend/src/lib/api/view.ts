import { orvalFetch } from "./runtime.js";

// Published-snapshot self-description. Served only by the snapshot Worker at
// /api/v1/view/meta, so it is not part of the generated OpenAPI client (the
// live Go server has no such route).
export interface ViewMeta {
  title?: string;
  statsOnly: boolean;
  gated?: boolean;
  blurbs?: Record<string, string>;
  orgs?: { name: string; color: string; projects: string[] }[];
  generatedAt: string;
}

export async function getViewMeta(): Promise<ViewMeta> {
  return orvalFetch<ViewMeta>("/api/v1/view/meta", { method: "GET" });
}
