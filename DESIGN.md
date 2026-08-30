# Bork Design System

This document extracts the current SolidJS/Wails desktop UI. It is descriptive, not a redesign: named tokens come from `frontend/src/base.css`; component and layout rules come from the five `frontend/src/*.css` files and their TSX consumers. No greenfield research log applies.

## 1. Atmosphere & Identity

Bork feels like a cool-neutral CJK desktop command center: compact, quiet, technical, and immediately legible while voice and network state change around it. Its signature is restrained operational depth: a shallow blue-gray atmospheric page, dense Microsoft/PingFang/Noto typography, and raised controls separated by small tonal shifts, fine borders, and selective shadows rather than decorative cards.

## 2. Color

### Palette

All theme pairs use `light-dark()` and follow `color-scheme`; `data-theme="light"` and `data-theme="dark"` override the system preference.

| Role | Token | Light | Dark | Current use |
| --- | --- | --- | --- | --- |
| Page | `--page-background` | `#e5e7ea` | `#08090b` | App and settings background |
| Gradient/start | `--page-gradient-start` | `#eef0f2` | `#07080a` | Body atmosphere |
| Gradient/end | `--page-gradient-end` | `#dde1e5` | `#101216` | Body atmosphere |
| Surface/raised | `--surface-raised` | `var(--page-background)` | `#14171b` | Popovers, floating cards, issues |
| Surface/input | `--surface-input` | `#eef0f2` | `#0f1115` | Fields, rows, listboxes |
| Surface/hover | `--surface-hover` | `#dde1e5` | `#191c21` | Hovered/focused controls |
| Text/strong | `--text-strong` | `#16191f` | `#f0f1f3` | Titles and selected state |
| Text/default | `--text-default` | `#303641` | `#d6d8de` | Primary UI copy |
| Text/muted | `--text-muted` | `#59616e` | `#9297a2` | Labels and supporting copy |
| Text/faint | `--text-faint` | `#5b6370` | `#78808c` | Metadata and inactive tabs |
| Text/on color | `--text-on-color` | `#fff` | `#fff` | Destructive filled control |
| Accent | `--accent` | `#4963a6` | `#91a7e8` | Selection, focus, counts |
| Accent/strong | `--accent-strong` | `#334f96` | `#aebdf0` | Focus outline and emphasis |
| Accent/muted | `--accent-muted` | `#526797` | `#8897bd` | Secondary action and status |
| Accent/faint | `--accent-faint` | `#68779c` | `#66718c` | Low-emphasis iconography |
| Divider | `--line` | `rgba(0, 0, 0, 0.12)` | `rgba(255, 255, 255, 0.085)` | Shell, drawer, major dividers |
| Danger | `--danger` | `#a7483f` | `#d9877a` | Failure, muted/clipped state |
| Danger/solid | `--danger-solid` | `#b33f3f` | `#c84f4f` | Window close hover |
| Danger/surface | `--danger-surface` | `#fff0ee` | `rgba(34, 20, 22, 0.97)` | Defined danger surface |
| Danger/text | `--danger-text` | `#7c302a` | `#e0b4ae` | Defined danger copy |
| Danger/strong | `--danger-strong` | `#913a32` | `#e49b90` | Error title/dismiss action |
| Danger/hover | `--danger-hover` | `#68231e` | `#ffd5cf` | Destructive hover |
| Success | `--success` | `#267354` | `#83dfba` | Connected, active, copied |
| Warning | `--warning` | `#775818` | `#e1b867` | Attention state |
| Scrollbar | `--scrollbar-thumb` | `#a3a9b3` | `#4f5870` | Thin scrollbar thumb |
| Scrollbar/hover | `--scrollbar-thumb-hover` | `#7c8490` | `#68738f` | Hovered thumb |

`--contrast-rgb` is `0, 0, 0` in light and `255, 255, 255` in dark. `--accent-rgb`, `--danger-rgb`, `--success-rgb`, `--warning-rgb`, and `--shadow-rgb` provide alpha variants: light values are `73, 99, 166`, `167, 72, 63`, `38, 115, 84`, `119, 88, 24`, and `36, 40, 50`; dark values are `145, 167, 232`, `217, 135, 122`, `131, 223, 186`, `225, 184, 103`, and `0, 0, 0`.

### Rules

