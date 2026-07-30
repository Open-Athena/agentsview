/// <reference types="vite-plus/client" />

interface ImportMetaEnv {
  readonly VITE_BUILD_COMMIT: string;
  readonly VITE_SNAPSHOT: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}
