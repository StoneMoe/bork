// Run with node frontend/scripts/check-i18n.mjs. Uses existing frontend dependencies.
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { runInNewContext } from "node:vm";
import * as solid from "solid-js/dist/solid.js";
import ts from "typescript";

const readSource = (name) => readFileSync(new URL(`../src/${name}`, import.meta.url), "utf8");
const storage = new Map();
let osLanguage = "zh-CN";
const globals = {
  navigator: { language: "en-US" },
  localStorage: {
    getItem: (key) => storage.get(key) ?? null,
    setItem: (key, value) => storage.set(key, value),
    removeItem: (key) => storage.delete(key),
  },
};

function load(name, modules = {}) {
  const exports = {};
  const { outputText } = ts.transpileModule(readSource(name), {
    compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
  });
  runInNewContext(outputText, { ...globals, exports, require: (id) => modules[id] });
  return exports;
}

const catalog = load("locales/en.ts");
const modules = {
  "solid-js": solid,
  "./locales/en": catalog,
  "@wailsjs/go/app/App": { GetSystemLanguage: async () => osLanguage },
};
const app = load("i18n.ts", modules);
assert.equal(app.languagePreference(), "auto");
await app.refreshSystemLanguage();
assert.equal(app.locale(), "zh-CN", "native UI language overrides browser language");
for (const language of ["zh-CN", "zh-Hant-TW", "zh_CN.UTF-8", "ZH", "zh-SG"]) {
  assert.equal(app.resolveLocale(language), "zh-CN");
}
for (const language of ["en-US", "fr-FR", "C", ""]) assert.equal(app.resolveLocale(language), "en");

app.setLanguagePreference("en");
assert.equal(app.t("创建房间"), "Create room");
assert.equal(app.t("从最近房间移除 {name}", { name: "测试 {room}" }), "Remove 测试 {room} from recent rooms");
assert.equal(load("i18n.ts", modules).languagePreference(), "en", "manual choice survives reload");
app.setLanguagePreference("zh-CN");
assert.equal(app.translateMessage("Room invite is required"), "请输入房间邀请");
app.setLanguagePreference("en");
assert.equal(app.translateMessage("屏幕视频解码失败: decoder details"), "Failed to decode screen video: decoder details");
assert.equal(app.translateMessage("unrecognized details"), "unrecognized details");
for (const detail of ["constructor", "__proto__", "{constructor}", "{__proto__}"]) {
  assert.equal(app.translateMessage(`technical details: ${detail}`), `technical details: ${detail}`);
}
app.setLanguagePreference("auto");
assert.equal(storage.has("bork.language"), false);
osLanguage = "de-DE";
await app.refreshSystemLanguage();
assert.equal(app.locale(), "en", "automatic follows the current system language");
storage.set("bork.language", "invalid");
assert.equal(load("i18n.ts", modules).languagePreference(), "auto");

const placeholders = (text) => [...text.matchAll(/\{\w+\}/g)].map(([value]) => value).sort();
for (const [source, translation] of Object.entries(catalog.default)) {
  assert.deepEqual(placeholders(translation), placeholders(source), `placeholders: ${source}`);
}
function checkMessage(node) {
  if (ts.isStringLiteral(node) && /\p{Script=Han}/u.test(node.text) && node.text !== "简体中文") {
    assert.ok(node.text in catalog.default, `missing English translation: ${node.text}`);
  }
  if (ts.isJsxText(node)) assert.doesNotMatch(node.text, /\p{Script=Han}/u, "untranslated JSX text");
  ts.forEachChild(node, checkMessage);
}
for (const name of ["App.tsx", "Room.tsx", "RoomControls.tsx", "Settings.tsx", "issues.ts"]) {
  checkMessage(ts.createSourceFile(name, readSource(name), ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX));
}
console.log(`i18n checks passed (${Object.keys(catalog.default).length} messages)`);
