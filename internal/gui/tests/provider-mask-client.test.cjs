// Run with Node's test runner and Playwright on the module path; see README.md.
// A custom provider's editor offers the client its requests can wear, for a
// relay that serves only that client's own: the row follows the Base URL's
// API — Codex over Responses, Claude Code over Anthropic, none over
// the others — starts off, and the Save sends which one the ticked row
// names. In English and Chinese, Chromium and WebKit.
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");

const assets = path.resolve(__dirname, "../assets");
const U = "https://relay.example.com";

function serve(lang, posts) {
  const state = { agents: [], profiles: [], settings: { lang, theme: "light" } };
  const models = [{ id: "m", name: "m", on: true }];
  const base = { icon: "generic", anthropic: "", models, agents: [], fallback: [], headers: {}, key: { set: true, masked: "sk-…1234" }, keyList: [], ready: true };
  const providers = { providers: [
    { ...base, id: "cc", name: "CC Relay", anthropic: U, baseAPI: "anthropic", maskClient: "claude-code" },
    { ...base, id: "chat", name: "Plain", chat: U + "/v1", baseAPI: "chat" },
    { ...base, id: "responses", name: "Responses Relay", responses: U + "/v1", baseAPI: "responses" },
  ], presets: [], excluded: [], gateway: { running: true, window: true } };
  return async (route) => {
    const url = new URL(route.request().url());
    const json = (data) => route.fulfill({ json: data });
    if (url.pathname === "/boot.js") return route.fulfill({ contentType: "text/javascript", body: `window.bootPrefs = {lang:${JSON.stringify(lang)},theme:"light",web:true};` });
    if (url.pathname === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window = {};" });
    if (url.pathname === "/api/state") return json(state);
    if (url.pathname === "/api/providers") return json(providers);
    if (url.pathname === "/api/groups") return json({ groups: [] });
    if (url.pathname.startsWith("/api/provider/")) {
      posts.push({ path: url.pathname, body: route.request().postDataJSON() });
      return json(providers);
    }
    if (url.pathname.startsWith("/api/")) return json({});
    const file = path.join(assets, url.pathname === "/" ? "index.html" : url.pathname);
    await route.fulfill({ body: await fs.readFile(file), contentType: { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png" }[path.extname(file)] });
  };
}

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  for (const lang of ["en", "zh"]) {
    test(`${engine} ${lang}: the client row follows the Base URL's API`, async (t) => {
      const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: process.env.PLAYWRIGHT_CHANNEL || "chromium" }));
      const page = await (await browser.newContext({ viewport: { width: 900, height: 760 }, reducedMotion: "reduce" })).newPage();
      t.after(() => browser.close());
      page.setDefaultTimeout(5000);
      const errors = [], posts = [];
      page.on("pageerror", (e) => errors.push(e.message));
      await page.route("**/*", serve(lang, posts));
      await page.goto("http://magpie.test/?view=providers");
      const zh = lang === "zh";
      const L = {
        codex: zh ? "Codex CLI 指纹" : "Codex CLI fingerprint",
        cc: zh ? "Claude Code 指纹" : "Claude Code fingerprint",
        save: zh ? "保存" : "Save",
      };
      const ed = page.locator(".editor");
      const open = async (id) => {
        if (await ed.count()) {
          await page.keyboard.press("Escape");
          // an editor with unsaved changes asks first: discard them
          const dlg = page.locator("dialog.action-confirm");
          if (await dlg.count()) await dlg.locator("button.primary").click();
          await ed.waitFor({ state: "detached" });
        }
        posts.length = 0;
        await page.locator(`#providers .row[data-id="${id}"]`).click();
        await ed.locator(".base-url").waitFor();
      };
      const save = async () => {
        await ed.locator(":scope > .bar").getByRole("button", { name: L.save, exact: true }).click();
        for (let i = 0; i < 60 && !posts.some((p) => p.path === "/api/provider/save"); i++) await page.waitForTimeout(50);
        return posts.find((p) => p.path === "/api/provider/save");
      };
      const tick = () => ed.locator("input.mask-client");
      const tickText = () => ed.locator("label.tick:has(input.mask-client) span").textContent();

      // saved as Anthropic with the mask: the row is there, ticked, Claude Code's
      await open("cc");
      assert.equal(await tick().isChecked(), true, "saved mask is on");
      assert.equal(await tickText(), L.cc, "the tick names Claude Code");

      // the API switched: the row follows it, the tick stays as it was
      await ed.locator('.segs .opt[data-api="responses"]').click();
      assert.equal(await tickText(), L.codex, "the tick names Codex");
      await ed.locator(".base-url").fill(U + "/v1");
      const saved = await save();
      assert(saved, "saved");
      assert.equal(saved.body.maskClient, "codex", "the save sends the client the row names now");
      assert.equal(saved.body.responses, U + "/v1");

      // unticked: none
      await open("cc");
      await tick().uncheck();
      const cleared = await save();
      assert.equal(cleared.body.maskClient, "", "unticked sends none");

      // Responses starts off; saving an explicit opt-in sends Codex.
      await open("responses");
      assert.equal(await tick().isVisible(), true);
      assert.equal(await tick().isChecked(), false);
      await tick().check();
      assert.equal((await save()).body.maskClient, "codex");

      // Chat has no client row. Switching through it clears the opt-in.
      await open("chat");
      assert.equal(await tick().isVisible(), false);
      assert.equal((await save()).body.maskClient, "");
      await open("cc");
      await ed.locator('.segs .opt[data-api="openai"]').click();
      assert.equal(await tick().isVisible(), false);
      assert.equal(await page.evaluate(() => asTyped().maskClient), "");
      await ed.locator('.segs .opt[data-api="responses"]').click();
      assert.equal(await tick().isChecked(), false);
      await ed.locator('.segs .opt[data-api="gemini"]').click();
      await ed.locator(".base-url").fill(U + "/v1beta");
      assert.equal(await tick().isVisible(), false);
      assert.equal((await save()).body.maskClient, "");
      assert.deepEqual(errors, []);
    });
  }
}
