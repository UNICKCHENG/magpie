// Settings About: fingerprint updates default on and retain an explicit opt-out.
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");

const assets = path.resolve(__dirname, "../assets");
const defaultVersions = Object.fromEntries(Object.entries(require("../../fingerprint/defaults.json")).map(([client, profile]) => [client, profile.version]));

const words = {
  en: { auto: "Automatic updates", every: "Check every", off: "Off", on: "On", m30: "30 min", h6: "6 h", h24: "24 h",
    version: "Version", check: "Check", isOff: "Automatic updates are off", latest: "Up to date", whileOn: "While automatic updates are on", often: "How often magpie looks for a newer version" },
  zh: { auto: "自动更新", every: "检查间隔", off: "关闭", on: "开启", m30: "30 分钟", h6: "6 小时", h24: "24 小时",
    version: "版本", check: "检查", isOff: "自动更新已关闭", latest: "已是最新", whileOn: "自动更新开启时生效", often: "magpie 多久检查一次新版本" },
};

function settingsPayload(lang, ctl) {
  const s = {
    theme: "light", lang, tray: "panel", quotaLeft: false, currency: "usd",
    dock: false, dockWindow: false, proxy: "", redact: false, redactPersonal: false, redactWords: [],
    codexWarmup: "", claudeWarmup: "", codexWarmAt: "", claudeWarmAt: "", workbuddyCheckin: false, noStats: false,
    trayUsage: "", trayUsageEvery: 3, vision: "", imageGen: "",
    version: "0.1.400", dir: "~/.config/magpie", gateway: "http://127.0.0.1:3425",
    proxyNow: "none", proxySource: "none", login: false,
    visionModels: [], imageGenModels: [], workbuddyCheckins: [], lanURLs: [],
    fx: { rate: 7.2, at: new Date().toISOString(), stale: false },
  };
  // as the backend sends them: left out while they are their defaults
  s.fingerprintUpdates = ctl.fingerprint !== false;
  s.fingerprintUpdate = {versions:defaultVersions, ...ctl.fingerprintStatus};
  if (ctl.off) s.noAutoUpdate = true;
  if (ctl.every) s.updateEvery = ctl.every;
  return s;
}

