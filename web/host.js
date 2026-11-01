"use strict";
// Host page: create or resume a room, then drive it from the projector.
(() => {
  const { api, connect, el, renderBars, stackedBar, legend, onThemeChange } = Pulse;

  const PIN_KEY = "pulse.hostPin";
  const ROOM_KEY = "pulse.hostRoom";
  const THEME_KEY = "pulse.hostTheme";

  const store = {
    get: (k) => { try { return localStorage.getItem(k) || ""; } catch { return ""; } },
    set: (k, v) => { try { v ? localStorage.setItem(k, v) : localStorage.removeItem(k); } catch {} },
  };

  let pin = store.get(PIN_KEY);
  let code = null;
  let conn = null;
  let state = null;
  let busy = false; // a control request is in flight; extra presses are ignored

  const $ = (sel, root = document) => root.querySelector(sel);
  const $$ = (sel, root = document) => root.querySelectorAll(sel);
  const field = (name, root) => $(`[data-field="${name}"]`, root);
  const hostApi = (method, path, body) => api(method, path, body, { "X-Host-Pin": pin });

  function showScreen(name) {
    for (const s of $$("[data-screen]")) s.hidden = s.dataset.screen !== name;
  }
  function showView(name) {
    for (const v of $$("[data-view]")) v.hidden = v.dataset.view !== name;
  }

  // --- Theme (light by default: projectors wash out dark pages) ----------

  function applyTheme(theme) {
    document.documentElement.dataset.theme = theme;
    store.set(THEME_KEY, theme);
    if (state) render(state);
  }
  applyTheme(store.get(THEME_KEY) || "light");
  onThemeChange(() => state && render(state));

  // --- PIN ---------------------------------------------------------------

  $("#pin-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    pin = $("#pin").value;
    try {
      await hostApi("POST", "/api/host/check");
      store.set(PIN_KEY, pin);
      $("#pin-error").textContent = "";
      showSetup();
    } catch (err) {
      $("#pin-error").textContent = err.status === 401 ? "That PIN is wrong." : err.message;
    }
  });

  $("#forget-pin").addEventListener("click", () => {
    pin = "";
    store.set(PIN_KEY, "");
    $("#pin").value = "";
    showScreen("pin");
  });

  // --- Create or resume --------------------------------------------------

  function showSetup(message = "") {
    $("#resume-code").value = store.get(ROOM_KEY);
    $("#resume-error").textContent = message;
    showScreen("setup");
  }

  const fileInput = $("#file");
  fileInput.addEventListener("change", () => {
    $("#create").disabled = !fileInput.files.length;
    $("#create-error").textContent = "";
    $("#problems").hidden = true;
  });

  $("#create").addEventListener("click", async () => {
    const file = fileInput.files[0];
    if (!file) return;
    const problems = $("#problems");
    problems.hidden = true;
    $("#create-error").textContent = "";
    $("#create").disabled = true;
    try {
      const snap = await hostApi("POST", "/api/rooms", await file.text());
      openRoom(snap.code, snap);
    } catch (err) {
      if (err.status === 401) return showScreen("pin");
      $("#create-error").textContent =
        err.status === 422 ? `${file.name} has problems:` : `Couldn't create the room: ${err.message}`;
      if (err.data && err.data.problems) {
        problems.replaceChildren(...err.data.problems.map((p) => el("li", {}, p)));
        problems.hidden = false;
      }
    } finally {
      $("#create").disabled = !fileInput.files.length;
    }
  });

  $("#resume-code").addEventListener("input", (e) => {
    e.target.value = e.target.value.toUpperCase().replace(/[^A-Z]/g, "").slice(0, 4);
  });
  $("#resume-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const c = $("#resume-code").value;
    try {
      openRoom(c, await api("GET", `/api/rooms/${c}`));
    } catch (err) {
      $("#resume-error").textContent = err.status === 404 ? `There's no room ${c}.` : err.message;
    }
  });

  // --- The room ----------------------------------------------------------

  function openRoom(c, snap) {
    code = c;
    store.set(ROOM_KEY, c);
    history.replaceState(null, "", `/host?r=${c}`); // a reload reopens this room
    for (const n of $$(".room-code")) n.textContent = c;
    $(".join-host").textContent = location.host;
    $(".join-url").textContent = location.host;
    $(".qr").src = `/api/rooms/${c}/qr.svg`;
    showScreen("room");
    render(snap);
    if (conn) conn.close();
    conn = connect(`/api/rooms/${c}`, "", {
      onSnapshot: render,
      onStatus: (live) => ($("#conn").hidden = live),
      onGone: () => leaveRoom(`Room ${c} no longer exists.`),
    });
  }

  function leaveRoom(message = "") {
    if (conn) conn.close();
    conn = code = state = null;
    $("#conn").hidden = true;
    if (document.fullscreenElement) document.exitFullscreen();
    history.replaceState(null, "", "/host");
    showSetup(message);
  }

  function render(s) {
    state = s;
    const n = s.participants;
    $(".participants").textContent = `${n} joined`;
    switch (s.phase) {
      case "lobby": {
        const root = $('[data-view="lobby"]');
        field("title", root).textContent = s.title;
        $(".joined", root).textContent = n === 0 ? "Waiting for people to join…" : `${n} ${n === 1 ? "person has" : "people have"} joined`;
        break;
      }
      case "voting": {
        const root = $('[data-view="voting"]');
        field("progress", root).textContent = `Question ${s.index + 1} of ${s.total}`;
        field("question", root).textContent = s.question.text;
        const detail = field("detail", root);
        detail.textContent = s.question.detail || "";
        detail.hidden = !s.question.detail;
        field("voted", root).textContent = `${s.votesCast} of ${n} voted`;
        $(".meter > div", root).style.width = `${n ? Math.min(100, (100 * s.votesCast) / n) : 0}%`;
        break;
      }
      case "results": {
        const root = $('[data-view="results"]');
        field("progress", root).textContent = `Question ${s.index + 1} of ${s.total} · Results`;
        field("question", root).textContent = s.question.text;
        renderBars($(".bars", root), s.choices, s.counts);
        field("note", root).textContent = `${s.votesCast} ${s.votesCast === 1 ? "vote" : "votes"}`;
        break;
      }
      case "done": {
        const root = $('[data-view="done"]');
        field("title", root).textContent = s.title;
        $(".legend-slot", root).replaceChildren(legend(s.choices));
        $(".summary", root).replaceChildren(
          ...s.summary.map((q, i) => {
            const total = q.counts.reduce((a, b) => a + b, 0);
            return el(
              "div",
              { class: "summary-row" },
              el("div", { class: "summary-q", title: q.text }, el("b", {}, `${i + 1}.`), q.text),
              stackedBar(s.choices, q.counts),
              el("div", { class: "summary-n" }, `${total} ${total === 1 ? "vote" : "votes"}`),
            );
          }),
        );
        break;
      }
    }
    showView(s.phase);
    renderControls(s);
  }

  // Button labels say exactly what the next press will do.
  function renderControls(s) {
    const last = s.index === s.total - 1;
    const advance = { lobby: "Start", voting: "Show results", results: last ? "Finish" : "Next question", done: "" }[s.phase];
    const back = {
      lobby: "",
      voting: s.index === 0 ? "Back to lobby" : "Previous results",
      results: "Reopen voting",
      done: "Back to last question",
    }[s.phase];
    $("#advance").textContent = advance;
    $("#advance").hidden = !advance;
    $("#back").textContent = back;
    $("#back").hidden = !back;
    $("#csv").hidden = s.phase !== "done";
  }

  async function control(action) {
    if (busy || !state || !code) return;
    if (action === "advance" && state.phase === "done") return;
    if (action === "back" && state.phase === "lobby") return;
    busy = true;
    $("#control-error").textContent = "";
    try {
      render(await hostApi("POST", `/api/rooms/${code}/control`, { action, phase: state.phase, index: state.index }));
    } catch (err) {
      // 409 means the room already moved (another tab, or a double press);
      // the event stream brings the current state.
      if (err.status === 401) $("#control-error").textContent = "The host PIN was rejected. Leave and sign in again.";
      else if (err.status !== 409) $("#control-error").textContent = `Couldn't ${action === "back" ? "go back" : "advance"}: ${err.message}`;
    } finally {
      busy = false;
    }
  }

  for (const [id, action] of [["#advance", "advance"], ["#back", "back"]]) {
    $(id).addEventListener("click", (e) => {
      e.currentTarget.blur(); // so Space/Enter don't also re-press the focused button
      control(action);
    });
  }

  $("#csv").addEventListener("click", async () => {
    // Fetched with the PIN header (never in the URL), then saved as a file.
    try {
      const res = await fetch(`/api/rooms/${code}/results.csv`, { headers: { "X-Host-Pin": pin } });
      if (!res.ok) throw new Error(res.statusText);
      const url = URL.createObjectURL(await res.blob());
      const a = el("a", { href: url, download: `pulse-check-${code}.csv` });
      document.body.append(a);
      a.click();
      a.remove();
      setTimeout(() => URL.revokeObjectURL(url), 1000);
    } catch (err) {
      $("#control-error").textContent = `Couldn't download the CSV: ${err.message}`;
    }
  });

  const toggleTheme = () => applyTheme(document.documentElement.dataset.theme === "dark" ? "light" : "dark");
  const toggleFullscreen = () =>
    document.fullscreenElement ? document.exitFullscreen() : document.documentElement.requestFullscreen().catch(() => {});
  $("#theme").addEventListener("click", (e) => { e.currentTarget.blur(); toggleTheme(); });
  $("#exit").addEventListener("click", () => leaveRoom());

  // Keyboard and presentation clickers (which send PageDown/PageUp or arrows).
  document.addEventListener("keydown", (e) => {
    if (!code || e.metaKey || e.ctrlKey || e.altKey) return;
    if (e.target.closest("input, textarea, select")) return;
    const key = e.key;
    if (["ArrowRight", "PageDown", " ", "Enter"].includes(key)) control("advance");
    else if (["ArrowLeft", "PageUp"].includes(key)) control("back");
    else if (key === "f" || key === "F") toggleFullscreen();
    else if (key === "t" || key === "T") toggleTheme();
    else return;
    e.preventDefault();
  });

  // --- Start -------------------------------------------------------------

  (async () => {
    if (!pin) return showScreen("pin");
    try {
      await hostApi("POST", "/api/host/check");
    } catch (err) {
      if (err.status === 401) {
        store.set(PIN_KEY, "");
        return showScreen("pin");
      }
    }
    const fromUrl = new URLSearchParams(location.search).get("r");
    if (fromUrl) {
      try {
        return openRoom(fromUrl.toUpperCase(), await api("GET", `/api/rooms/${fromUrl.toUpperCase()}`));
      } catch {
        return showSetup(`There's no room ${fromUrl.toUpperCase()}.`);
      }
    }
    showSetup();
  })();
})();
