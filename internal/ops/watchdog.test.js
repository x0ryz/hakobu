// Tests of the watchdog Worker: `just test-js`.
import { test, expect } from "bun:test";
import { check } from "./watchdog.js";

function env(statuses) {
  const kv = new Map(), sent = [];
  let i = 0;
  globalThis.fetch = async () => {
    const s = statuses[Math.min(i++, statuses.length - 1)];
    if (s === "timeout") { const e = new Error("t"); e.name = "TimeoutError"; throw e; }
    return new Response(s === 200 ? "ok" : "bad", { status: s });
  };
  return {
    sent, kv,
    PANEL: "https://hakobu.example.com", FROM: "alerts@example.com", TO: "me@example.org",
    STATE: {
      async get(k, t) { const v = kv.get(k); return v == null ? null : (t === "json" ? JSON.parse(v) : v); },
      async put(k, v) { kv.set(k, v); }, async delete(k) { kv.delete(k); },
    },
    EMAIL: { async send(m) { sent.push(m.subject + " | " + m.text.split("\n")[0]); } },
  };
}
const nosleep = async () => {};

test("a blip that passes on retry isn't an outage", async () => {
  const e = env([502, 200]);
  await check(e, 0, nosleep);
  expect(e.sent).toEqual([]);
  expect(e.kv.size).toBe(0);
});

test("an outage is mailed once, again after 6h, and its end", async () => {
  const e = env([530]);
  await check(e, 0, nosleep);
  await check(e, 60_000, nosleep);
  await check(e, 6 * 3600_000, nosleep);
  expect(e.sent.length).toBe(2);
  expect(e.sent[0]).toContain("The panel is unreachable");
  expect(e.sent[0]).toContain("530: the tunnel isn't connected");
  expect(e.sent[1]).toContain("still unreachable");
  const ok = env([200]); ok.STATE = e.STATE; ok.EMAIL = e.EMAIL;
  await check(ok, 6 * 3600_000 + 125 * 60_000, nosleep);
  expect(e.sent[2]).toContain("answers again");
  expect(e.sent[2]).toContain("for 8 h 5 min");
  expect(e.kv.size).toBe(0);
});

test("a timeout reads as no answer", async () => {
  const e = env(["timeout"]);
  await check(e, 0, nosleep);
  expect(e.sent[0]).toContain("no answer within 10 s");
});
