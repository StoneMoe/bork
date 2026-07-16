import { fileURLToPath, URL } from "node:url";
import { defineConfig } from "vitest/config";
import solid from "vite-plugin-solid";

export default defineConfig({
  plugins: [solid()],
  resolve: {
    alias: {
      "@wailsjs": fileURLToPath(new URL("../build/wailsjs", import.meta.url)),
    },
  },
  build: {
    outDir: "../build/frontend",
    emptyOutDir: true,
  },
  test: {
    environment: "node",
  },
});
