import { fileURLToPath, URL } from "node:url";
import { defineConfig } from "vite";
import solid from "vite-plugin-solid";

export default defineConfig({
  plugins: [solid()],
  resolve: {
    alias: [
      { find: /^@game-proxy$/, replacement: fileURLToPath(new URL(
        process.env.BORK_GAME_PROXY === "1" ? "./src/game-proxy.enabled.tsx" : "./src/game-proxy.disabled.tsx",
        import.meta.url,
      )) },
      { find: "@wailsjs", replacement: fileURLToPath(new URL("../build/wailsjs", import.meta.url)) },
    ],
  },
  build: {
    outDir: "../internal/webassets/dist",
    emptyOutDir: true,
  },
});
