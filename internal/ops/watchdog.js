// hakobu's watchdog: a Worker hakobu deploys to the owner's Cloudflare
// account. Every minute it asks the panel and each app from outside the
// server, the way their visitors reach them, and emails the owner when one
// stops answering and when it's back. hakobu can't see that itself: the
// server, hakobu or the tunnel may be down, or an app's address broken
// while it runs fine inside.
//
// Bindings: TARGETS (JSON: [{name, url, need}], the panel first; need is
// "ok" for the panel's /healthz, "2xx", or "any" for any answer but a
// server error), FROM and TO (the addresses of hakobu's own emails), EMAIL
// (sends to TO, an address verified in Email Routing), STATE (KV: the
// outages being reported, written only on a change), and DB (D1, if the
// token allows: one row per target and minute, kept 30 days, which the
// panel shows as uptime).

const TRIES = 3;
const PAUSE_MS = 15_000; // between tries: a restart isn't an outage
const TIMEOUT_MS = 10_000;
const AGAIN_MS = 6 * 60 * 60 * 1000;
const KEEP_MS = 30 * 24 * 60 * 60 * 1000;

export default {
	async scheduled(event, env) {
		await check(env, Date.now());
	},
};

export async function check(env, now, sleep = (ms) => new Promise((r) => setTimeout(r, ms))) {
	const targets = JSON.parse(env.TARGETS);
	const results = await Promise.all(targets.map((t) => probeTries(t, sleep)));
	if (env.DB) await record(env.DB, targets, results, now);

	let state = (await env.STATE.get("down", "json")) || {};
	if ("since" in state) state = { panel: state }; // as v0.9 kept the panel's
	const before = JSON.stringify(state);
	const panelDown = !!results[0].why;
	for (const [i, t] of targets.entries()) {
		const { why } = results[i];
		const down = state[t.name];
		if (!why) {
			if (down) {
				delete state[t.name];
				await mail(env, `${label(t)} answers again`,
					`${t.url} answers again; it was unreachable from ${new Date(down.since).toUTCString()} for ${minutes(now - down.since)}.`);
			}
		} else if (i > 0 && panelDown) {
			// The panel's email covers it: the server, hakobu or the tunnel.
		} else if (!down) {
			state[t.name] = { since: now, mailed: now };
			await mail(env, `${label(t)} is unreachable`, unreachable(t, why, i === 0));
		} else if (now - down.mailed >= AGAIN_MS) {
			state[t.name] = { since: down.since, mailed: now };
			await mail(env, `${label(t)} is still unreachable`,
				`Unreachable for ${minutes(now - down.since)}. ` + unreachable(t, why, i === 0));
		}
	}
	if (JSON.stringify(state) !== before) {
		if (Object.keys(state).length) await env.STATE.put("down", JSON.stringify(state));
		else await env.STATE.delete("down");
	}
}

function label(t) {
	return t.name === "panel" ? "The panel" : t.name.replace(/^app:/, "");
}

// probeTries asks a target up to TRIES times while it fails.
async function probeTries(t, sleep) {
	let res;
	for (let i = 0; i < TRIES; i++) {
		res = await probe(t);
		if (!res.why) break;
		if (i < TRIES - 1) await sleep(PAUSE_MS);
	}
	return res;
}

// probe returns how long the target took, its status and why it isn't
// healthy, "" when it is.
export async function probe(t) {
	const start = Date.now();
	try {
		const r = await fetch(t.url, {
			redirect: "manual",
			signal: AbortSignal.timeout(TIMEOUT_MS),
			// A trace marked not sampled keeps the checks out of the app's traces.
			headers: { "user-agent": "hakobu-watchdog", "sentry-trace": `${hex(16)}-${hex(8)}-0` },
		});
		const body = t.need === "ok" ? (await r.text()).trim() : (await r.body?.cancel(), "");
		const ms = Date.now() - start;
		const healthy =
			t.need === "ok" ? r.status === 200 && body === "ok" :
			t.need === "2xx" ? r.status >= 200 && r.status < 300 :
			r.status < 500;
		return { ms, status: r.status, why: healthy ? "" : `it answered HTTP ${r.status}${hint(r.status)}` };
	} catch (e) {
		const why = e && e.name === "TimeoutError" ? `no answer within ${TIMEOUT_MS / 1000} s` : `no answer (${e})`;
		return { ms: Date.now() - start, status: 0, why };
	}
}

function hex(bytes) {
	return [...crypto.getRandomValues(new Uint8Array(bytes))].map((b) => b.toString(16).padStart(2, "0")).join("");
}

async function record(db, targets, results, now) {
	const ts = Math.floor(now / 60_000) * 60;
	const insert = db.prepare("INSERT OR REPLACE INTO checks (target, ts, up, ms, status) VALUES (?, ?, ?, ?, ?)");
	const statements = targets.map((t, i) => insert.bind(t.name, ts, results[i].why ? 0 : 1, results[i].ms, results[i].status));
	if (new Date(now).getUTCMinutes() === 0) {
		statements.push(db.prepare("DELETE FROM checks WHERE ts < ?").bind(Math.floor((now - KEEP_MS) / 1000)));
	}
	try {
		await db.batch(statements);
	} catch (e) {
		console.log("recording the checks failed:", e);
	}
}

function hint(status) {
	switch (status) {
		case 502:
			return ": the tunnel is up but can't reach hakobu, which isn't running";
		case 530:
			return ": the tunnel isn't connected, so the server or its cloudflared is down";
		default:
			return "";
	}
}

function unreachable(t, why, isPanel) {
	if (isPanel) {
		return `hakobu's watchdog on Cloudflare can't reach ${t.url.replace(/\/healthz$/, "")}: ${why}. ` +
			"Its apps are likely down too, and hakobu can't send its own emails until it's back. " +
			"Check that the server is running, then `systemctl status hakobu` on it.";
	}
	return `hakobu's watchdog on Cloudflare can't reach ${t.url}: ${why}, while the panel answers. ` +
		"If hakobu shows the app running, its address is what's broken: its DNS record, the tunnel's route, " +
		"or a Cloudflare rule in front of it.";
}

function minutes(ms) {
	const m = Math.round(ms / 60_000);
	return m < 60 ? `${m} min` : `${Math.floor(m / 60)} h ${m % 60} min`;
}

async function mail(env, subject, text) {
	const panel = JSON.parse(env.TARGETS)[0].url.replace(/\/healthz$/, "");
	text += "\n\n-- \nhakobu's watchdog, a Worker in your Cloudflare account. Turn it off in Settings: " +
		panel + "/settings#notifications\n";
	try {
		await env.EMAIL.send({ from: { email: env.FROM, name: "Hakobu" }, to: env.TO, subject, text });
	} catch (e) {
		// Bindings without Email Sending take a raw message.
		const { EmailMessage } = await import("cloudflare:email");
		const raw = [
			`From: Hakobu <${env.FROM}>`,
			`To: ${env.TO}`,
			`Subject: ${subject}`,
			`Message-ID: <${crypto.randomUUID()}@${env.FROM.split("@")[1]}>`,
			`Date: ${new Date().toUTCString()}`,
			"MIME-Version: 1.0",
			"Content-Type: text/plain; charset=utf-8",
			"",
			text,
		].join("\r\n");
		await env.EMAIL.send(new EmailMessage(env.FROM, env.TO, raw));
	}
}
