# AI Gateway — "Precision" Design System (v2 revamp)

This is the single source of truth for the UI revamp. Every page must conform.
If you need a pattern that is not listed here, keep it *boring and consistent*
with what the primitives in `src/components/ui.tsx` do.

## 1. Direction

**Precision minimal** — a modern infra console (Linear / Vercel / Tailwind UI
family):

1. **Cool slate neutrals, one indigo accent.** Semantic color only for status:
   green = healthy/positive, amber = degraded/warning, red = error/destructive.
   Indigo (`accent`) = interactive emphasis, active state, brand. Never use
   `accent` to mean "good" anymore.
2. **Sans-only.** Inter everywhere (the `font-display` class is redefined to
   Inter semibold, tight tracking — it is *not* a serif anymore).
   JetBrains Mono for IDs, numbers, code, timestamps.
3. **Hairlines over shadows.** Cards are `border border-stone bg-surface` with
   at most `shadow-card`. Shadows (`shadow-pop`) only for modals, popovers,
   toasts, mobile drawer.
4. **Small radii.** Controls & cards: `rounded-lg`. Modals & plates: `rounded-xl`.
   Pills/badges: `rounded-full`.
5. **Density with rhythm.** 4px base scale. Page vertical rhythm is
   `space-y-6` between major blocks. Controls are h-9 (md), h-8 (sm), h-10 (lg).
6. **Status discipline.** healthy/up/succeeded/active → `good` (emerald);
   degraded/pending/expired → `warn` (amber); down/failed/revoked → `bad`
   (danger); informational/neutral → neutral. `HealthDot`: up = `bg-good`,
   down = `bg-danger`, unknown = `bg-muted/60`.

## 2. Tokens (defined in `src/index.css`, wired in `tailwind.config.js`)

Two themes, toggled by `html.light` (light is the default; `:root` is dark).
**Plates are always dark in both themes** (`bg-ink text-cream` = near-black
slate plate with light text) — this replaced the old light-plate-in-dark-mode
behavior.

