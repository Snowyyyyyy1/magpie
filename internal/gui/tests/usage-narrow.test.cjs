// Run with Node's test runner and Playwright on the module path; see README.md.
// The Usage page in a narrow native window (mode=window, as the app opens it):
// the head takes the rows it needs instead of one 34px line, its controls stay
// packed at the left (no lone sum floating at the right of a row of its own),
// the agent chips and the chart's metrics wrap inside their own row rather
// than clipping at the edge, the six figures go three to a row with their
// sub-lines whole, the chart's dates stop colliding, and the page never
// scrolls sideways. Chromium and WebKit.
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");

const assets = process.env.MAGPIE_ASSETS || path.resolve(__dirname, "../assets");
const iso = (d) => [d.getFullYear(), d.getMonth() + 1, d.getDate()].map((n) => String(n).padStart(2, "0")).join("-");
const to = new Date();
const back = (i) => { const d = new Date(to); d.setDate(d.getDate() - i); return d; };
// as many agents as a toolbar holds, so the chip row is wider than the page
const AGENTS = [["claude", "Claude Code"], ["codex", "Codex"], ["grokbuild", "Grok Build"], ["hermes", "Hermes Agent"],
  ["deepseek", "DeepSeek Harness"], ["omp", "omp"], ["qoder", "Qoder"], ["pi", "Pi"], ["zcode", "ZCode"],
  ["opencode", "OpenCode"], ["workbuddy", "WorkBuddy"], ["cursor", "Cursor"], ["kiro", "Kiro"], ["devin", "Devin"],
  ["factory", "Factory"], ["qodercn", "Qoder CN"]];
const MODELS = ["claude-opus-4", "gpt-5", "deepseek-v4", "grok-4"];
const allDays = Array.from({ length: 200 }, (_, i) => back(199 - i)).map((d, i) => {
  const date = iso(d);
  if (i % 3 === 2) return { date, usage: [], active: [] };
  const n = 1 + (i % 7);
  const hours = new Array(24).fill(0);
  hours[9] = 600 * n; hours[14] = 300 * n; hours[22] = 60;
  const usage = [];
  for (let a = 0; a < 4; a++) {
    usage.push({ agent: AGENTS[(i + a) % AGENTS.length][0], cwd: a % 2 ? "/work/beta" : "/work/alpha", model: MODELS[a],
      input: 30000 * n, output: 4000 * n, cache_read: 90000 * n, cache_write: 1000, cost: 5 * n, priced: true });
  }
  return { date, usage, active: usage.slice(0, 2).map((r, k) => ({ agent: r.agent, cwd: r.cwd, seconds: k ? 120 : hours.reduce((x, y) => x + y, 0), hours })) };
});
const stats = (n) => {
  const days = allDays.slice(-n);
  return { from: days[0].date, to: iso(to), days, agents: Object.fromEntries(AGENTS) };
};
const top = (i) => ({ key: "claude:s" + i, agent: "claude", id: "s" + i, cwd: i ? "/work/beta" : "/work/alpha", title: "Top session " + i, last: to.toISOString(),
  input: 900000 - i * 1000, output: 5000, cache_read: 0, cache_write: 0, cost: 12 - i, priced: true, active: 3600 - i * 60, models: ["claude-opus-4"], days: [iso(to)] });
const full = (i) => ({ key: "claude:s" + i, agent: "claude", name: "Claude Code", icon: "claude", id: "s" + i, cwd: "/work/alpha", title: "Latest " + i,
  start: back(1).toISOString(), last: to.toISOString(), input: 900000, output: 5000, cache_read: 0, cache_write: 0, cost: 12, priced: true,
  models: [{ model: "claude-opus-4", input: 900000, output: 5000, cost: 12, priced: true }], resume: "claude --resume s" + i });
const latest = AGENTS.map(([agent, name], i) => ({ ...full(i), agent, name, key: agent + ":s" + i, id: agent + "-" + i, title: name + " session " + i }));

