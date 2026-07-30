import { getViewMeta, type ViewMeta } from "../api/view.js";
import { SNAPSHOT } from "../utils/snapshot.js";
import { setOrgTheme } from "../utils/projectColor.js";

// Holds a published snapshot's self-description (served at /api/v1/view/meta).
// Only loaded in snapshot mode; null otherwise.
class ViewMetaStore {
  meta: ViewMeta | null = $state(null);

  async load(): Promise<void> {
    if (!SNAPSHOT) return;
    try {
      this.meta = await getViewMeta();
      if (this.meta?.title) document.title = this.meta.title;
      setOrgTheme(this.meta?.orgs);
    } catch {
      // Older snapshots / non-snapshot servers have no view/meta; ignore.
    }
  }

  get title(): string | null {
    return this.meta?.title ?? null;
  }

  get statsOnly(): boolean {
    return this.meta?.statsOnly ?? false;
  }

  blurb(project: string): string | null {
    return this.meta?.blurbs?.[project] ?? null;
  }
}

export const viewMeta = new ViewMetaStore();
