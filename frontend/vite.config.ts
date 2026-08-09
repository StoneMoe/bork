import { fileURLToPath, URL } from "node:url";
import { defineConfig } from "vite";
import solid from "vite-plugin-solid";

export default defineConfig({
  plugins: [solid()],
  resolve: {
    alias: {
      "@wailsjs": fileURLToPath(new URL("../build/wailsjs", import.meta.url)),
    },
  },
  build: {
    outDir: "../internal/webassets/dist",
    emptyOutDir: true,
  },
});
