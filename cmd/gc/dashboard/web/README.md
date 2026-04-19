# Dashboard SPA

This directory is the TypeScript source for the Gas City dashboard.
It replaces the hand-written JSON API proxy in `cmd/gc/dashboard/`.

The SPA talks directly to the supervisor's OpenAPI-typed endpoints
(`/v0/...`) using a client generated from `internal/api/openapi.json`.
The Go service exists only to serve the compiled static bundle.

## Dev workflow

Requires Node 20+ and npm.

```bash
npm install          # one-time
npm run gen          # regenerate src/generated/schema.d.ts from the spec
npm run typecheck    # tsc --noEmit
npm test             # Vitest/jsdom dashboard tests
npm run build        # Vite production build → dist/
npm run dev          # Vite dev server with HMR on :5173
```

`dist/` is tracked because the Go static server embeds it. It is rebuilt
by CI and by the pre-commit hook (see `.githooks/pre-commit`) whenever
the dashboard source or OpenAPI spec changes. Run `make dashboard-check`
before handing off dashboard or API-schema changes; for a runtime smoke
test, run `npm run preview -- --host 127.0.0.1 --port <port>` after the
build and load the served page.

`src/generated/` is also git-ignored. `npm run gen` must be run after
any change to `internal/api/openapi.json`. The pre-commit hook does
this automatically when the spec regenerates.

## Layout

```
web/
├── index.html                 shell; the Go static server injects <meta name="supervisor-url">
├── package.json
├── tsconfig.json
├── vite.config.ts
└── src/
    ├── main.ts                entry
    ├── api.ts                 openapi-fetch client
    ├── sse.ts                 EventSource wrappers
    ├── panels/                one module per UI panel
    │   ├── status.ts
    │   ├── crew.ts
    │   ├── mail.ts
    │   ├── issues.ts
    │   ├── ready.ts
    │   ├── convoys.ts
    │   ├── activity.ts
    │   └── options.ts
    ├── util/                  small helpers
    └── generated/             openapi-typescript output (git-ignored)
        └── schema.d.ts
```

## Principle

The SPA owns zero hand-written networking: every request to the
supervisor goes through `openapi-fetch` typed on the generated
schema. If you find yourself writing `fetch("/v0/...")` directly or
parsing a response body with `JSON.parse`, stop — the typed client
already covers it.

The only endpoints the SPA talks to are under `/v0/`. `/api/*` is
gone. Any `gc` command that used to run via the old `/api/run`
endpoint is reachable directly through a typed supervisor operation
from the relevant panel.
