# API gateway (Kong DB-less) — operator setup

The gateway fronts authenticated third-party HTTP APIs for the agent's
`query_api` tool. Kong (DB-less, declarative `kong.yml`) owns each provider's
credential and injects it upstream; the daemon holds **zero** third-party
secrets and presents only an internal `apikey` (key-auth) token. The credential
is therefore invisible to the LLM. First provider shipped: **Google Maps**.

Design: `https://docs.vornik.io`
(§2, §5.1, §7). Gateway selection spike:
`https://docs.vornik.io`.

## 1. Secrets file — `~/.config/vornik/secrets/gateway.env`

The env-file is sourced by the daemon (→ `VORNIK_GATEWAY_TOKEN`) **and** mounted
into the Kong container. Create it mode `0600`:

```bash
install -m 0600 /dev/null ~/.config/vornik/secrets/gateway.env
cat > ~/.config/vornik/secrets/gateway.env <<EOF
# vornik API gateway secrets — mode 0600.
VORNIK_GATEWAY_TOKEN=$(openssl rand -hex 32)
GOOGLE_MAPS_API_KEY=<your-google-maps-api-key>
EOF
```

- `VORNIK_GATEWAY_TOKEN` — the internal daemon↔gateway key-auth secret. On this
  deployment it is **already generated**; confirm it exists, do not regenerate
  (a mismatch causes `gateway authentication failed`).
- `GOOGLE_MAPS_API_KEY` — **replace the placeholder** with a real key. Until
  replaced, Google Maps calls return 4xx.

The compose file resolves this path via
`${VORNIK_CONFIG_DIR:-/opt/vornik/.config/vornik}/secrets/gateway.env`.
Set `VORNIK_CONFIG_DIR` if your config dir differs. (Compose does not expand a
literal `~`, hence the absolute default.)

## 2. Daemon config

Enable the gateway in the daemon config (`~/.config/vornik/config.yaml`; see the
documented block in `configs/vornik.yaml.example`):

```yaml
gateway:
  enabled: true
  address: http://127.0.0.1:8010
  # token_file: /home/you/.config/vornik/secrets/gateway.token   # or VORNIK_GATEWAY_TOKEN env
  providers:
    maps:
      base_path: /maps
      allowed_methods: ["GET", "HEAD"]
      writes_enabled: false
      description: "Google Maps API (geocoding, places, directions)."
```

The daemon reads the same `VORNIK_GATEWAY_TOKEN` from the secrets env-file, so
no separate `token`/`token_file` is required when the daemon sources
`gateway.env`. Set `token_file` (or the `VORNIK_GATEWAY_TOKEN` env) only if the
daemon does not source that file.

## 3. Bring up the gateway

```bash
podman compose -f deployments/podman/gateway.compose.yaml up -d
```

Loopback-only: the proxy is published on `127.0.0.1:8010` (→ container `:8000`),
reachable by the host daemon but not the network. The admin API is off.

## 4. Verify

```bash
vornikctl doctor
```

Expect `gateway_healthy OK` (the check is `SKIPPED` when `gateway.enabled` is
false / no address is configured). A quick manual probe — unauthenticated calls
must be rejected by the global key-auth plugin:

```bash
# 401 (no apikey) — key-auth is working:
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8010/maps/geocode/json
```

## Validating `kong.yml` changes

Parse the declarative config without starting the stack:

```bash
podman run --rm -e KONG_DATABASE=off -e GOOGLE_MAPS_API_KEY=x -e VORNIK_GATEWAY_TOKEN=x \
  -v "$PWD/deployments/podman/gateway/kong.yml:/kong/kong.yml:ro,Z" \
  docker.io/kong:3.7 kong config parse /kong/kong.yml
```
