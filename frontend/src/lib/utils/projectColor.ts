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

export function projectColor(name: string): string {
  if (!name) return FALLBACK;
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