// the app's own window: bootPrefs.web false, mode window, the page's API faked
function serve(lang) {
  const state = { agents: [{ id: "claude", name: "Claude Code", path: "/test/settings.json", fields: [] }], profiles: [], settings: { lang, theme: "light" } };
  return async (route) => {
    const url = new URL(route.request().url());
    const json = (data) => route.fulfill({ json: data });
    const q = url.searchParams;
    if (url.pathname === "/boot.js") return route.fulfill({ contentType: "text/javascript", body: `window.bootPrefs = {lang:"${lang}",theme:"light",web:false};` });
    if (url.pathname === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window = {};" });
    if (url.pathname === "/api/state") return json(state);
    if (url.pathname === "/api/plugins") return json({ plugins: [] });
    if (url.pathname === "/api/groups") return json({ groups: [] });
    if (url.pathname === "/api/providers") return json({ providers: [], gateway: { running: true, window: true } });
    if (url.pathname === "/api/usage/quotas") return json([]);
    if (url.pathname === "/api/sessions/progress") return json({ indexing: false });
    if (url.pathname === "/api/sessions") return json({ sessions: latest, dirs: ["/test/sessions"] });
    if (url.pathname === "/api/sessions/stats") return json(stats(+q.get("days") || 200));
    if (url.pathname === "/api/sessions/overview") {
      const days = allDays.slice(-(+q.get("days") || 200));
      const tops = [0, 1, 2].map(top);
      return json({ count: 42, median: 120000, p90: 880000, days: days.map((d) => d.usage.length ? 4 : 0), top: { tokens: tops, cost: [...tops].reverse(), active: tops.slice(0, 2) },
        messages: days.map((d) => d.usage.length ? 30 : 0), output: days.map(() => 0),
        shape: { messages: { edges: [1, 6, 16, 31, 61, 121], counts: [3, 10, 14, 9, 4, 2], total: 42 }, minutes: { edges: [1, 6, 16, 31, 61, 121], counts: [20, 12, 6, 3, 1, 0], total: 42 }, autonomy: { edges: [0, 1, 3, 6, 11, 21], counts: [5, 7, 10, 12, 6, 2], total: 42 } },
        tools: { calls: 1000, sessions: 40, top: [["Bash", "Bash", 400, 38], ["Read", "Read", 250, 30], ["Edit", "Edit", 200, 25]].map(([name, category, calls, sessions]) => ({ name, category, calls, sessions })), categories: [["Bash", 400], ["Read", 250], ["Edit", 200]].map(([name, calls]) => ({ name, calls, sessions: 1 })), weeks: ["2026-08-31", "2026-09-07"].map((start, i) => ({ start, calls: { Bash: 100 * i + 10, Read: 60 } })) },
        skills: { calls: 30, count: 12, top: [{ name: "artifact-design", calls: 20, sessions: 8, last: "2026-09-27", agents: { claude: 15 }, projects: [{ name: "/work/alpha", calls: 18, sessions: 7 }] }] } });
    }
    if (url.pathname.startsWith("/api/")) return json({});
    const file = path.join(assets, url.pathname === "/" ? "index.html" : url.pathname);
    const contentType = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png" }[path.extname(file)];
    const body = await fs.readFile(file).catch(() => null);
    await (body ? route.fulfill({ body, contentType }) : route.fulfill({ status: 404, body: "" }));
  };
}

// Everything a width is asked about, in one reading of the page.
const look = () => {
  const view = document.querySelector("#view-usage");
  const head = view.querySelector(".usage-head");
  const vr = view.getBoundingClientRect();
  const box = (e) => { const b = e.getBoundingClientRect(); return { left: +b.left.toFixed(1), right: +b.right.toFixed(1), top: +b.top.toFixed(1), bottom: +b.bottom.toFixed(1) }; };
  const seg = (sel) => {
    const e = document.querySelector(sel);
    const over = e.scrollWidth > e.clientWidth + 1;
    const overflowX = getComputedStyle(e).overflowX;
    // an overflow the page keeps to itself is one the reader can scroll to
    return { ...box(e), clientW: e.clientWidth, scrollW: e.scrollWidth, overflowX, reachable: !over || overflowX === "auto" || overflowX === "scroll" };
  };
  const kids = [...head.children].filter((e) => e.getBoundingClientRect().width > 0).map((e) => ({ id: e.id || e.className, ...box(e) }));
  const rows = [];
  for (const k of [...kids].sort((a, b) => a.top - b.top || a.left - b.left)) {
    const row = rows.find((r) => k.top < r.bottom - 1 && k.bottom > r.top + 1);
    if (row) { row.items.push(k); row.bottom = Math.max(row.bottom, k.bottom); } else rows.push({ top: k.top, bottom: k.bottom, items: [k] });
  }
  for (const r of rows) r.items.sort((a, b) => a.left - b.left);
  // The refresh and the sum are one group, so a head row holds both or
  // neither; which row each is on is read off the rows themselves, and a
  // group whose two are on rows of their own is the orphan this guards.
  const rowOf = (sel) => {
    const e = document.querySelector(sel);
    if (!e) return -1;
    const b = e.getBoundingClientRect();
    return rows.findIndex((r) => b.top < r.bottom - 1 && b.bottom > r.top + 1);
  };
  // every strip's thumb on the option it is under: a strip that wraps (the
  // agent chips in a narrow window) must not carry one thumb over every row
  const thumbs = [...document.querySelectorAll(".segs")].filter((s) => s.offsetParent).map((s) => {
    const on = s.querySelector(":scope > .on"), th = s.querySelector(":scope > .thumb");
    if (!on || !th) return null;
    const ob = on.getBoundingClientRect(), tb = th.getBoundingClientRect();
    return { id: s.id || s.className, on: on.textContent,
      rows: new Set([...s.querySelectorAll(":scope > .opt")].map((b) => b.offsetTop)).size,
      dTop: +(tb.top - ob.top).toFixed(1), dLeft: +(tb.left - ob.left).toFixed(1),
      dH: +(tb.height - ob.height).toFixed(1), dW: +(tb.width - ob.width).toFixed(1) };
  }).filter(Boolean);
  // a chip out of the strip's sight is not one that is cut off: the strip
  // scrolls to it. What lies under a scroller is that scroller's business.
  const inScroller = (e) => {
    for (let p = e.parentElement; p && p !== view; p = p.parentElement) {
      const o = getComputedStyle(p);
      if (o.overflowX !== "visible" || o.overflowY !== "visible") return true;
    }
    return false;
  };
  const over = [...view.querySelectorAll("*")].filter((e) => e.offsetParent && !inScroller(e) && e.getBoundingClientRect().right > vr.right + 1)
    .map((e) => (typeof e.className === "string" && e.className ? e.className : e.tagName) + " @" + e.getBoundingClientRect().right.toFixed(0));
  const text = (e) => { const rg = document.createRange(); rg.selectNodeContents(e); const b = rg.getBoundingClientRect(); return { t: e.textContent, left: +b.left.toFixed(1), right: +b.right.toFixed(1) }; };
  const labels = [...document.querySelectorAll("#sessChart .labels span")].filter((s) => s.textContent && getComputedStyle(s).visibility === "visible")
    .map(text).sort((a, b) => a.left - b.left);
  const collide = [];
  for (let i = 1; i < labels.length; i++) if (labels[i].left < labels[i - 1].right + 2) collide.push(labels[i - 1].t + "/" + labels[i].t);
  const months = [...document.querySelectorAll("#sessChart .sess-cal .mo")].map(text).sort((a, b) => a.left - b.left);
  const monthsCollide = [];
  for (let i = 1; i < months.length; i++) if (months[i].left < months[i - 1].right + 2) monthsCollide.push(months[i - 1].t + "/" + months[i].t);
  return {
    window: document.body.classList.contains("window"),
    view: { clientW: view.clientWidth, scrollW: view.scrollWidth, ...box(view) },
    doc: { clientW: document.documentElement.clientWidth, scrollW: document.documentElement.scrollWidth },
    head: box(head), headRows: rows, over,
    refreshRow: rowOf("#usageRefresh"), costRow: rowOf("#usageCost"), costText: document.querySelector("#usageCost b")?.textContent || "",
    thumbs,
    agent: seg("#sessAgent"), metric: seg("#sessChart .sess-chart-head .segs"),
    kpis: [...document.querySelectorAll("#sessStats .kpi")].map((k) => {
      const s = k.querySelector("small");
      return { value: k.querySelector("b").textContent, sub: s ? s.textContent : "", clip: !!s && s.scrollWidth > s.clientWidth + 1 };
    }),
    labels, collide, months: months.length, monthsCollide,
  };
};

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  for (const lang of ["en", "zh"]) {
  test(engine + " " + lang + ": the Usage page keeps its head, chips and figures inside a narrow window", async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    const context = await browser.newContext({ viewport: { width: 950, height: 760 }, reducedMotion: "reduce", timezoneId: "Asia/Shanghai" });
    const page = await context.newPage();
    page.setDefaultTimeout(10000);
    const errors = [];
    page.on("pageerror", (e) => errors.push(e.message));
    t.after(() => browser.close());
    await page.route("**/*", serve(lang));
    await page.addInitScript(() => { localStorage.setItem("magpie.usageTab", "sessions"); localStorage.setItem("magpie.sessRange", "30d"); });
    await page.goto("http://magpie.test/?view=usage");
    await page.locator("#sessGrid:not([hidden])").waitFor();
    await page.waitForFunction(() => !document.querySelector("#sessStats .kpi.stale") && document.querySelectorAll("#sessStats .kpi").length === 6 &&
      document.querySelectorAll("#sessAgent .opt").length > 8 && document.querySelectorAll("#sessChart .labels span").length > 20);
    const settle = () => page.evaluate(() => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r))));

    for (const width of [950, 760, 560]) {
      await page.setViewportSize({ width, height: 760 });
      await settle();
      const m = await page.evaluate(look);
      assert.ok(m.window, "the fixture boots the app's own window");
      // the page, and the view in it, never scroll sideways
      assert.ok(m.view.scrollW <= m.view.clientW + 1, width + "px: the view scrolls sideways (" + m.view.scrollW + " > " + m.view.clientW + ")");
      assert.ok(m.doc.scrollW <= m.doc.clientW + 1, width + "px: the page scrolls sideways");
      assert.deepEqual(m.over, [], width + "px: something stands past the page's right edge");
      // the head: its controls in rows that start at the left and stay together,
      // with only the head's own gaps between them (10px, and the refresh's
      // 10px margin-right) — a lone control at the far right of its row fails
      assert.ok(m.headRows.length <= 2, width + "px: the head takes more than two rows: " + JSON.stringify(m.headRows.map((r) => r.items.map((i) => i.id))));
      for (const row of m.headRows) {
        const names = row.items.map((i) => i.id).join(",");
        assert.ok(row.items[0].left <= m.head.left + 1, width + "px: the head row starts away from the left edge (" + names + ")");
        for (let i = 1; i < row.items.length; i++) {
          const gap = row.items[i].left - row.items[i - 1].right;
          assert.ok(gap >= -0.5 && gap <= 24, width + "px: " + row.items[i].id + " is " + gap.toFixed(1) + "px from " + row.items[i - 1].id + " (" + names + ")");
        }
      }
      // the sum is long enough to matter (the real page's ≈$1486+), and it
      // keeps the refresh for company: a sum alone on a row, with the row
      // above ending in empty space, is the orphan this guards
      assert.ok(/\d{4,}/.test(m.costText), width + "px: the sum is not the long one the fixture asked for (" + m.costText + ")");
      assert.ok(m.refreshRow >= 0 && m.refreshRow === m.costRow, width + "px: the refresh and the sum are apart (rows " + m.refreshRow + " and " + m.costRow + "): " + JSON.stringify(m.headRows.map((r) => r.items.map((i) => i.id))));
      // every strip's thumb sits on the option it is under, whether the strip
      // is one row or (the agent chips here) several
      const offThumb = m.thumbs.filter((s) => Math.abs(s.dTop) > 1 || Math.abs(s.dH) > 1 || Math.abs(s.dLeft) > 1 || Math.abs(s.dW) > 1)
        .map((s) => s.id + "(" + s.on + ", " + s.rows + " rows): top " + s.dTop + " h " + s.dH + " left " + s.dLeft + " w " + s.dW);
      assert.deepEqual(offThumb, [], width + "px: a strip's thumb is not on its option");
      const wrapped = m.thumbs.find((s) => s.id === "sessAgent");
      assert.ok(wrapped && wrapped.rows > 1, width + "px: the agent chips are one row, so nothing wraps here");
      // the chips and the chart's metrics: inside the pane, scrollable to their end
      for (const [what, s] of [["the agent chips", m.agent], ["the chart's metrics", m.metric]]) {
        assert.ok(s.left >= m.view.left - 0.5 && s.right <= m.view.right + 0.5, width + "px: " + what + " stand outside the page: " + JSON.stringify(s));
        assert.ok(s.reachable, width + "px: " + what + " are cut off with no way to reach them");
        assert.ok(s.scrollW <= s.clientW + 1 || s.overflowX !== "visible", width + "px: " + what + " overflow in the open");
      }
      // six figures, their sub-lines whole
      assert.equal(m.kpis.length, 6, width + "px: six figures");
      const clipped = m.kpis.filter((k) => k.clip).map((k) => k.value + " / " + k.sub);
      assert.deepEqual(clipped, [], width + "px: a figure's sub-line is cut short");
      // the chart's dates do not run one into the next
      assert.ok(m.labels.length >= 4, width + "px: " + m.labels.length + " dates on the chart");
      assert.deepEqual(m.collide, [], width + "px: two dates collide");
      if (process.env.ARTIFACT_DIR) {
        await fs.mkdir(process.env.ARTIFACT_DIR, { recursive: true });
        await page.screenshot({ path: path.join(process.env.ARTIFACT_DIR, engine + "-" + width + "-usage-narrow.png") });
        await page.screenshot({ path: path.join(process.env.ARTIFACT_DIR, engine + "-" + width + "-usage-top.png"), clip: { x: 0, y: 0, width, height: 500 } });
      }
    }

    // Select options on every wrapped row, then widen and narrow while
    // keeping that choice: resizing must remove and restore its vertical box.
    await page.setViewportSize({ width: 560, height: 760 });
    const rowPicks = await page.locator("#sessAgent .opt").evaluateAll((opts) => {
      const seen = new Set();
      return opts.flatMap((o, i) => seen.has(o.offsetTop) ? [] : (seen.add(o.offsetTop), [i]));
    });
    assert(rowPicks.length > 1, "the interaction fixture has wrapped rows");
    for (const i of rowPicks) {
      await page.locator("#sessAgent .opt").nth(i).click();
      await settle();
      const check = await page.evaluate(look);
      const th = check.thumbs.find((s) => s.id === "sessAgent");
      assert(Math.abs(th.dTop) <= 1 && Math.abs(th.dH) <= 1 && Math.abs(th.dLeft) <= 1, "a clicked row is highlighted");
    }
    for (const width of [1600, 950, 560]) {
      await page.setViewportSize({ width, height: 760 });
      await settle();
      const th = (await page.evaluate(look)).thumbs.find((s) => s.id === "sessAgent");
      assert(Math.abs(th.dTop) <= 1 && Math.abs(th.dH) <= 1 && Math.abs(th.dLeft) <= 1, width + "px: resized highlight stays on its choice");
    }
    // Nothing changed in the API while Usage was hidden: a cached response
    // must not leave a highlight measured at the window's former width.
    await page.setViewportSize({ width: 560, height: 760 });
    await page.locator("#sessAgent .opt").filter({ hasText: /^Kiro$/ }).click();
    await page.evaluate(() => show("agents"));
    await page.setViewportSize({ width: 950, height: 760 });
    await settle();
    await page.evaluate(() => show("usage"));
    await settle();
    const hiddenResize = (await page.evaluate(look)).thumbs.find((s) => s.id === "sessAgent");
    assert.equal(hiddenResize.on, "Kiro");
    assert(Math.abs(hiddenResize.dTop) <= 1 && Math.abs(hiddenResize.dH) <= 1 && Math.abs(hiddenResize.dLeft) <= 1 && Math.abs(hiddenResize.dW) <= 1,
      "the cached page refits after resizing while hidden: " + JSON.stringify(hiddenResize));

    await page.locator("#sessAgent .opt").first().click();
    await settle();

    await page.mouse.move(400, 400);
    for (let i = 0; i < 12; i++) {
      const b = await page.locator("#sessRange .opt").last().boundingBox();
      if (b && b.y > 70 && b.y < 650) break;
      await page.mouse.wheel(0, -300);
      await page.waitForTimeout(50);
    }
    // the year's calendar: its months, too, stay inside and apart
    await page.locator("#sessRange .opt").last().click();
    await page.waitForFunction(() => document.querySelectorAll("#sessChart .sess-cal i").length > 100);
    for (const width of [950, 560]) {
      await page.setViewportSize({ width, height: 760 });
      await settle();
      const m = await page.evaluate(look);
      assert.ok(m.view.scrollW <= m.view.clientW + 1, width + "px: the calendar scrolls the view sideways");
      assert.ok(m.months >= 6, width + "px: " + m.months + " months named on the calendar");
      assert.deepEqual(m.monthsCollide, [], width + "px: two month names collide");
      if (process.env.ARTIFACT_DIR) await page.screenshot({ path: path.join(process.env.ARTIFACT_DIR, engine + "-" + width + "-usage-calendar.png"), fullPage: true });
    }
    assert.deepEqual(errors, []);
  });
}
}