- Use semantic variables directly, or the matching `*-rgb` variable for transparent borders, fills, glows, and shadows.
- Accent communicates focus, selection, active controls, topology/status metadata, and calm emphasis. Success, warning, and danger retain their operational meanings.
- The body atmosphere is the existing pair of low-alpha accent radial gradients over a 135-degree page gradient.
- Screen-video chrome is an existing media-specific exception in `room.css`: `#08090d` / `#f3f5fa` and black overlays remain isolated from ordinary application surfaces.

## 3. Typography

### Font Stacks

- UI `--font-ui`: `"Microsoft YaHei UI", "Microsoft YaHei", "PingFang SC", "Hiragino Sans GB", "Noto Sans CJK SC", "Noto Sans SC", "Source Han Sans SC", sans-serif`.
- Mono `--font-mono`: `"Cascadia Mono", "Sarasa Mono SC", "Source Han Mono SC", "Noto Sans Mono CJK SC", "Microsoft YaHei UI", "PingFang SC", monospace`.
- `font-synthesis: none`; UI controls inherit the application font. Mono is reserved for the wordmark, addresses, timings, versions, counts, shortcuts, and other machine data.

### Observed Scale

The scale is implicit rather than tokenized; these are the values present in CSS.

| Size | Typical weight / line height | Current role |
| --- | --- | --- |
| `19px` | `650 / 1.2` | Lobby subview H1 |
| `17px` | `580` | Voice-stage title |
| `15px` | `800 / 1` or form default | Wordmark and prominent lobby input |
| `14px` | `400 / 1.65`, `650` | Explanatory copy and floating-card title |
| `13px` | `400 / 1.5`, `620-650` | Base text inputs, member names, primary actions |
| `12px` | `600-650 / 1-1.5` | Settings controls, labels, row copy |
| `11px` | `600-650 / 1-1.55` | Dense labels, statuses, diagnostic values |
| `10px` | `600-700 / 1-1.5` | Section labels, metadata, diagnostic detail |
| `9px` | `1.1-1.3` | Secondary machine metadata |
| `7px` | `700 / 1` | PTT shortcut badge only |

Tracking is used sparingly: wordmark `0.18em`, room name and section labels `0.08em`, action copy around `0.035-0.05em`, diagnostic sublabels `0.04em`, and the 19px H1 `-0.015em`.

## 4. Spacing & Layout

### Base Unit

The implicit base is **4px**. There are no named spacing variables; preserve the observed rhythm rather than inventing token names.

| Step | Current use |
| --- | --- |
| `4px` | Tight text/icon gaps, compact breakpoint gaps |
| `8px` | Default compact gap/padding, row padding, focus offset family |
| `12px` | Shell padding, ordinary gap, compact surface padding |
| `16px` | Panel/card padding and grouped spacing |
| `20px` | Room mobile inset and icon columns |
| `24px` | Settings section padding, topbar horizontal padding, major inset |
| `28px` | Narrow page/dialog clearance |
| `32px` | Room section inset and separated groups |
| `40-44px` | Touch/control dimensions and modal viewport clearance |

Half-step and legacy values (`3`, `5`, `6`, `7`, `9`, `10`, `11`, `13`, `14`, `17`, `18`, and `22px`) are already common in compact controls and are recorded as debt in Section 8, not normalized here.

### Shell, Grid, and Scroll Ownership

- `--content-max-width: 1280px`; `.shell` is full-size with `70px 1fr` rows and `12px` inset, dropping to `58px` and `7px` below `640px`. Maximized windows remove the outer inset.
- `.topbar` and `.main-view` share the content maximum. The topbar is a Wails drag region; buttons opt out.
- The room centers a vertical `.room-peers` stack up to `900px` wide and `560px` high. Lists own their internal vertical scroll.
- The settings layer is fixed. `.settings-drawer` is `min(480px, 92vw)`, full-height, and a two-row grid: tabs plus `minmax(0, 1fr)`. The drawer itself stays `overflow: hidden`; `.settings-content` is the only panel scroll owner, with `overflow-y: auto`, `overscroll-behavior: contain`, and stable scrollbar gutter. Tabs and issue overlay do not scroll with panel content.
- Settings sections use `24px` padding. Setting rows are horizontal clusters with a text stack and fixed/compact control, `min-height: 50px`, and `18px` gap.
- Existing viewport breakpoints are `760px`, `640px`, and `400px`. Narrow layouts collapse lobby forms and transfer grids, reduce shell/room insets, and preserve one readable content column.

## 5. Components

The following existing primitives and states are the contract for the upcoming game-proxy Settings tab.

### Application Shell and Topbar

