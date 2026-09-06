import { createSignal } from "solid-js";
import { GetSystemLanguage } from "@wailsjs/go/app/App";
import english from "./locales/en";

export type Locale = "zh-CN" | "en";
export type LanguagePreference = "auto" | Locale;
const storageKey = "bork.language";

function readLanguagePreference(): LanguagePreference {
  try {
    const saved = localStorage.getItem(storageKey);
    if (saved === "zh-CN" || saved === "en") return saved;
  } catch { /* Like other UI preferences, storage is optional. */ }
  return "auto";
}

export function resolveLocale(language: string): Locale {
  return /^zh(?:[-_]|$)/i.test(language) ? "zh-CN" : "en";
}

export const [languagePreference, updateLanguagePreference] = createSignal(readLanguagePreference());
const [systemLanguage, setSystemLanguage] = createSignal(navigator.language);
export const locale = (): Locale => {
  const preference = languagePreference();
  return preference === "auto" ? resolveLocale(systemLanguage()) : preference;
};

export async function refreshSystemLanguage() {
  try {
    setSystemLanguage(await GetSystemLanguage() || navigator.language);
  } catch {
    // Browser previews have no native bridge; desktop uses the OS UI language.
    setSystemLanguage(navigator.language);
  }
}

export function setLanguagePreference(preference: LanguagePreference) {
  updateLanguagePreference(preference);
  try {
    if (preference === "auto") localStorage.removeItem(storageKey);
    else localStorage.setItem(storageKey, preference);
  } catch { /* The current window can still change language without storage. */ }
  if (preference === "auto") void refreshSystemLanguage();
}

// Chinese source messages double as keys, so a missing translation stays readable.
// Calls made while rendering read the locale signal and update without remounting UI.
export function t(source: string, values: Record<string, string | number> = {}): string {
  const text = locale() === "en" && Object.hasOwn(english, source) ? english[source] : source;
  return text.replace(/\{(\w+)\}/g, (placeholder, key: string) => Object.hasOwn(values, key) ? String(values[key]) : placeholder);
}

const sourceByEnglish = new Map(Object.entries(english).map(([source, text]) => [text.toLowerCase(), source]));

export function translateMessage(message: string): string {
  // Keep original error details. Only known application messages and their
  // colon-separated context are translated, including errors already on screen.
  return message.split(": ").map((part) => t(sourceByEnglish.get(part.toLowerCase()) ?? part)).join(": ");
}
