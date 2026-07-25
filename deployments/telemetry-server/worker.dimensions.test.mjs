import assert from "node:assert/strict";
import test from "node:test";

import worker from "./worker.js";

const endpoint = "https://telemetry.vornik.io/v1/collect.json";

// Collects writeDataPoint calls the way the Analytics Engine binding would.
function fakeEnv() {
  const written = [];
  return { written, TELEMETRY: { writeDataPoint: (p) => written.push(p) } };
}

function post(query, init = {}) {
  return new Request(`${endpoint}?${query}`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ schema_version: 1 }),
    ...init,
  });
}

const install = "e=install_succeeded&sv=1&v=2026.7.4&os=linux&arch=amd64&source=quickstart";
const project =
  "e=project_created&sv=1&v=2026.7.4&os=darwin&arch=arm64&source=cli_template&tpl=personal-assistant&auto=1";

test("records an aggregate datapoint from the allowlisted query dimensions", async () => {
  const env = fakeEnv();
  const response = await worker.fetch(post(project), env);

  assert.equal(response.status, 202);
  assert.equal(env.written.length, 1);
  const [point] = env.written;
  assert.deepEqual(point.blobs, [
    "project_created",
    "2026.7.4",
    "darwin",
    "arm64",
    "cli_template",
    "personal-assistant",
  ]);
  assert.deepEqual(point.doubles, [1]);
  assert.deepEqual(point.indexes, ["project_created"]);
});

test("install events record an empty template and zero autonomy", async () => {
  const env = fakeEnv();
  await worker.fetch(post(install), env);
  const [point] = env.written;
  assert.deepEqual(point.blobs, [
    "install_succeeded",
    "2026.7.4",
    "linux",
    "amd64",
    "quickstart",
    "",
  ]);
  assert.deepEqual(point.doubles, [0]);
});

// A datapoint's dimensions are billed and grouped, so an attacker sending
// unbounded values must not be able to blow up cardinality. Every dimension is
// bucketed against a closed set before it is written.
test("buckets out-of-enum dimensions instead of trusting the caller", async () => {
  const env = fakeEnv();
  await worker.fetch(
    post(
      "e=install_succeeded&sv=1&v=" +
        encodeURIComponent("2026.7.4-112-g29df3bdb") +
        "&os=plan9&arch=mips64&source=made_up&tpl=" +
        encodeURIComponent("../../etc/passwd") +
        "&auto=maybe",
    ),
    env,
  );
  const [point] = env.written;
  assert.deepEqual(point.blobs, [
    "install_succeeded",
    "other", // unbounded build stamp must not become a dimension value
    "other",
    "other",
    "other",
    "custom",
  ]);
  assert.deepEqual(point.doubles, [0]);
});

test("an unknown event name is refused rather than recorded", async () => {
  const env = fakeEnv();
  const response = await worker.fetch(post("e=something_else&sv=1"), env);
  assert.equal(response.status, 400);
  assert.equal(env.written.length, 0);
});

// The size guard streams the body to count bytes when Content-Length is absent,
// so "unconsumed" is not the invariant. The invariant is that the body is never
// PARSED: nothing from it may reach a datapoint, a log line, or the response.
test("never lets body content reach a dimension, a log line, or the response", async () => {
  const env = fakeEnv();
  const lines = [];
  const original = console.log;
  console.log = (...args) => lines.push(args.join(" "));
  let response;
  try {
    response = await worker.fetch(
      new Request(`${endpoint}?${project}`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ schema_version: 1, secret: "SENTINEL-do-not-record" }),
      }),
      env,
    );
  } finally {
    console.log = original;
  }

  const surfaces = JSON.stringify(env.written) + lines.join("") + (await response.text());
  assert.ok(!surfaces.includes("SENTINEL"), "body content must never be recorded or echoed");
  assert.ok(!surfaces.includes("secret"), "body keys must never be recorded either");
});

// The binding is absent on a free-tier or preview deploy. Emission must degrade
// to accept-and-log rather than 500 on every install in the field.
test("works when the Analytics Engine binding is absent", async () => {
  const response = await worker.fetch(post(project), {});
  assert.equal(response.status, 202);
  const undefEnv = await worker.fetch(post(project));
  assert.equal(undefEnv.status, 202);
});

// Workers Logs is the zero-cost surface; a structured line makes the dimensions
// greppable there without enabling request-body logging or Logpush.
test("emits one structured log line carrying only allowlisted dimensions", async () => {
  const lines = [];
  const original = console.log;
  console.log = (...args) => lines.push(args.join(" "));
  try {
    await worker.fetch(post(project), fakeEnv());
  } finally {
    console.log = original;
  }

  assert.equal(lines.length, 1);
  const parsed = JSON.parse(lines[0]);
  assert.deepEqual(parsed, {
    msg: "collect",
    event: "project_created",
    sv: 1,
    v: "2026.7.4",
    os: "darwin",
    arch: "arm64",
    source: "cli_template",
    tpl: "personal-assistant",
    auto: 1,
  });
  // No IP, no headers, no body: the privacy contract forbids retaining them.
  const raw = lines[0];
  for (const forbidden of ["cf-connecting-ip", "ip", "user-agent", "schema_version"]) {
    assert.ok(!raw.toLowerCase().includes(forbidden), `log must not carry ${forbidden}`);
  }
});