- **Structure/layout**: `.shell` grid -> `.topbar` cluster + `.main-view`; title/actions truncate rather than expanding the shell.
- **Variants/states**: lobby/room wordmark, copied success, warning/error attention, disabled/waiting actions, maximized shell, native window controls.
- **Accessibility**: every icon button has an `aria-label` and title; copy/error changes use visually hidden live status/alert nodes; focus is restored after overlays close.
- **Motion/depth**: 120ms control feedback, 80ms press translation, fine divider, tonal hover, selective inset ring.

### Button and Control Families

- **Structure**: icon-only controls use inline stroke SVGs with `currentColor`; text controls pair a label stack with an optional leading/trailing icon. Existing families are `.topbar-icon-button`, `.window-control-button`, `.lobby-entry-button`, `.feature-button`, `.audio-icon-button`, and compact transfer/dismiss actions.
- **Variants/states**: default, hover/focus, pressed, disabled/waiting; semantic modifiers include create, copied, active, open, attention, muted, clipped, and destructive close. Active/open is accent or success, attention is warning, and clipped/destructive is danger.
- **Spacing/layout**: icon targets are 24-40px depending on hierarchy; primary rows are 42-54px. SVGs are normally 14-22px with 1.45-1.8px round strokes.
- **Accessibility**: icon-only buttons require an `aria-label`; toggles expose `aria-pressed` or `aria-expanded`; disabled actions preserve a visible waiting/disabled state.
- **Motion/depth**: global 80ms press translation and 120ms semantic feedback; copied confirmation uses `ui-pop`, and pending file attention uses the bounded pulse.

### Floating Card and Context Popover

- **Structure/layout**: `.floating-card` is a fixed, centered, bounded dialog/popover with a 60px header and internally scrolling body. Attention, gain, member-info, Select, and tracker surfaces reuse the same raised-surface/accent-border/shadow language, with local positioning and bounded scroll ownership.
- **Variants/states**: native popover or fallback-open, centered dialog, anchored member info, hover/focus disclosure, above/below placement, open/closed visibility, and transparent or dimmed backdrop according to context.
- **Accessibility**: dialogs are labelled/described; anchored surfaces remain keyboard-openable and Escape-dismissible; scroll is contained so the shell does not move.
- **Motion/depth**: `ui-enter` for substantial surfaces, `ui-fade` for small anchored context, 120ms opacity/translation for hover/focus disclosures.

### Stateful Rows, Lists, and Progress

- **Structure/layout**: `.member-row`, `.transfer-row`, `.file-recipient-list`, and source/diagnostic rows use `minmax(0, 1fr)` grids, truncating primary text while preserving compact state/action columns. Their list or panel owns vertical scroll where content is bounded.
- **Variants/states**: local, hover/focus-within, speaking success rail, active/open, empty, pending, failed danger, muted, and clipped. Progress uses an accent gradient and switches to danger on failure.
- **Accessibility**: semantic list/listitem or labelled panel structure, named actions, native progress/meter semantics, and selectable hashes/addresses.
- **Motion/depth**: rows enter with `row-enter 140ms ease-out`; speaking uses opacity/scale on a success rail; progress changes over 160ms and live input level over 70ms.

### Settings Drawer, Tabs, and Panels

- **Structure/layout**: `.settings-layer` -> backdrop + modal `.settings-drawer`; `.settings-tabs[role=tablist]` precedes the single `.settings-content` scroll owner; each `.settings-section.settings-panel[role=tabpanel]` is hidden when inactive.
- **Spacing**: 44px tabs, 24px panel padding, fixed 480px/92vw drawer width.
- **States**: tab default, hover/focus, active accent underline/tonal fill; optional warning/error `.settings-issue`; open/close via backdrop or Escape.
- **Accessibility**: `role=dialog`, `aria-modal`, roving tab index, Arrow Left/Right and Home/End navigation, initial tab focus, contained Tab loop, labelled panels, and focus return to the settings trigger.
- **Motion/depth**: backdrop `ui-fade`; drawer `settings-drawer-in`; left border plus left-cast shadow.

### Setting Row

- **Structure/layout**: `.setting-row` horizontal cluster with a leading text stack (`strong` + optional `small`) and trailing control. `.audio-toggle` adds row hover/focus fill and sibling dividers.
- **Spacing**: 50px minimum height, 18px main gap; current compact toggle treatment uses `0 8px`, 5px text gap, and an 88px trailing control.
- **States**: default, row hover/focus-within, checked/pressed, disabled, and conditional child rows.
- **Accessibility**: use a wrapping `label` when the trailing control is labelable; otherwise provide explicit control naming and status text.
- **Reuse**: game-proxy boolean or key/value preferences should compose this row before introducing another row anatomy. The class name `.audio-toggle` is legacy, not a new semantic requirement.

