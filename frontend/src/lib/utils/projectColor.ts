import { hashColor } from "@kenn-io/kit-ui/utils/color-hash";

export const PROJECT_PALETTE: readonly string[] = [
  "var(--accent-blue)",
  "var(--accent-purple)",
  "var(--accent-amber)",
  "var(--accent-teal)",
  "var(--accent-rose)",
  "var(--accent-green)",
  "var(--accent-indigo)",
  "var(--accent-orange)",
  "var(--accent-sky)",
  "var(--accent-pink)",
  "var(--accent-coral)",
  "var(--accent-lime)",
] as const;

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
// not in any org keep the hash-based palette.

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

// projectColor delegates to kit-ui's hashColor over the app palette, except
// when an org theme is installed (published views): org projects render as a
// deterministic shade of their org hue instead.
export function projectColor(name: string): string {
  if (!name) return "var(--text-muted)";
  const hue = projectHue.get(name);
  if (hue !== undefined) return orgShade(name, hue);
  return hashColor(name, PROJECT_PALETTE);
}

export function seriesColorMap(
  ids: readonly string[],
): ReadonlyMap<string, string> {
  const colors = new Map<string, string>();
  const occupied = new Set<number>();
  const uniqueIds = [...new Set(ids)]
    .filter((id) => id !== "" && id !== "__other__")
    .sort();

  for (const id of uniqueIds) {
    const preferred = PROJECT_PALETTE.indexOf(projectColor(id));
    if (preferred < 0) continue;

    let slot = preferred;
    for (let offset = 0; offset < PROJECT_PALETTE.length; offset++) {
      const candidate = (preferred + offset) % PROJECT_PALETTE.length;
      if (!occupied.has(candidate)) {
        slot = candidate;
        occupied.add(candidate);
        break;
      }
    }
    colors.set(id, PROJECT_PALETTE[slot]!);
  }

  for (const id of ids) {
    if (id === "" || id === "__other__") colors.set(id, "var(--text-muted)");
  }

  return colors;
}
