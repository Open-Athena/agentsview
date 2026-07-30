// Node 26 ships an experimental native `localStorage` global that shadows
// jsdom's window.localStorage and is `undefined` unless the process is started
// with `--localstorage-file`. That leaves `localStorage` undefined in the
// jsdom test environment, breaking any code that touches it (e.g.
// api/runtime.ts). Install a minimal in-memory Web Storage polyfill so tests
// run regardless of the host Node's localStorage support.
function memoryStorage(): Storage {
  const map = new Map<string, string>();
  return {
    get length() {
      return map.size;
    },
    clear: () => map.clear(),
    getItem: (k: string) => (map.has(k) ? map.get(k)! : null),
    key: (i: number) => Array.from(map.keys())[i] ?? null,
    removeItem: (k: string) => void map.delete(k),
    setItem: (k: string, v: string) => void map.set(k, String(v)),
  } as Storage;
}

if (typeof localStorage === "undefined" || localStorage === null) {
  Object.defineProperty(globalThis, "localStorage", {
    value: memoryStorage(),
    configurable: true,
  });
}
if (typeof sessionStorage === "undefined" || sessionStorage === null) {
  Object.defineProperty(globalThis, "sessionStorage", {
    value: memoryStorage(),
    configurable: true,
  });
}
