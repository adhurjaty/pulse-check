"use strict";
// Shared helpers for the student and host pages.
const Pulse = (() => {
  // Diverging scale for ordered answers: blue for the first half, red for
  // the second, strongest at the ends, gray in the middle for odd counts.
  // Each arm is listed from the outer end inward and was checked with the
  // data-viz validator (--ordinal) against the light and dark surfaces.
  // In dark mode the outer ends are the brighter steps.
  const PALETTES = {
    light: {
      blue: [["#256abf"], ["#256abf", "#86b6ef"], ["#1c5cab", "#3987e5", "#86b6ef"]],
      red: [["#c23a39"], ["#c23a39", "#f0908e"], ["#a82c2c", "#e05150", "#f0908e"]],
      mid: "#898781",
    },
    dark: {
      blue: [["#5598e7"], ["#5598e7", "#1c5cab"], ["#6da7ec", "#2a78d6", "#1c5cab"]],
      red: [["#e66767"], ["#e66767", "#a82c2c"], ["#ec8584", "#d24745", "#a82c2c"]],
      mid: "#898781",
    },
  };
  const darkQuery = matchMedia("(prefers-color-scheme: dark)");

  // isDark follows <html data-theme> if a page set it, else the OS setting.
  function isDark() {
    const theme = document.documentElement.dataset.theme;
    return theme ? theme === "dark" : darkQuery.matches;
  }

  function choiceColors(n) {
    const p = isDark() ? PALETTES.dark : PALETTES.light;
    const arm = Math.floor(n / 2);
    if (arm < 1 || arm > 3) return Array(n).fill(p.mid);
    const colors = [...p.blue[arm - 1]];
    if (n % 2) colors.push(p.mid);
    colors.push(...[...p.red[arm - 1]].reverse());
    return colors;
  }

  function onThemeChange(fn) {
    darkQuery.addEventListener("change", fn);
  }

  // el("div", {class: "x"}, child, "text", ...)
  function el(tag, attrs = {}, ...children) {
    const node = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs)) node.setAttribute(k, v);
    node.append(...children);
    return node;
  }

  // renderBars draws one horizontal bar per choice. Bar length is the share
  // of all votes. Every row is labeled with its choice, count and percent,
  // so color is never the only cue. opts.mine marks the viewer's answer.
  function renderBars(container, choices, counts, opts = {}) {
    const total = counts.reduce((a, b) => a + b, 0);
    const colors = choiceColors(choices.length);
    container.replaceChildren(
      ...choices.map((label, i) => {
        const share = total ? counts[i] / total : 0;
        const name = el("span", { class: "bar-label" }, label);
        if (opts.mine === i) name.append(el("span", { class: "tag" }, "Your answer"));
        const fill = el("div", { class: "bar-fill" });
        fill.style.width = `${share * 100}%`;
        fill.style.background = colors[i];
        return el(
          "div",
          { class: "bar-row" },
          el("div", { class: "bar-head" }, name, el("span", { class: "bar-value" }, `${counts[i]} · ${Math.round(share * 100)}%`)),
          el("div", { class: "bar-track" }, fill),
        );
      }),
    );
  }

  // textOn picks dark or white text for a label inside a colored fill.
  function textOn(hex) {
    const [r, g, b] = [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16) / 255);
    const lin = (c) => (c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4);
    const L = 0.2126 * lin(r) + 0.7152 * lin(g) + 0.0722 * lin(b);
    return L > 0.3 ? "#0b0b0b" : "#ffffff";
  }

  // legend lists the choices with their colors (used with stacked bars,
  // where segments are too small to name).
  function legend(choices) {
    const colors = choiceColors(choices.length);
    return el(
      "div",
      { class: "legend" },
      ...choices.map((c, i) => {
        const swatch = el("i", { "aria-hidden": "true" });
        swatch.style.background = colors[i];
        return el("span", {}, swatch, c);
      }),
    );
  }

  // stackedBar draws one 100% bar split by choice. Segments of 7% or more
  // show their percent; every segment has a tooltip with the full numbers.
  function stackedBar(choices, counts) {
    const total = counts.reduce((a, b) => a + b, 0);
    if (!total) return el("div", { class: "stack-empty" }, "No votes");
    const colors = choiceColors(choices.length);
    const bar = el("div", { class: "stack", role: "img" });
    const parts = [];
    choices.forEach((c, i) => {
      if (!counts[i]) return;
      const pct = Math.round((100 * counts[i]) / total);
      parts.push(`${c}: ${counts[i]} (${pct}%)`);
      const seg = el("div", { title: `${c}: ${counts[i]} of ${total} (${pct}%)` }, pct >= 7 ? `${pct}%` : "");
      seg.style.flex = `${counts[i]} 0 0`;
      seg.style.background = colors[i];
      seg.style.color = textOn(colors[i]);
      bar.append(seg);
    });
    bar.setAttribute("aria-label", parts.join(", "));
    return bar;
  }

  // api sends a JSON request. On failure it throws an Error with .status
  // (0 for network errors) and .data (the parsed error body, if any).
  async function api(method, path, body, headers = {}) {
    let res;
    try {
      res = await fetch(path, {
        method,
        headers: { "Content-Type": "application/json", ...headers },
        body: body === undefined ? undefined : typeof body === "string" ? body : JSON.stringify(body),
        cache: "no-store",
      });
    } catch (err) {
      throw Object.assign(new Error("Couldn't reach the server"), { status: 0 });
    }
    const data = res.status === 204 ? null : await res.json().catch(() => null);
    if (!res.ok) {
      throw Object.assign(new Error((data && data.error) || res.statusText), { status: res.status, data });
    }
    return data;
  }

  // voterId returns this browser's anonymous ID, creating it on first use.
  // It uses getRandomValues rather than randomUUID, which is unavailable on
  // plain-HTTP pages (such as testing over a LAN IP).
  function voterId() {
    const key = "pulse.voterId";
    try {
      const existing = localStorage.getItem(key);
      if (existing && /^[A-Za-z0-9_-]{1,64}$/.test(existing)) return existing;
    } catch {}
    const bytes = crypto.getRandomValues(new Uint8Array(16));
    const id = Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
    try {
      localStorage.setItem(key, id);
    } catch {}
    return id;
  }

  // connect keeps a live event stream open for a room and calls
  // onSnapshot with every snapshot. It recovers from:
  //  - dropped connections (EventSource retries by itself),
  //  - a failed reconnect, such as a server restart (retried every 3 s),
  //  - silently dead connections after a phone sleeps or switches networks
  //    (no message or heartbeat for 40 s, or the page becoming visible
  //    after a long time hidden).
  // onStatus(true/false) reports whether the stream is live. onGone is
  // called if the room no longer exists.
  function connect(roomPath, voter, { onSnapshot, onStatus = () => {}, onGone = () => {} }) {
    const query = voter ? `?voter=${encodeURIComponent(voter)}` : "";
    const STALE_MS = 40000;
    let es = null;
    let closed = false;
    let retryTimer = null;
    let lastHeard = Date.now();

    function open() {
      if (es) es.close();
      lastHeard = Date.now();
      es = new EventSource(`${roomPath}/events${query}`);
      es.onmessage = (e) => {
        lastHeard = Date.now();
        onStatus(true);
        onSnapshot(JSON.parse(e.data));
      };
      es.addEventListener("ping", () => {
        lastHeard = Date.now();
        onStatus(true);
      });
      es.onerror = () => {
        onStatus(false);
        if (es.readyState === EventSource.CLOSED) scheduleRetry(3000);
      };
    }

    function scheduleRetry(ms) {
      if (closed || retryTimer) return;
      retryTimer = setTimeout(reconnect, ms);
    }

    async function reconnect() {
      clearTimeout(retryTimer);
      retryTimer = null;
      if (closed) return;
      try {
        const res = await fetch(`${roomPath}${query}`, { cache: "no-store" });
        if (res.status === 404) {
          close();
          onGone();
          return;
        }
      } catch {
        // Offline: open() anyway; EventSource keeps retrying until it's back.
      }
      if (!closed) open();
    }

    function close() {
      closed = true;
      clearTimeout(retryTimer);
      clearInterval(watchdog);
      document.removeEventListener("visibilitychange", onVisible);
      if (es) es.close();
    }

    function onVisible() {
      if (document.hidden || closed) return;
      if (es.readyState === EventSource.CLOSED || Date.now() - lastHeard > STALE_MS) {
        onStatus(false);
        reconnect();
      }
    }

    const watchdog = setInterval(() => {
      if (!document.hidden && Date.now() - lastHeard > STALE_MS) {
        onStatus(false);
        reconnect();
      }
    }, 5000);
    document.addEventListener("visibilitychange", onVisible);
    open();
    return { close };
  }

  return { api, connect, el, renderBars, stackedBar, legend, choiceColors, isDark, onThemeChange, voterId };
})();
