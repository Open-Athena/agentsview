// Palette uses the golden-angle hue rotation (137.5°) so
// PALETTE_SIZE evenly-spaced hues maximally separate any two
// consecutive entries — adjacent hash buckets land in
// perceptually distant hues. PALETTE_SIZE = 24 was chosen so a
// session with ~50 projects expects ~2 projects per color slot
// (down from ~4 with the prior 12-color CSS-var palette).
//
// The first 12 hues align with the prior named-accent palette
// so existing per-project colors don't shift much in practice;
// the next 12 fill in between for the new collision relief.
const GOLDEN_ANGLE = 137.508;
const PALETTE_SIZE = 24;

function generatePalette(): string[] {
  return Array.from({ length: PALETTE_SIZE }, (_, i) => {
    const hue = (i * GOLDEN_ANGLE) % 360;
    return `hsl(${hue.toFixed(1)}, 65%, 60%)`;
  });
}

export const PROJECT_PALETTE: readonly string[] = generatePalette();

const FALLBACK = "var(--text-muted)";

function djb2(s: string): number {
  let h = 5381;
  for (let i = 0; i < s.length; i++) {
    h = ((h << 5) + h + s.charCodeAt(i)) | 0;
  }
  return Math.abs(h);
}

// --- Org/tag color-coding -------------------------------------------------
//
// A published view can group projects by GitHub org / tag (see view/meta):
// each org gets a base hue and its projects render as deterministic *shades*
// of it, so the treemap/charts read as "OA orange, HCCS green, ...". Projects
// not in any org keep the golden-angle palette.

const COLOR_HUES: Record<string, number> = {
  red: 0, orange: 30, amber: 45, yellow: 55, lime: 90, green: 140,
  emerald: 155, teal: 170, cyan: 190, sky: 205, blue: 220, indigo: 250,
  violet: 270, purple: 285, fuchsia: 305, magenta: 320, pink: 335,
};

export interface OrgTheme {
  name: string;
  color: string; // named (e.g. "orange") or "hNNN" hue
  projects: string[];
}

// project name -> org base hue.
let projectHue = new Map<string, number>();

function colorToHue(color: string): number | null {
  if (!color) return null;
  const m = /^h(\d{1,3})$/i.exec(color.trim());
  if (m) return Number(m[1]) % 360;
  const h = COLOR_HUES[color.trim().toLowerCase()];
  return h === undefined ? null : h;
}

/** Install (or clear) the org theme; rebuilds the project->hue index. */
export function setOrgTheme(orgs: OrgTheme[] | undefined | null): void {
  const next = new Map<string, number>();
  for (const org of orgs ?? []) {
    const hue = colorToHue(org.color);
    if (hue === null) continue;
    for (const p of org.projects ?? []) next.set(p, hue);
  }
  projectHue = next;
}

// A stable shade within an org hue family: hash-derived hue jitter (+/-14 deg)
// and lightness so projects in the same org are distinguishable but related.
function orgShade(name: string, hue: number): string {
  const h = djb2(name);
  const jitter = ((h % 5) - 2) * 7;
  const light = 48 + (Math.floor(h / 5) % 5) * 6; // 48..72
  return `hsl(${((hue + jitter + 360) % 360).toFixed(0)}, 62%, ${light}%)`;
}

export function projectColor(name: string): string {
  if (!name) return FALLBACK;
  const hue = projectHue.get(name);
  if (hue !== undefined) return orgShade(name, hue);
  return PROJECT_PALETTE[djb2(name) % PROJECT_PALETTE.length]!;
}

// disambiguateColors guarantees that the top `n` items in a
// sorted list each get a unique color, even when their
// hash-derived colors collide. Walks in order; if an item's
// preferred color is already taken by an earlier item in the
// window, advance through the palette to the next unused color.
// Items past index `n` keep their hash-derived color.
//
// Use this for displays where adjacent same-colored tiles
// visually merge (treemap, stacked-area legend). Time-series
// where a project's color must stay stable across windows
// should keep calling projectColor() directly.
export function disambiguateColors(
  items: { id: string }[],
  n: number = PALETTE_SIZE,
): string[] {
  const used = new Set<string>();
  const out: string[] = [];
  const cap = Math.min(n, items.length);
  for (let i = 0; i < items.length; i++) {
    const preferred = projectColor(items[i]!.id);
    if (i >= cap) {
      out.push(preferred);
      continue;
    }
    if (!used.has(preferred)) {
      used.add(preferred);
      out.push(preferred);
      continue;
    }
    // Walk forward from preferred's slot for the first free color.
    const startIdx = PROJECT_PALETTE.indexOf(preferred);
    let picked = preferred;
    for (let off = 1; off < PROJECT_PALETTE.length; off++) {
      const c = PROJECT_PALETTE[(startIdx + off) % PROJECT_PALETTE.length]!;
      if (!used.has(c)) {
        picked = c;
        break;
      }
    }
    used.add(picked);
    out.push(picked);
  }
  return out;
}