### Text Field and Custom Select

- **Structure/layout**: text fields use the global input treatment; `Select.tsx` provides `.select` -> combobox trigger + conditional listbox/options. The listbox owns its own bounded scroll and can open above.
- **Spacing**: settings controls are 38px high with 7px radius; select trigger uses 10/12px horizontal padding and listbox uses 4px padding.
- **States**: default, hover/focus border and tonal fill, open chevron rotation, active option/checkmark, disabled opacity, long-label ellipsis.
- **Accessibility**: labelled combobox, `aria-expanded`, `aria-controls`, `aria-activedescendant`; Arrow/Home/End, Enter/Space, typeahead, Escape, Tab close, and outside-pointer close. DOM focus remains on the trigger.
- **Motion/depth**: 120ms chevron/control transitions; raised listbox uses accent border and prominent popover shadow.

### Checkbox, Segmented Choice, and Compact Action

- **Structure**: native checkbox with custom check/cross data-URI assets; `.theme-button-group[role=group]` for mutually exclusive pressed buttons; `.push-to-talk-key-button` for a compact captured value.
- **States**: checkbox unchecked/checked, hover/focus, active, disabled; segmented hover/focus and `aria-pressed=true`; compact action hover/focus, capture text, disabled.
- **Accessibility**: retain native checkbox semantics, explicit group labels, `aria-pressed`, and a polite live region for capture mode.
- **Motion/depth**: 80ms checkbox press scale, 120ms color/fill/border feedback; grouped controls use a single input surface and internal borders.

### Diagnostic Section and Data Group

- **Structure/layout**: `.diagnostic-section` stack -> `.diagnostic-heading` + value, empty text, list, or `.tracker-group`; machine values use mono and permit selection where copying matters.
- **Spacing**: 9px internal gap, 22px between sections, 8px list-row rhythm, 38-48px grouped rows.
- **States**: populated/empty/waiting, failed danger value, row hover/focus, tooltip above/below, tooltip dismissed with Escape.
- **Accessibility**: semantic lists/articles, focusable tooltip owner with `aria-describedby`, `role=tooltip`, keyboard dismissal, overflow wrapping or intentional ellipsis/title for long addresses.
- **Reuse**: game-proxy runtime status and endpoint data should use this heading/value/group grammar rather than a new card family.

### Attention and Error Item

- **Structure/layout**: shared `.attention-item` is a two-column message/dismiss row used in the topbar popover and as `.settings-issue` above scrollable settings content.
- **States**: warning default, `.error` danger variant, dismiss hover/focus, empty state removes the topbar attention center.
- **Accessibility**: descriptive trigger count, expanded state, labelled dismiss action, Escape close, live alert announcement, and focus recovery.
- **Motion/depth**: 120ms popover reveal; warning/danger border, raised surface, and shadow distinguish it without a new palette.

### Fourth Settings Tab Integration Contract

Add the future tab to the existing `settingsTabs` model and use the same tab/tabpanel IDs, ARIA linkage, roving keyboard behavior, `.settings-section` inset, and `.settings-content` scroll owner. Compose setting rows, existing form controls, diagnostic groups, and attention/error items as applicable. The current three-column CSS coupling must be changed when the fourth tab is implemented; it is not changed by this extraction.

## 6. Motion & Interaction

| Pattern | Actual timing | Use |
| --- | --- | --- |
| Button feedback | `120ms ease`; press transform `80ms ease` | Color, background, border, shadow, 1px press |
| Input/select feedback | `120-140ms ease` | Border/fill and chevron |
| `ui-enter` / `row-enter` | `140-170ms ease-out` | Lobby, cards, rows, screen stage |
| `ui-fade` | `120-160ms ease-out` | Backdrop, popover, preview |
| `ui-pop` | `180ms ease-out` | Copied checkmark |
| Settings drawer | `170ms ease-out` | Fade plus 14px horizontal entry |
| Popover reveal | `120ms ease` | Opacity, 3-4px translation, visibility |
| Attention pulse | `720ms ease-out`, 3 iterations | Pending file indicator |
| Waiting breathe | `2.4s ease-in-out`, infinite | Waiting state only |
| Live meter / progress | `70ms linear` / `160ms ease` | Audio level and file progress |

