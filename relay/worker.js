// Cloudflare OAuth relay for hakobu. The OAuth client can register only one
// redirect URL, so Cloudflare sends every login here; the hakobu installer
// that started it polls for its code by the random state it generated. The
// code is useless without the PKCE verifier only that installer holds, and
// it's kept for five minutes at most and handed out once.

const TTL = 300;

const page = (body) =>
  new Response(
    `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>hakobu</title><style>
body{font-family:system-ui,sans-serif;display:flex;min-height:100vh;align-items:center;justify-content:center;margin:0;padding:16px;color:#18181b}
main{max-width:420px;border:1px solid #e4e4e7;border-radius:12px;padding:24px}
</style></head><body><main>${body}</main></body></html>`,
    { headers: { "content-type": "text/html; charset=utf-8" } },
  );

const validState = (s) => typeof s === "string" && /^[A-Za-z0-9_-]{32,128}$/.test(s);

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    const state = url.searchParams.get("state");

    if (url.pathname === "/cf/callback") {
      if (!validState(state)) {
        return page("<p>This login link is invalid. Start again from the hakobu installer.</p>");
      }
      const result = url.searchParams.get("code")
        ? { code: url.searchParams.get("code") }
        : { error: url.searchParams.get("error_description") || url.searchParams.get("error") || "no code" };
      await env.CODES.put(state, JSON.stringify(result), { expirationTtl: TTL });
      return page(
        result.code
          ? "<h2>Cloudflare connected</h2><p>Go back to the terminal, hakobu continues there.</p>"
          : "<h2>Not connected</h2><p>Go back to the terminal and try again.</p>",
      );
    }

    if (url.pathname === "/cf/poll") {
      if (!validState(state)) return new Response("bad state", { status: 400 });
      const result = await env.CODES.get(state);
      if (result === null) return new Response("pending", { status: 404 });
      await env.CODES.delete(state);
      return new Response(result, { headers: { "content-type": "application/json" } });
    }

    return new Response("hakobu OAuth relay", { status: 404 });
  },
};