| token      | light (RGB)      | dark (RGB)       | meaning |
|------------|------------------|------------------|---------|
| graphite / `app`  | 246 247 249 (#F6F7F9) | 10 15 26 (#0A0F1A) | page background |
| `surface`  | 255 255 255      | 17 24 39 (#111827) | cards, panels |
| `raised`   | 238 241 245 (#EEF1F5) | 30 41 59 (#1E293B) | hover / subtle fills |
| `paper`    | 11 18 32 (#0B1220)  | 231 236 245 (#E7ECF5) | primary text |
| `stone`    | 226 231 239 (#E2E7EF) | 40 51 71 (#283347) | borders / hairlines |
| `muted`    | 91 102 122 (#5B667A) | 148 163 184 (#94A3B8) | secondary text |
| `danger`   | 220 38 38 (#DC2626)  | 248 113 113 (#F87171) | error / destructive |
| `accent`   | 79 70 229 (#4F46E5)  | 129 140 248 (#818CF8) | indigo, interactive |
| `onaccent` | 255 255 255      | 8 12 24 (#080C18)   | text on solid accent |
| `ink`      | 17 24 39 (#111827)   | 5 8 15 (#05080F)    | plate background (always dark) |
| `cream`    | 241 245 249 (#F1F5F9) | 241 245 249 (#F1F5F9) | text on plates (always light) |
| `amber`    | 180 83 9 (#B45309)   | 251 191 36 (#FBBF24) | warning |
| `good`     | 4 120 87 (#047857)   | 52 211 153 (#34D399) | success / healthy |

Rules:
- On plates (`bg-ink`), accents and hovers must use light values that read on
  dark: `text-indigo-300`, `border-cream/15`, `hover:bg-cream/5`,
  `text-cream/70`. Never `text-accent` (indigo-600 on near-black fails
  contrast) and never `border-stone` (invisible on a dark plate).
- Tinted status fills are always `bg-<color>/10 text-<color> border border-<color>/30`
  (e.g. `bg-good/10 text-good border-good/30`). The `good` utility exists in
  tailwind config — use it for positive status.
- Solid danger buttons do **not** exist. The `danger` button variant is tonal
  (red tint + red text + red hairline). Destructive actions use
  `<Button variant="danger">` or `Confirm`.
- Destructive *text* (error notes) = `text-danger` with `bg-danger/10
  border-danger/25`.

## 3. Typography scale

- Root = 16px (the old `html { font-size: 15px }` is gone — do not re-add).
- Page title: `text-2xl` (24px), `font-display` (Inter 600, `-tracking-tight`),
  `leading-tight`. Never `text-3xl`/`text-4xl` for page titles.
- Section heading (`Section`): handled by the primitive (`text-[15px] font-semibold`).
- Hero marketing-style headlines (login panel, dashboard plate) may go up to
  `text-3xl md:text-4xl` max — the old `text-5xl` hero is gone.
- Stat value: `text-2xl font-semibold tabular-nums` (was 28px display).
- Body / table cells: `text-sm` (14px). Secondary: `text-xs` (12px).
- Uppercase micro-labels only in table headers and eyebrows
  (`text-[11px] uppercase tracking-wider font-semibold text-muted`).
- Mono: `font-mono tabular-nums` for numbers in tables, IDs, timestamps, code.

## 4. Page anatomy (every page must follow)

```
<PageHeader title description actions />   // one per page, always first
  → toolbar / filter row (if any)          // Card pad=false or flex gap-2 row
  → primary content (tables, grids)        // TableShell, Card, grid
  → secondary content                       // Section blocks
```

- Major blocks separated by `space-y-6` at the page root (the page root is a
  `<div className="space-y-6">` or `space-y-6` equivalent).
- Tables: use `TableShell` + `Th` + `Td`. Interactive rows get
  `hover:bg-raised/60 transition-colors`. Numeric columns get
  `className="text-right font-mono tabular-nums"` on `Td`/`Th`.
- Empty states: `EmptyState`. Loading: `Skeleton` / `TableSkeleton`. Errors:
  `ErrorNote`.
- Modals: only via `Modal`/`Confirm` primitives.
- Toolbars: filter controls in one row (`flex flex-wrap items-center gap-2`),
  primary action pushed right with `ml-auto`.

## 5. Components (`src/components/ui.tsx`)

Read the file — do not guess. Summary of the v2 primitives:

- `Button` — `sm` h-8 / `md` h-9 / `lg` h-10, `rounded-lg`, `font-medium`.
  Variants: `primary` (solid indigo), `secondary` (border + surface),
  `ghost`, `subtle` (indigo tint), `danger` (red tint, tonal).
- `Card` — `rounded-lg border border-stone bg-surface shadow-card`, pad `p-4`
  by default (`pad={false}` for table/tool containers).
- `PageHeader` — `text-2xl` title, optional `eyebrow`, `description`,
  `actions` slot on the right.
- `Eyebrow` — small indigo uppercase label with hairline.
- `Badge` — pill; tones `neutral | good | warn | bad | info | brand`
  (good = emerald, info = indigo tint).
- `HealthDot` — green/gray/red per §1.
- `Stat` — compact KPI card, 32px icon chip, `text-2xl` value, delta pill.
- `Progress` — 4px bar; tones `accent | amber | red`.
- `Field` / `Input` / `Select` / `Textarea` — h-9, `rounded-lg`, labels are
  `text-xs font-medium text-muted` (not uppercase).
- `TableShell` / `Th` / `Td` — as §4. `TableShell` owns the single
  `<table>` element: pass `minWidth={n}` for horizontal scroll thresholds
  (renders as an inline min-width on the table) and `footer={...}` for a
  pinned bar under the table (legend/note rows). Never nest a second
  `<table>` inside it — children are `thead`/`tbody`/`TableSkeleton` only.
- `EmptyState`, `Skeleton`, `TableSkeleton`, `ErrorNote` — as-is roles.
- `Modal` / `Confirm` — `rounded-xl`, `shadow-pop`, compact header
  (`text-[15px] font-semibold`).
- `SegmentedControl` — active = dark chip (`bg-ink text-cream`).
- `Toaster`, `useToast`, `useClickOutside`, `CopyButton`, `Avatar`,
  `Section`, `useSection` — same APIs as before.

**API stability rule:** do not change exported component *props*. You may restyle
a primitive's internals only in `ui.tsx` (Lead owns that file — pages must not
redefine primitives).

## 6. Shell (owned by `App.tsx` — do not edit in page work)

- Sidebar 260px expanded / 68px collapsed; **single-line nav items**
  (icon chip + label + optional right-side count); group headers are quiet
  `text-[11px] uppercase tracking-wider text-muted`.
- Sticky header h-14: hamburger (mobile), breadcrumb `group / page`, right
  side: search (⌘K), theme toggle, user menu.
- Content: `max-w-[1380px]`, `px-5 lg:px-8 py-6 lg:py-8`.
- Nav groups (order matters): **Operate** (Overview, Playground, Requests,
  Performance) · **Connect** (Providers, Models, Routing, Settings) ·
  **Govern** (API Keys, Teams, Users, Webhooks, Audit) · **Optimize**
  (Analytics, Attribution, Costs). Settings moved out of Optimize.

## 7. Hard rules for page work

1. **Restyle only — no behavior changes.** Keep every handler, effect, fetch,
   state variable, permission check, and prop exactly as-is. If logic looks
   wrong, leave it and note it in your report.
2. **Only touch your assigned files.** Never edit `App.tsx`, `ui.tsx`,
   `index.css`, `tailwind.config.js`, `index.html`, `DESIGN.md`, or another
   group's pages.
3. Use existing primitives; do not hand-roll new button/card/input styling.
   Page-local layout components are fine (toolbars, tabs, panes).
4. Remove one-off styling debris: `!important` overrides, hardcoded grays/blues
   that fight the palette, `text-5xl`-style heroes, inline hex colors, serif
   assumptions.
5. Preserve accessibility: labels, `aria-*`, keyboard handlers, focus states
   (`focus-visible` is global).
6. No new dependencies.
7. Do NOT run `npm run build`, `tsc -b`, or write to `cmd/gateway/web` — the
   Lead runs typecheck/build after all groups land. Proofread your JSX instead.