function server(lang, ctl) {
  return async (route) => {
    const req = route.request(), url = new URL(req.url());
    const json = (data) => route.fulfill({ json: data });
    if (url.pathname === "/boot.js") return route.fulfill({ contentType: "text/javascript", body: `window.bootPrefs = {lang:"${lang}",theme:"light",web:false};` });
    if (url.pathname === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window = {};" });
    if (url.pathname === "/api/state") return json({ agents: [], profiles: [], settings: settingsPayload(lang, ctl), fx: settingsPayload(lang, ctl).fx });
    if (url.pathname === "/api/settings") {
      if (req.method() === "POST") {
        const body = req.postDataJSON();
        ctl.fingerprint = !!body.fingerprintUpdates;
        ctl.off = !!body.noAutoUpdate;
        ctl.every = body.updateEvery === 360 ? 0 : body.updateEvery;
        ctl.saves.push(body);
      }
      return json(settingsPayload(lang, ctl));
    }
    if (url.pathname === "/api/update/check") {
      ctl.checks++;
      ctl.state = "latest";
      return json({ state: "latest", current: "0.1.400", latest: "0.1.400" });
    }
    if (url.pathname === "/api/update") return json({ state: ctl.state, current: "0.1.400" });
    if (url.pathname === "/api/plugins") return json({ plugins: [] });
    if (url.pathname === "/api/usage/quotas") return json([]);
    if (url.pathname === "/api/groups") return json({ groups: [], models: [] });
    if (url.pathname.startsWith("/api/")) return json({});
    const file = path.join(assets, url.pathname === "/" ? "index.html" : url.pathname);
    const contentType = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png" }[path.extname(file)];
    await route.fulfill({ body: await fs.readFile(file), contentType });
  };
}

// wheel a row into sight, as a user would; a click must then not move the page
async function showRow(page, row) {
  await page.locator("#view-settings").hover();
  for (let i = 0; i < 40; i++) {
    const box = await row.boundingBox();
    const view = await page.locator("#view-settings").boundingBox();
    if (box && view && box.y >= view.y + 4 && box.y + box.height <= view.y + view.height - 4) break;
    await page.mouse.wheel(0, 180);
    await page.waitForTimeout(16);
  }
  await page.waitForTimeout(250);
}
const scrolls = (page) => page.evaluate(() => [window.scrollY, document.scrollingElement.scrollTop, ...[...document.querySelectorAll(".view")].map((v) => v.scrollTop)].join(","));

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  test(engine + ": fingerprint updates default on, retain opt-out and keep status current", async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({channel:"chromium"}));
    t.after(() => browser.close());
    for (const lang of ["en", "zh"]) await t.test(lang, async () => {
      const ctl = {off:false, every:0, state:"", saves:[], checks:0};
      const context = await browser.newContext({viewport:{width:1100,height:800},reducedMotion:"reduce"});
      t.after(() => context.close());
      const page = await context.newPage(), errors=[];
      page.on("pageerror", e=>errors.push(e.message));
      page.setDefaultTimeout(10000);
      await page.route("**/*",server(lang,ctl));
      await page.goto("http://magpie.test/?view=settings&tab=about");
      const row=page.locator(".fingerprint-update-row"), status=page.locator(".fingerprint-status-row");
      await row.waitFor({state:"visible"});
      assert.equal((await row.locator(".opt.on").innerText()).trim(),words[lang].on);
      const expectDefaults = async () => {
        const versions = await status.locator(".val").innerText();
        assert.ok(versions.includes(`Codex CLI ${defaultVersions.codex}`));
        assert.ok(versions.includes(`Claude Code ${defaultVersions["claude-code"]}`));
      };
      await expectDefaults();
      assert.equal(ctl.saves.length,0,"reading Settings must not save preferences");
      await showRow(page,row);
      let before=await scrolls(page);
      await row.locator(".opt",{hasText:words[lang].off}).click();
      await page.waitForFunction(()=>/^(Off|关闭)$/.test(document.querySelector(".fingerprint-update-row .opt.on")?.textContent.trim()));
      assert.equal(ctl.saves.at(-1).fingerprintUpdates,false);
      assert.equal(await scrolls(page),before,"opting out must not scroll");
      await page.reload();
      await row.waitFor({state:"visible"});
      assert.equal((await row.locator(".opt.on").innerText()).trim(),words[lang].off,"reload must preserve opt-out");
      await expectDefaults();
      await showRow(page,row);before=await scrolls(page);
      await row.locator(".opt",{hasText:words[lang].on}).click();
      await page.waitForFunction(()=>document.querySelector(".fingerprint-update-row .opt.on")?.dataset.val === "on" || /^(On|开启)$/.test(document.querySelector(".fingerprint-update-row .opt.on")?.textContent.trim()));
      assert.equal(ctl.saves.at(-1).fingerprintUpdates,true);
      assert.equal(ctl.saves.at(-1).noAutoUpdate,false);
      assert.equal(await scrolls(page),before,"enabling must not scroll");
      // A different setting's save must carry the fingerprint choice.
      const auto=page.locator(".update-auto-row");
      await showRow(page,auto);
      await auto.locator(".opt",{hasText:words[lang].off}).click();
      await page.waitForFunction(()=>/^(Off|关闭)$/.test(document.querySelector(".update-auto-row .opt.on")?.textContent.trim()));
      assert.equal(ctl.saves.at(-1).fingerprintUpdates,true);
      ctl.fingerprintStatus={running:true};
      await status.getByText(lang === "zh" ? /正在下载并验证/ : /Downloading and checking/).waitFor();
      await expectDefaults();
      ctl.fingerprintStatus={running:false,checked:"2026-10-11T01:00:00Z",updated:"2026-10-11T01:00:00Z",versions:{codex:"0.999.0","claude-code":"2.1.999"}};
      await status.getByText(/Codex CLI 0\.999\.0/).waitFor();
      assert.match(await status.locator(".val").innerText(),/Claude Code 2\.1\.999/);
      ctl.fingerprintStatus.error="capture rejected";
      await status.getByText(/capture rejected/).waitFor();
      assert.match(await status.innerText(),/0\.999\.0/);
      await page.reload();
      await row.waitFor({state:"visible"});
      assert.equal((await row.locator(".opt.on").innerText()).trim(),words[lang].on);
      ctl.fingerprintStatus.running=true;
      await status.getByText(lang === "zh" ? /正在下载并验证/ : /Downloading and checking/).waitFor();
      await showRow(page,row);before=await scrolls(page);
      await row.locator(".opt",{hasText:words[lang].off}).click();
      await page.waitForFunction(()=>/^(Off|关闭)$/.test(document.querySelector(".fingerprint-update-row .opt.on")?.textContent.trim()));
      assert.equal(ctl.saves.at(-1).fingerprintUpdates,false);
      assert.equal(await scrolls(page),before,"disabling must not scroll");
      await status.getByText(lang === "zh" ? /正在停止指纹更新/ : /Stopping the fingerprint update/).waitFor();
      ctl.fingerprintStatus.running=false;
      await status.getByText(lang === "zh" ? /正在停止指纹更新/ : /Stopping the fingerprint update/).waitFor({state:"hidden"});
      assert.match(await status.innerText(),/0\.999\.0/,"off keeps the saved fingerprints visible");
      assert.deepEqual(errors,[]);
      await context.close();
    });
  });
}
