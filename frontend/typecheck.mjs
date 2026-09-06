import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

const config = process.env.BORK_GAME_PROXY === "1" ? "tsconfig.game_proxy.json" : "tsconfig.json";
const result = spawnSync(process.execPath, [
  fileURLToPath(new URL("./node_modules/typescript/bin/tsc", import.meta.url)),
  "--project", config, "--noEmit",
], { cwd: fileURLToPath(new URL(".", import.meta.url)), stdio: "inherit" });
if (result.error) console.error(result.error);
process.exit(result.status ?? 1);
