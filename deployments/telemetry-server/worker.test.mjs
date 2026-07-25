import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

import worker from "./worker.js";

const endpoint = "https://telemetry.vornik.io/v1/collect.json";

test("POST returns the canonical mock response with privacy headers", async () => {
  const canonical = JSON.parse(
    await readFile(new URL("./response.json", import.meta.url), "utf8"),
  );
  const response = await worker.fetch(
    new Request(`${endpoint}?e=install_succeeded&sv=1`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ schema_version: 1 }),
    }),
  );

  assert.equal(response.status, 202);
  assert.equal(response.headers.get("Cache-Control"), "no-store");
  assert.deepEqual(await response.json(), canonical);
});

test("rejects non-POST, non-JSON, and oversized requests", async () => {
  assert.equal((await worker.fetch(new Request(endpoint))).status, 405);
  assert.equal(
    (
      await worker.fetch(
        new Request(endpoint, {
          method: "POST",
          headers: { "Content-Type": "text/plain" },
          body: "{}",
        }),
      )
    ).status,
    415,
  );
  assert.equal(
    (
      await worker.fetch(
        new Request(endpoint, {
          method: "POST",
          headers: {
            "Content-Type": "application/json",
            "Content-Length": "4097",
          },
          body: "{}",
        }),
      )
    ).status,
    413,
  );
});

test("does not claim unrelated paths", async () => {
  const response = await worker.fetch(
    new Request("https://telemetry.vornik.io/not-telemetry", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: "{}",
    }),
  );
  assert.equal(response.status, 404);
});
