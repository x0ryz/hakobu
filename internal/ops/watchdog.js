// hakobu's watchdog: a Worker hakobu deploys to the owner's Cloudflare
// account, which asks the panel's /healthz every minute from outside the
// server and emails the owner when it stops answering, and when it's back.
// hakobu can't report that itself: the server, hakobu or the tunnel is down.
//
// Bindings: PANEL (https://<panel host>), FROM and TO (the addresses of
// hakobu's own emails), EMAIL (sends to TO, an address verified in Email
// Routing), STATE (KV: the outage being reported, so each is mailed once,
// then again every six hours while it lasts; written only on a change).

const TRIES = 3;
const PAUSE_MS = 15_000; // between tries: a restart of hakobu isn't an outage
const TIMEOUT_MS = 10_000;
const AGAIN_MS = 6 * 60 * 60 * 1000;

export default {
	async scheduled(event, env) {
		await check(env, Date.now());
	},
};

export async function check(env, now, sleep = (ms) => new Promise((r) => setTimeout(r, ms))) {
	let why = "";
	for (let i = 0; i < TRIES; i++) {
		why = await probe(env.PANEL);
		if (!why) break;
		if (i < TRIES - 1) await sleep(PAUSE_MS);
	}
	const down = await env.STATE.get("down", "json");
	if (!why) {
		if (down) {
			await env.STATE.delete("down");
			await mail(env, "The panel answers again",
				`${env.PANEL} answers again; it was unreachable from ${new Date(down.since).toUTCString()} for ${minutes(now - down.since)}.`);
		}
		return;
	}
	if (!down) {
		await env.STATE.put("down", JSON.stringify({ since: now, mailed: now }));
		await mail(env, "The panel is unreachable", unreachable(env, why));
	} else if (now - down.mailed >= AGAIN_MS) {
		await env.STATE.put("down", JSON.stringify({ since: down.since, mailed: now }));
		await mail(env, "The panel is still unreachable",
			`Unreachable for ${minutes(now - down.since)}. ` + unreachable(env, why));
	}
}

// probe returns why the panel isn't healthy, "" when it is.
export async function probe(panel) {
	try {
		const r = await fetch(panel + "/healthz", {
			redirect: "manual",
			signal: AbortSignal.timeout(TIMEOUT_MS),
			headers: { "user-agent": "hakobu-watchdog" },
		});
		const body = (await r.text()).trim();
		if (r.status === 200 && body === "ok") return "";
		return `it answered HTTP ${r.status}${hint(r.status)}`;
	} catch (e) {
		return e && e.name === "TimeoutError" ? `no answer within ${TIMEOUT_MS / 1000} s` : `no answer (${e})`;
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

function unreachable(env, why) {
	return `hakobu's watchdog on Cloudflare can't reach ${env.PANEL}: ${why}. ` +
		"Its apps are likely down too, and hakobu can't send its own emails until it's back. " +
		"Check that the server is running, then `systemctl status hakobu` on it.";
}

function minutes(ms) {
	const m = Math.round(ms / 60_000);
	return m < 60 ? `${m} min` : `${Math.floor(m / 60)} h ${m % 60} min`;
}

async function mail(env, subject, text) {
	text += "\n\n-- \nhakobu's watchdog, a Worker in your Cloudflare account. Turn it off in Settings: " +
		env.PANEL + "/settings#notifications\n";
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
