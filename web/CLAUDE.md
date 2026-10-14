# web/ – storecraft admin console

Vue 3 + TypeScript single-page app that talks to the storecraft Go server. These rules add to the root `CLAUDE.md`.

## Rules
- Vue 3 Composition API only, with `<script setup lang="ts">`. No Options API.
- Typed props and emits (`defineProps<...>()`, `defineEmits<...>()`).
- Shared state lives in Pinia stores. No other state library.
- Strict TypeScript. No `any`, no `@ts-ignore` / `@ts-expect-error`, no disabled lint rules (no `eslint-disable` comments, no rules turned off in the config).
- One styling approach: plain CSS in components (`<style scoped>`) plus the small shared stylesheet `src/styles/base.css`.
- API types come ONLY from `api/openapi.yaml`, generated into `src/api/schema.d.ts` by `npm run gen:api`. Never edit the generated file and never hand-write API response types. When the API changes, change `api/openapi.yaml` first, then regenerate.
- All HTTP calls go through `src/api/client.ts`. The token is sent only in the `Authorization` header, never in a URL.
- Map API errors by the envelope `error.code` (see `src/api/errors.ts`), never by matching message text.
- Use the shared components (`BaseButton`, `BaseTextField`, `BaseModal`, `ToastHost`/`useToast`) instead of raw one-off equivalents.
- Never render raw HTML (`v-html`). The session token is kept in `sessionStorage`; injected markup would expose it.
- Accessibility: every input has a label, modals trap and restore focus and close with Esc, everything is usable by keyboard, status messages are announced (`aria-live`).
- Keep the bundle small: no new dependency without asking.

- Live updates go through `useChangeFeed` (fetch + streaming reader on `/v1/changes/stream`, never EventSource, because the token must stay in the Authorization header). Lists come with a `seq` (`GET /v1/keys`); start the feed from it.
- Apply live changes by **seq**, not by version: a key's version restarts at 1 when it is deleted and re-created, seq never does (see `useKeys.applyChange`).

## Quota feature (STORE-210)
- The `quota_enforcement` flag is read ONLY through `src/stores/flags.ts` (`quotaEnabled`); App.vue polls it every 3 s while signed in. Hide every quota element behind it, except the admin flag switch.
- Error texts for quota come from the envelope code and `details` (`src/quota.ts`, `overQuotaMessage`), never from the server's message text.
- Client events go through `src/events.ts` (`track`); they stay in the browser and must never contain a token or a key's value.
- The usage meter refreshes on every change event (debounced) and every 10 s (`useUsage`).

## Approved dependencies
vue, pinia, vite, @vitejs/plugin-vue, typescript, vue-tsc, eslint, eslint-plugin-vue, typescript-eslint, vitest, @vue/test-utils, jsdom, @playwright/test, openapi-typescript, @axe-core/playwright, @types/node (approved in A6, dev only: types for the e2e setup and scripts).

Anything else (direct dependency) needs the user's approval first. Packages these pull in automatically are part of them. Versions are pinned exactly; prefer releases that are at least two weeks old. TypeScript stays on 5.x while openapi-typescript and typescript-eslint require it. `package.json` "overrides" pins `playwright-core` to the same version as `@playwright/test` (otherwise @axe-core/playwright pulls a newer one with incompatible types).

## Bundle budget
`npm run check:bundle` fails if the main JavaScript file of `npm run build` is larger than **120 kB** (uncompressed). At A6 it was about 103 kB (38 kB gzip); with the quota UI (B8) it is 115.4 kB (42.1 kB gzip), so only about 4.6 kB is left; if it is exceeded, look for an accidental heavy import before raising it, and ask the user before raising it.

## Browser tests (Playwright, Chromium only)
- `npm run e2e` runs `e2e/*.spec.ts` against the REAL Go server. `e2e/global-setup.ts` builds a private Go binary into a temp folder, seeds a fresh database with synthetic tenants, and starts the Go primary and the Vite dev server on free ports (never :8090); `e2e/global-teardown.ts` stops exactly those processes (recorded PIDs) and deletes the temp folder. Requires Go on PATH.
- Tests run one at a time (one shared server; one test restarts it).
- Every page gets an axe check with zero violations.
- One-time browser install: `npx playwright install chromium`. On this machine Node's downloader timed out, so the A6 install fetched the same official archives with curl into `%LOCALAPPDATA%\ms-playwright` (see docs/DONE.md, A6).

## Dev proxy note
`vite.config.ts` closes the browser side of a proxied response when the Go side ends abruptly, so a stopped server is noticed at once by the change feed. The 45 s watchdog in `useChangeFeed` remains the safety net for other proxies.

## Commands (run from `web/`)
- `npm install` – install the pinned dependencies
- `npm run dev` – dev server on http://localhost:5173; `/v1` and `/metrics` are proxied to the Go server on http://localhost:8090 (so the Go server needs no CORS code)
- `npm run gen:api` – regenerate `src/api/schema.d.ts` from `../api/openapi.yaml`
- `npm run lint`, `npm run typecheck`, `npm run test`, `npm run build`, `npm run check:bundle`, `npm run e2e` – all must pass after every web change
- `npm run dev` with another Go server: `$env:STORECRAFT_API_TARGET = "http://localhost:8091"; npm run dev`
