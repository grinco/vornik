const MAX_BODY_BYTES = 4096;

const RESPONSE_BODY = Object.freeze({
  accepted: true,
  body_stored: false,
  message: "Request accepted. The mock endpoint does not store or process the JSON body.",
});

const RESPONSE_HEADERS = Object.freeze({
  "Cache-Control": "no-store",
  "Content-Type": "application/json; charset=utf-8",
  "Referrer-Policy": "no-referrer",
  "X-Content-Type-Options": "nosniff",
});

// Closed dimension vocabularies. Every value written to a log line or an
// Analytics Engine datapoint is bucketed against these first: dimensions are
// grouped and billed, so a caller sending unbounded values must not be able to
// inflate cardinality, and an unexpected value must never become a new
// dimension. Mirrors the client's own normalizers in
// internal/telemetryclient/client.go — keep the two in step.
const EVENTS = new Set(["install_succeeded", "project_created"]);
const OSES = new Set(["linux", "darwin", "windows", "freebsd", "other"]);
const ARCHES = new Set(["amd64", "arm64", "386", "arm", "other"]);
const SOURCES = new Set([
  "quickstart",
  "macos_quickstart",
  "cli_basic",
  "cli_template",
  "api_template",
]);
// Bounded release identifiers only, plus the client's two sentinels. A
// git-describe stamp (2026.7.4-112-g29df3bdb) is effectively unique, so it is
// bucketed to "other" rather than retained.
const RELEASE = /^[0-9]{4}\.[0-9]{1,2}\.[0-9]{1,3}(-(?:alpha|beta|rc)\.?[0-9]{1,3})?$/;
const TEMPLATE = /^[a-z0-9-]{1,64}$/;

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    if (url.pathname !== "/v1/collect.json") {
      return jsonResponse({ accepted: false, error: "not_found" }, 404);
    }
    if (request.method !== "POST") {
      return jsonResponse(
        { accepted: false, error: "method_not_allowed" },
        405,
        { Allow: "POST" },
      );
    }
    if (!isJSON(request.headers.get("Content-Type"))) {
      return jsonResponse(
        { accepted: false, error: "content_type_must_be_application_json" },
        415,
      );
    }
    if (!(await bodyFits(request, MAX_BODY_BYTES))) {
      return jsonResponse({ accepted: false, error: "body_too_large" }, 413);
    }

    const dims = readDimensions(url.searchParams);
    if (dims === null) {
      return jsonResponse({ accepted: false, error: "unknown_event" }, 400);
    }

    // Aggregate-only recording, derived entirely from the allowlisted query
    // string. The body is still never parsed, stored, or forwarded — the URL
    // dimensions exist precisely so the edge never has to read it.
    //
    // Nothing here touches the client IP, headers, or cookies: the datapoint
    // and the log line carry only the bucketed dimensions above, which keeps
    // the IP/retention contract in the design's privacy section intact.
    record(env, dims);

    return jsonResponse(RESPONSE_BODY, 202);
  },
};

// readDimensions buckets the query string into the closed dimension set, or
// returns null when the event itself is not one we accept.
function readDimensions(params) {
  const event = params.get("e") || "";
  if (!EVENTS.has(event)) return null;

  const version = params.get("v") || "";
  const template = params.get("tpl");
  return {
    event,
    sv: bucketSchemaVersion(params.get("sv")),
    v: RELEASE.test(version) || version === "dev" || version === "unknown"
      ? version
      : "other",
    os: bucketMember(params.get("os"), OSES),
    arch: bucketMember(params.get("arch"), ARCHES),
    source: bucketMember(params.get("source"), SOURCES),
    // Absent means "event carries no project properties" (an install), which
    // is distinct from "present but not a catalog slug" (custom template).
    tpl: template === null ? "" : TEMPLATE.test(template) ? template : "custom",
    auto: params.get("auto") === "1" ? 1 : 0,
  };
}

function record(env, dims) {
  // Workers Logs: one structured line, greppable in the dashboard and via
  // `wrangler tail`, with no request-body logging and no Logpush needed.
  console.log(JSON.stringify({ msg: "collect", ...dims }));

  // Analytics Engine is optional so a free-tier or preview deploy still
  // accepts events instead of failing every install in the field.
  const dataset = env && env.TELEMETRY;
  if (!dataset || typeof dataset.writeDataPoint !== "function") return;
  dataset.writeDataPoint({
    blobs: [dims.event, dims.v, dims.os, dims.arch, dims.source, dims.tpl],
    doubles: [dims.auto],
    indexes: [dims.event],
  });
}

function bucketMember(value, allowed) {
  return allowed.has(value || "") ? value : "other";
}

function bucketSchemaVersion(value) {
  const parsed = Number.parseInt(value || "", 10);
  return Number.isInteger(parsed) && parsed > 0 && parsed < 1000 ? parsed : 0;
}

function isJSON(contentType) {
  return (contentType || "").split(";", 1)[0].trim().toLowerCase() === "application/json";
}

async function bodyFits(request, maxBytes) {
  const declared = request.headers.get("Content-Length");
  if (declared !== null) {
    const bytes = Number(declared);
    return Number.isInteger(bytes) && bytes >= 0 && bytes <= maxBytes;
  }
  if (!request.body) return true;

  const reader = request.body.getReader();
  let bytes = 0;
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) return true;
      bytes += value.byteLength;
      if (bytes > maxBytes) {
        await reader.cancel("body too large");
        return false;
      }
    }
  } finally {
    reader.releaseLock();
  }
}

function jsonResponse(value, status, extraHeaders = {}) {
  return new Response(`${JSON.stringify(value)}\n`, {
    status,
    headers: { ...RESPONSE_HEADERS, ...extraHeaders },
  });
}
