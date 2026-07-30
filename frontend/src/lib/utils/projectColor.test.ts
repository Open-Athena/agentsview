import { describe, it, expect } from "vite-plus/test";
import {
  projectColor,
  disambiguateColors,
  PROJECT_PALETTE,
} from "./projectColor.js";

describe("projectColor", () => {
  it("returns a palette color for any non-empty string", () => {
    expect(PROJECT_PALETTE).toContain(projectColor("agentsview"));
  });

  it("is deterministic", () => {
    expect(projectColor("agentsview")).toBe(projectColor("agentsview"));
  });

  it("maps empty input to the muted fallback", () => {
    expect(projectColor("")).toBe("var(--text-muted)");
  });

  it("spreads different names across the palette", () => {
    const names = [
      "agentsview", "quokka", "arrow-rs", "side-quests",
      "infrastructure", "blog", "experiments", "docs",
      "dotfiles", "playground", "sandbox", "notes",
    ];
    const seen = new Set(names.map(projectColor));
    expect(seen.size).toBeGreaterThanOrEqual(6);
  });

  it("palette has at least 24 distinct colors", () => {
    expect(new Set(PROJECT_PALETTE).size).toBe(PROJECT_PALETTE.length);
    expect(PROJECT_PALETTE.length).toBeGreaterThanOrEqual(24);
  });
});

describe("disambiguateColors", () => {
  it("returns one color per input item", () => {
    const items = [{ id: "a" }, { id: "b" }, { id: "c" }];
    expect(disambiguateColors(items)).toHaveLength(items.length);
  });

  it("gives every top-N item a unique color", () => {
    // Pick names that previously collided; whatever the hash does
    // the top window must come out fully distinct.
    const items = Array.from({ length: 12 }, (_, i) => ({
      id: `proj-${i}`,
    }));
    const colors = disambiguateColors(items, 12);
    expect(new Set(colors).size).toBe(items.length);
  });

  it("leaves items past the window alone", () => {
    const items = Array.from({ length: 30 }, (_, i) => ({
      id: `p${i}`,
    }));
    const colors = disambiguateColors(items, 8);
    // First 8 must be unique
    expect(new Set(colors.slice(0, 8)).size).toBe(8);
    // Past the window, color is just projectColor() of the id
    for (let i = 8; i < items.length; i++) {
      expect(colors[i]).toBe(projectColor(items[i]!.id));
    }
  });
});
