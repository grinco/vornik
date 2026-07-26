# telemetry.vornik.io mock endpoint

This directory is the repository-owned source for Vornik's anonymous
lifecycle-telemetry endpoint.

The website remains GitHub Pages at `vornik.io`. GitHub Pages cannot accept
POST requests and the Pages site can have only its existing `vornik.io`
custom-domain configuration, so `telemetry.vornik.io/v1/collect.json` is
handled by a narrowly scoped Cloudflare Worker Route.

The Worker:

- accepts only `POST /v1/collect.json`;
- requires `Content-Type: application/json`;
- limits the body to 4 KiB;
- rejects an unrecognised `e=` event with HTTP 400;
- returns `response.json` with HTTP 202;
- does not parse, store, log, identify, or forward the body;
- records only the allowlisted **query** dimensions, each bucketed against a
  closed vocabulary before use (see below);
- sets `Cache-Control: no-store` and no cookies;
- leaves every other website path outside the configured Worker Route.

## What is recorded

The URL dimensions exist so the edge can count basics without reading the body.
Two surfaces consume them, and nothing else is retained — no body, no headers,
no cookies, no client address:

| Surface | Contents | Retention |
|---|---|---|
| Workers Logs | one `{"msg":"collect",...}` line per accepted event | ~7 days |
| Analytics Engine (`TELEMETRY` → `vornik_lifecycle_v1`) | `blobs=[event, version, os, arch, source, template]`, `doubles=[autonomy]`, `indexes=[event]` | ~90 days |

Every dimension is bucketed to a closed set first — unknown OS/arch/source
becomes `other`, a non-catalog template becomes `custom`, and a version that is
not a bounded release identifier becomes `other`. That keeps a hostile or
misconfigured client from inflating cardinality, and keeps a `git describe`
build stamp (which identifies one commit) out of the store.

Analytics Engine needs the Workers Paid plan. On free tier the binding is absent
and the Worker degrades to accept-and-log; emission keeps working.

Query it over the SQL API:

```bash
curl "https://api.cloudflare.com/client/v4/accounts/$CF_ACCOUNT/analytics_engine/sql" \
  -H "Authorization: Bearer $CF_API_TOKEN" \
  --data "SELECT blob1 AS event, blob2 AS version, blob3 AS os, blob6 AS template,
                 SUM(_sample_interval) AS events
          FROM vornik_lifecycle_v1
          WHERE timestamp > NOW() - INTERVAL '7' DAY
          GROUP BY event, version, os, template
          ORDER BY events DESC"
```

## Test

Node 20 or newer:

```bash
cd deployments/telemetry-server
npm test
```

## Deploy the Worker

There is no CI workflow or stored Cloudflare credential in this repository.
The optional Wrangler path uses an interactive Cloudflare login to deploy the
preview Worker; the dashboard path does not require a CLI.

### Dashboard editor

1. In **Workers & Pages**, open the `vornik-telemetry-mock` Worker and choose
   **Edit code**.
2. Replace the deployed module with `worker.js` from this directory and deploy.
3. Do not use Pages, static asset upload, or repository asset upload.

### Wrangler

```bash
cd deployments/telemetry-server
npm install
npx wrangler login
WRANGLER_SEND_METRICS=false npm run deploy
```

`wrangler.jsonc` now declares the production route, so `npm run deploy` updates
**production**, not just the `workers.dev` preview. To stage a build without
promoting it, use `npx wrangler versions upload`.
`WRANGLER_SEND_METRICS=false` disables Wrangler's own CLI usage telemetry for
this deployment.

## Production route

Declared in `wrangler.jsonc` so a fresh deploy reproduces the live endpoint:

- Zone: `vornik.io`
- Route: `telemetry.vornik.io/v1/collect.json*`

It was previously dashboard-only, which meant the repository could not
reproduce the deployment. If the route already exists by hand, confirm the
pattern matches exactly so a deploy updates it rather than adding a second one.

The `telemetry` DNS record must remain proxied. Do not add Logpush,
request-body logging, cookies, or visitor identifiers — Workers Logs and the
Analytics Engine dataset carry only the bucketed query dimensions listed above,
and that is the whole of what may be retained.

Verify a deployment with:

```bash
curl -i -X POST \
  -H 'Content-Type: application/json' \
  --data '{"schema_version":1,"event":"install_succeeded"}' \
  'https://telemetry.vornik.io/v1/collect.json?e=install_succeeded&sv=1&v=test&os=linux&arch=amd64&source=quickstart'
```

Expected: HTTP 202 and the JSON in `response.json`.

Production client emission is enabled. Any Worker or route change must retain
the privacy controls above and pass the local tests and live synthetic check.
