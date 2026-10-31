"use strict";
// Student page: join a room, then follow the host through each question.
(() => {
  const { api, connect, el, renderBars, choiceColors, onThemeChange, voterId } = Pulse;
  const voter = voterId();

  let code = null; // room code once joined
  let conn = null; // live event stream
  let state = null; // latest snapshot
  let pending = null; // {index, choice} while a vote request is in flight
  let voteSeq = 0; // only the latest vote request updates the page
  let voteError = ""; // message from the last failed vote
  let builtFor = ""; // which question the answer buttons were built for

  const $ = (sel, root = document) => root.querySelector(sel);
  const screen = (name) => $(`[data-screen="${name}"]`);
  const field = (name, root) => $(`[data-field="${name}"]`, root);

  function show(name) {
    for (const s of document.querySelectorAll("[data-screen]")) s.hidden = s.dataset.screen !== name;
  }

  // --- Joining ---------------------------------------------------------

  const codeInput = $("#code");
  const joinButton = $("#join-button");
  const joinError = $("#join-error");

  codeInput.addEventListener("input", () => {
    codeInput.value = codeInput.value.toUpperCase().replace(/[^A-Z]/g, "").slice(0, 4);
    joinError.textContent = "";
  });
  $("#join-form").addEventListener("submit", (e) => {
    e.preventDefault();
    join(codeInput.value);
  });

  async function join(raw) {
    const c = raw.trim().toUpperCase();
    if (!/^[A-Z]{4}$/.test(c)) {
      showJoin("Room codes are 4 letters.");
      return;
    }
    joinButton.disabled = true;
    joinButton.textContent = "Joining…";
    try {
      const snap = await api("POST", `/api/rooms/${c}/join`, { voter });
      code = c;
      history.replaceState(null, "", `/?r=${c}`); // a reload rejoins the same room
      $("#room-footer .room-code").textContent = c;
      $("#room-footer").hidden = false;
      render(snap);
      conn = connect(`/api/rooms/${c}`, voter, {
        onSnapshot: render,
        onStatus: (live) => ($("#conn").hidden = live),
        onGone: () => leave("That room no longer exists."),
      });
    } catch (err) {
      codeInput.value = c;
      showJoin(
        err.status === 404
          ? `There's no room ${c}. Check the code on the screen.`
          : "Couldn't reach the server. Check your connection and try again.",
      );
    } finally {
      joinButton.disabled = false;
      joinButton.textContent = "Join";
    }
  }

  function showJoin(message) {
    joinError.textContent = message || "";
    show("join");
  }

  function leave(message) {
    if (conn) conn.close();
    conn = code = state = null;
    builtFor = "";
    $("#room-footer").hidden = true;
    $("#conn").hidden = true;
    history.replaceState(null, "", "/");
    showJoin(message);
  }

  // --- Rendering -------------------------------------------------------

  function render(s) {
    state = s;
    if (pending && (s.phase !== "voting" || s.index !== pending.index)) pending = null;
    if (s.phase !== "voting") voteError = "";
    switch (s.phase) {
      case "lobby":
        renderLobby(s);
        break;
      case "voting":
        renderVoting(s);
        break;
      case "results":
        renderResults(s);
        break;
      case "done":
        show("done");
        break;
    }
  }

  function renderLobby(s) {
    const root = screen("lobby");
    field("title", root).textContent = s.title;
    const n = s.participants;
    field("participants", root).textContent = `${n} ${n === 1 ? "person has" : "people have"} joined.`;
    show("lobby");
  }

  function renderVoting(s) {
    const root = screen("voting");
    const choicesBox = $(".choices", root);
    const key = `${s.code}:${s.index}:${s.choices.join("|")}`;
    if (builtFor !== key) {
      builtFor = key;
      voteError = "";
      field("progress", root).textContent = `Question ${s.index + 1} of ${s.total}`;
      field("question", root).textContent = s.question.text;
      const detail = field("detail", root);
      detail.textContent = s.question.detail || "";
      detail.hidden = !s.question.detail;
      const colors = choiceColors(s.choices.length);
      choicesBox.replaceChildren(
        ...s.choices.map((label, i) => {
          const swatch = el("span", { class: "swatch", "aria-hidden": "true" });
          swatch.style.background = colors[i];
          const button = el(
            "button",
            { type: "button", class: "choice", "aria-pressed": "false" },
            swatch,
            el("span", { class: "label" }, label),
            el("span", { class: "check", "aria-hidden": "true" }),
          );
          button.addEventListener("click", () => vote(i));
          return button;
        }),
      );
    }
    const selected = pending ? pending.choice : s.myVote;
    choicesBox.querySelectorAll(".choice").forEach((b, i) => b.setAttribute("aria-pressed", String(i === selected)));

    const status = $(".vote-status", root);
    status.classList.toggle("error", Boolean(voteError));
    status.textContent = voteError
      ? voteError
      : pending
        ? "Sending…"
        : s.myVote != null
          ? "Vote saved. You can change it until voting closes."
          : "Tap an answer. You can change it until voting closes.";
    show("voting");
  }

  function renderResults(s) {
    const root = screen("results");
    field("progress", root).textContent = `Question ${s.index + 1} of ${s.total} · Results`;
    field("question", root).textContent = s.question.text;
    renderBars($(".bars", root), s.choices, s.counts, { mine: s.myVote });
    const votes = `${s.votesCast} ${s.votesCast === 1 ? "vote" : "votes"}`;
    field("note", root).textContent = s.myVote == null ? `${votes}. You didn't answer this one.` : `${votes}.`;
    show("results");
  }

  // --- Voting ----------------------------------------------------------

  async function vote(choice) {
    if (!state || state.phase !== "voting") return;
    const index = state.index;
    const seq = ++voteSeq;
    pending = { index, choice };
    voteError = "";
    render(state);
    try {
      const snap = await api("POST", `/api/rooms/${code}/vote`, { voter, question: index, choice });
      if (seq !== voteSeq) return; // a newer tap is in flight
      pending = null;
      render(snap);
    } catch (err) {
      if (seq !== voteSeq) return;
      pending = null;
      voteError =
        err.status === 409
          ? "Voting on this question has closed."
          : err.status === 0
            ? "Couldn't send your vote. Check your connection and tap again."
            : `Couldn't save your vote: ${err.message}`;
      if (state) render(state);
    }
  }

  // Redraw colors if the phone switches between light and dark mode.
  onThemeChange(() => {
    builtFor = "";
    if (state) render(state);
  });

  // --- Start -----------------------------------------------------------

  const fromUrl = new URLSearchParams(location.search).get("r");
  if (fromUrl) {
    codeInput.value = fromUrl.toUpperCase().slice(0, 4);
    join(fromUrl);
  } else {
    show("join");
    codeInput.focus();
  }
})();