Interaction motion communicates entry, open/closed state, confirmation, waiting, or live progress. Existing animation uses opacity and transform for movement, while interactive transitions also change semantic color, border, background, and shadow. `prefers-reduced-motion: reduce` globally reduces animation/transition duration to `0.01ms`, limits animation to one iteration, removes button/checkbox/range press transforms and slider rings, and separately suppresses meter/progress transitions.

## 7. Depth & Surface

### Strategy: Mixed Tonal, Border, and Shadow Hierarchy

- **Base**: page gradients create atmosphere; `--page-background`, `--surface-input`, `--surface-hover`, and `--surface-raised` establish tonal layers.
- **Fine separation**: `--line` handles structural dividers. Compact rows and controls use 1px `contrast-rgb` borders, usually alpha `0.04-0.18`; emphasized controls use `accent-rgb` borders, usually alpha `0.13-0.58`.
- **Low lift**: history/recipient hover uses `0 5px 18px rgba(var(--shadow-rgb), 0.12)`; settings issues use `0 5px 14px ... 0.16`; lobby primary uses `0 7px 24px ... 0.10`.
- **Raised**: attention/member popovers use `0 8px 22px ... 0.18`; listboxes, floating cards, gain controls, and tracker tooltips use `0 8px 24px ... 0.22`.
- **Overlay**: settings uses `-10px 0 28px ... 0.20` and a `0.56` backdrop. The draggable video stage uses its isolated black media surface and `0 10px 28px rgba(0,0,0,0.28)`.
- **Shape**: controls cluster around 7-8px radii; compact internals use 4-6px; popovers/panels use 9-12px; pills, tracks, and circles use `999px`/`50%`.

## 8. Accessibility Constraints & Accepted Debt

### Constraints

- No formal WCAG version or contrast target is declared in the current source. Preserve the implemented baseline: a 2px `--accent-strong` `:focus-visible` outline with 2px offset on buttons and focusable elements; component focus treatments supplement it.
- Settings must remain fully keyboard reachable: modal Tab containment, Escape close, focus entry/return, tablist arrow/Home/End navigation, and Select keyboard/typeahead behavior.
- Keep semantic roles and relationships already present: dialog/tablist/tab/tabpanel, combobox/listbox/option, tooltip, group, status/alert/live regions, and labelled icon-only controls.
- Keep `prefers-reduced-motion` handling described in Section 6. On `hover: none`, screen overlay controls remain visible rather than hover-gated.
- Global `user-select: none` is intentionally relaxed for inputs, textareas, diagnostic addresses, codes, and issue messages that users may need to copy.
- Long CJK labels and network values must retain the existing `min-width: 0`, ellipsis, `overflow-wrap: anywhere`, and bounded-scroll behavior. The settings content remains the sole drawer scroll owner.

### Accepted Debt

| Item | Location | Why accepted in this extraction | Owner / Exit |
| --- | --- | --- | --- |
| `Settings.tsx` is 565 lines and combines modal focus management, three tabs, preferences, device sync, diagnostics, tooltip positioning, and formatting helpers. | `frontend/src/Settings.tsx` | This task documents the existing system and does not refactor behavior before the game-proxy edit. | Frontend / split only with behavior-preserving coverage when Settings is next structurally refactored. |
| No frontend test framework or test script exists. | `frontend/package.json` | Current scripts are only `dev`, `build`, and `typecheck`; adding infrastructure is outside extraction scope. | Frontend / establish tests when interactive Settings logic is changed with an approved test task. |
| Wails bindings are generated and ignored rather than reviewed source. | `build/` imports such as `@wailsjs/go/app/App`; `AGENTS.md` | This is the repository's existing build contract. | Build tooling / regenerate through the native build workflow; never hand-edit generated bindings. |
| Spacing is based on 4px but not tokenized, with many legacy non-4px values. | All five `frontend/src/*.css` files | Normalizing values would be a redesign and could alter compact desktop geometry. | Frontend/design / consolidate only during an approved visual-system refactor. |
| The settings tab track count is hard-coded to three while tab data lives in TSX. | `settings.css` `.settings-tabs`; `Settings.tsx` `settingsTabs` | Existing UI has exactly three tabs; no generic abstraction is needed yet. | Upcoming game-proxy tab / update the CSS track count together with the fourth tab. |
| Dense metadata reaches 7-11px and typography sizes/weights are not named tokens. | `room-controls.css`, `settings.css`, `app.css` | These values are current command-center density, and this task has no measured accessibility or redesign mandate. | Frontend/design / evaluate contrast and legibility before changing dense metadata surfaces. |
