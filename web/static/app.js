// Drydock frontend — dependency-free vanilla JS (ES module).
//
// Views: auth (login/register tabs) -> app (topbar, kanban board, card drawer).
// Live data comes from two WebSockets:
//   GET /api/stream        board event stream (card created/updated/progress/...) with reconnect backoff
//   GET /api/cards/:id/chat per-card chat socket (history on open, then live frames)
// Typing guard: the drawer detail is never re-rendered while the user has focus
// in a drawer input/textarea; progress + incoming messages apply as DOM-only updates.

const $ = (id) => document.getElementById(id);

const COLUMNS = [
  { key: "inbox", label: "Inbox" },
  { key: "working", label: "Working" },
  { key: "waiting", label: "Waiting" },
  { key: "needs_you", label: "Needs You" },
  { key: "qa", label: "QA" },
  { key: "done", label: "Done" },
];

const state = {
  user: null,
  view: "board",           // "board" | "admin"
  boards: [],
  boardId: null,
  cards: new Map(),        // id -> CardSummary
  openCardId: null,
  detail: null,            // CardDetail for openCardId
  stream: null,            // board event WebSocket
  streamWanted: false,     // reconnect loop runs while true
  streamBackoff: 1000,     // reconnect backoff ms, doubles up to 30s
  chat: null,              // per-card chat WebSocket
  chatCardId: null,
  drawerTyping: false,     // true while focus is in a drawer input/textarea
  drawerStale: false,      // stream event arrived for open card while typing; refetch on blur
};

/* ---------------- helpers ---------------- */

function toast(msg) {
  const el = $("toast");
  el.textContent = msg;
  el.classList.remove("hidden");
  clearTimeout(toast._t);
  toast._t = setTimeout(() => el.classList.add("hidden"), 4000);
}

function showError(elId, msg) {
  const el = $(elId);
  el.textContent = msg;
  el.classList.remove("hidden");
}

function hideError(elId) {
  $(elId).classList.add("hidden");
}

function timeAgo(ts) {
  if (!ts) return "—";
  const s = Math.max(0, (Date.now() - new Date(ts).getTime()) / 1000);
  if (s < 60) return "just now";
  if (s < 3600) return Math.floor(s / 60) + "m ago";
  if (s < 86400) return Math.floor(s / 3600) + "h ago";
  return Math.floor(s / 86400) + "d ago";
}

function wsUrl(path) {
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  return proto + "//" + location.host + path;
}

function shortContainer(id) {
  return id && id.length > 12 ? id.slice(0, 12) : (id || "");
}

/* ---------------- API client ----------------
   Same-origin JSON. Any 401 drops back to the auth view. */
async function api(path, opts = {}) {
  const res = await fetch(path, {
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    ...opts,
  });
  if (res.status === 401) {
    showAuth(); // session expired or never established
    throw new Error("unauthorized");
  }
  if (!res.ok) {
    let msg = res.statusText;
    try {
      const j = await res.json();
      msg = j.error || j.message || msg;
    } catch { /* non-JSON error body */ }
    throw new Error(msg);
  }
  const text = await res.text();
  return text ? JSON.parse(text) : null;
}

/* ---------------- auth ---------------- */

function showAuth() {
  state.user = null;
  state.view = "board";
  state.streamWanted = false;
  closeStream();
  closeChat();
  closeDrawer();
  $("app-view").classList.add("hidden");
  $("auth-view").classList.remove("hidden");
  // Reset to the board view for the next login.
  $("board-view").classList.remove("hidden");
  $("admin-view").classList.add("hidden");
  $("nav-board").classList.add("active");
  $("nav-admin").classList.remove("active");
  $("nav-admin").classList.add("hidden");
  for (const id of ["board-select", "new-board-btn", "new-card-btn"]) {
    $(id).classList.remove("hidden");
  }
  hideError("auth-error");
}

function setAuthTab(which) {
  const login = which === "login";
  $("tab-login").classList.toggle("active", login);
  $("tab-register").classList.toggle("active", !login);
  $("login-form").classList.toggle("hidden", !login);
  $("register-form").classList.toggle("hidden", login);
  hideError("auth-error");
}

async function submitAuth(path, username, password) {
  hideError("auth-error");
  try {
    state.user = await api(path, {
      method: "POST",
      body: JSON.stringify({ username, password }),
    });
    showApp();
  } catch (e) {
    if (e.message !== "unauthorized") showError("auth-error", e.message);
  }
}

/* ---------------- app shell ---------------- */

async function showApp() {
  $("auth-view").classList.add("hidden");
  $("app-view").classList.remove("hidden");
  $("user-label").textContent = state.user.username || "";
  // Admin view is only reachable for admins (the API 403s otherwise).
  $("nav-admin").classList.toggle("hidden", !state.user.is_admin);
  await loadBoards();
  connectStream();
}

/* ---------------- board / admin view switch ---------------- */

function showView(which) {
  state.view = which;
  const admin = which === "admin";
  $("board-view").classList.toggle("hidden", admin);
  $("admin-view").classList.toggle("hidden", !admin);
  $("nav-board").classList.toggle("active", !admin);
  $("nav-admin").classList.toggle("active", admin);
  // Board controls don't apply to the admin view.
  for (const id of ["board-select", "new-board-btn", "new-card-btn"]) {
    $(id).classList.toggle("hidden", admin);
  }
  if (admin) loadAdmin();
}

async function loadBoards() {
  try {
    state.boards = (await api("/api/boards")) || [];
  } catch (e) {
    if (e.message !== "unauthorized") toast("Could not load boards: " + e.message);
    return;
  }
  const sel = $("board-select");
  sel.innerHTML = "";
  for (const b of state.boards) {
    const opt = document.createElement("option");
    opt.value = b.id;
    opt.textContent = b.name;
    sel.appendChild(opt);
  }
  const stillThere = state.boards.some((b) => b.id === state.boardId);
  if (state.boards.length === 0) {
    state.boardId = null;
    $("board-empty").classList.remove("hidden");
    $("board").innerHTML = "";
  } else {
    state.boardId = stillThere ? state.boardId : state.boards[0].id;
    sel.value = state.boardId;
    $("board-empty").classList.add("hidden");
    await loadBoard();
  }
}

async function createBoard() {
  const name = prompt("Board name:");
  if (!name || !name.trim()) return;
  try {
    const board = await api("/api/boards", {
      method: "POST",
      body: JSON.stringify({ name: name.trim() }),
    });
    state.boardId = board.id; // loadBoards keeps the selection
    await loadBoards();
  } catch (e) {
    if (e.message !== "unauthorized") toast("Could not create board: " + e.message);
  }
}

async function loadBoard() {
  if (!state.boardId) return;
  try {
    const cards = (await api("/api/boards/" + encodeURIComponent(state.boardId) + "/cards")) || [];
    state.cards = new Map(cards.map((c) => [c.id, c]));
    renderBoard();
  } catch (e) {
    if (e.message !== "unauthorized") toast("Could not load cards: " + e.message);
  }
}

/* ---------------- kanban board ---------------- */

function renderBoard(preserveScroll = false) {
  // Remember per-column scroll so live re-renders don't jump.
  let scrolls = null;
  if (preserveScroll) {
    scrolls = {};
    document.querySelectorAll(".column-cards").forEach((el) => {
      scrolls[el.dataset.column] = el.scrollTop;
    });
  }

  const board = $("board");
  board.innerHTML = "";
  for (const col of COLUMNS) {
    const cards = [...state.cards.values()]
      .filter((c) => c.column === col.key)
      .sort((a, b) => new Date(b.updated_at) - new Date(a.updated_at));

    const colEl = document.createElement("section");
    colEl.className = "column";

    const head = document.createElement("div");
    head.className = "column-head";
    const name = document.createElement("span");
    name.textContent = col.label;
    const count = document.createElement("span");
    count.className = "column-count";
    count.textContent = String(cards.length);
    head.append(name, count);

    const list = document.createElement("div");
    list.className = "column-cards";
    list.dataset.column = col.key;
    if (cards.length === 0) {
      const empty = document.createElement("div");
      empty.className = "column-empty";
      empty.textContent = "No cards";
      list.appendChild(empty);
    } else {
      for (const c of cards) list.appendChild(cardEl(c));
    }

    colEl.append(head, list);
    board.appendChild(colEl);
  }

  if (scrolls) {
    document.querySelectorAll(".column-cards").forEach((el) => {
      if (scrolls[el.dataset.column] != null) el.scrollTop = scrolls[el.dataset.column];
    });
  }
}

function cardEl(c) {
  const el = document.createElement("div");
  el.className = "card"
    + (c.column === "needs_you" ? " needs-you" : "")
    + (c.column === "waiting" ? " waiting" : "")
    + (c.id === state.openCardId ? " selected" : "");

  const title = document.createElement("div");
  title.className = "card-title";
  title.textContent = c.title;
  el.appendChild(title);

  const meta = document.createElement("div");
  meta.className = "card-meta";

  const pill = document.createElement("span");
  pill.className = "pill";
  pill.textContent = c.source || "manual";
  meta.appendChild(pill);

  // Blue progress badge, only in the Working column.
  if (c.column === "working" && c.progress_pct > 0) {
    const prog = document.createElement("span");
    prog.className = "badge progress";
    prog.textContent = c.progress_pct + "%";
    meta.appendChild(prog);
  }

  // Alarm badge when the agent is waiting on the user.
  if (c.has_open_question) {
    const alarm = document.createElement("span");
    alarm.className = "badge alarm";
    alarm.textContent = "🔔";
    alarm.title = "Agent is waiting for your answer";
    meta.appendChild(alarm);
  }
  el.appendChild(meta);

  // Waiting reason, when parked on an external response.
  if (c.column === "waiting" && c.wait_reason) {
    const wr = document.createElement("div");
    wr.className = "card-wait";
    wr.textContent = "⏳ " + c.wait_reason;
    wr.title = c.wait_reason;
    el.appendChild(wr);
  }

  const sub = document.createElement("div");
  sub.className = "card-sub";
  const bits = [];
  if (c.agent_runtime) bits.push(c.agent_runtime);
  bits.push(timeAgo(c.updated_at));
  sub.textContent = bits.join(" · ");
  el.appendChild(sub);

  el.addEventListener("click", () => openCard(c.id));
  return el;
}

/* ---------------- card drawer ---------------- */

function openCard(id) {
  if (state.openCardId === id) return;
  state.openCardId = id;
  state.drawerStale = false;
  renderBoard(true); // update selection highlight without losing scroll
  $("drawer").classList.remove("hidden");
  // Move-to selector reflects the card's current column once detail loads.
  refreshDetail();
  openChat(id);
}

function closeDrawer() {
  state.openCardId = null;
  state.detail = null;
  state.drawerStale = false;
  closeChat();
  $("drawer").classList.add("hidden");
  renderBoard(true);
}

async function refreshDetail() {
  if (!state.openCardId) return;
  try {
    const d = await api("/api/cards/" + encodeURIComponent(state.openCardId));
    state.detail = d;
    // Keep the board summary in sync from the detail payload.
    if (d && d.id) state.cards.set(d.id, summaryFromDetail(d));
    renderBoard(true);
    renderDrawer();
  } catch (e) {
    if (e.message === "unauthorized") return; // showAuth already handled it
    toast("Could not load card: " + e.message);
  }
}

function summaryFromDetail(d) {
  const { body, steps, questions, messages, ...summary } = d;
  return summary;
}

// Lightweight progress update that never rebuilds the drawer (typing-safe).
function updateDrawerProgress(pct) {
  const bar = document.querySelector("#drawer-body .progress-fill");
  const label = document.querySelector("#drawer-body .progress-label");
  if (bar) bar.style.width = Math.max(0, Math.min(100, pct)) + "%";
  if (label) label.textContent = pct + "%";
}

function renderDrawer() {
  const d = state.detail;
  const body = $("drawer-body");
  body.innerHTML = "";
  if (!d) return;

  $("drawer-move").value = d.column || "inbox";

  // Title
  const h = document.createElement("h2");
  h.className = "drawer-title";
  h.textContent = d.title;
  body.appendChild(h);

  // Waiting banner when the card is parked on an external response.
  if (d.column === "waiting") {
    const wb = document.createElement("div");
    wb.className = "wait-banner";
    wb.textContent = "⏳ Waiting" + (d.wait_reason ? ": " + d.wait_reason : " — move back to Working to resume.");
    body.appendChild(wb);
  }

  // Source / runtime / container info line (short container id).
  const info = document.createElement("div");
  info.className = "drawer-info";
  const infoBits = [d.source || "manual"];
  if (d.agent_runtime) infoBits.push(d.agent_runtime);
  infoBits.push(d.container_id ? "container " + shortContainer(d.container_id) : (d.column === "done" ? "container removed" : "no container"));
  infoBits.push(timeAgo(d.updated_at));
  infoBits.forEach((b, i) => {
    if (i > 0) {
      const sep = document.createElement("span");
      sep.className = "sep";
      sep.textContent = "·";
      info.appendChild(sep);
    }
    const span = document.createElement("span");
    span.textContent = b;
    info.appendChild(span);
  });
  body.appendChild(info);

  // Container action.
  if (d.container_id) {
    const actions = document.createElement("div");
    actions.className = "drawer-actions";
    const stopBtn = document.createElement("button");
    stopBtn.className = "btn danger small";
    stopBtn.textContent = "Stop container";
    stopBtn.addEventListener("click", async () => {
      stopBtn.disabled = true;
      try {
        await api("/api/cards/" + encodeURIComponent(d.id) + "/stop", { method: "POST" });
        toast("Container stopped (volume kept).");
        await refreshDetail();
      } catch (e) {
        if (e.message !== "unauthorized") toast("Stop failed: " + e.message);
      } finally {
        stopBtn.disabled = false;
      }
    });
    actions.appendChild(stopBtn);
    body.appendChild(actions);
  }

  // Progress bar (meaningful while working).
  const progSec = document.createElement("div");
  progSec.className = "drawer-section";
  const progH = document.createElement("h3");
  progH.textContent = "Progress";
  const bar = document.createElement("div");
  bar.className = "progress-bar";
  const fill = document.createElement("div");
  fill.className = "progress-fill";
  fill.style.width = (d.progress_pct || 0) + "%";
  const label = document.createElement("div");
  label.className = "progress-label";
  label.textContent = (d.progress_pct || 0) + "%";
  bar.append(fill, label);
  progSec.append(progH, bar);
  body.appendChild(progSec);

  // Steps checklist (read-only).
  const stepsSec = document.createElement("div");
  stepsSec.className = "drawer-section";
  const stepsH = document.createElement("h3");
  stepsH.textContent = "Steps";
  stepsSec.appendChild(stepsH);
  const steps = (d.steps || []).slice().sort((a, b) => a.sort - b.sort);
  if (steps.length === 0) {
    const p = document.createElement("p");
    p.className = "muted";
    p.textContent = "No steps yet.";
    stepsSec.appendChild(p);
  } else {
    const ul = document.createElement("ul");
    ul.className = "steps";
    for (const s of steps) {
      const li = document.createElement("li");
      if (s.done) li.classList.add("done");
      const cb = document.createElement("input");
      cb.type = "checkbox";
      cb.checked = !!s.done;
      cb.disabled = true;
      cb.title = "Steps are agent-managed";
      const lab = document.createElement("span");
      lab.textContent = s.label;
      li.append(cb, lab);
      ul.appendChild(li);
    }
    stepsSec.appendChild(ul);
  }
  body.appendChild(stepsSec);

  // Questions thread. Open question is highlighted with an answer box.
  const qSec = document.createElement("div");
  qSec.className = "drawer-section";
  const qH = document.createElement("h3");
  qH.textContent = "Questions";
  qSec.appendChild(qH);
  const questions = d.questions || [];
  if (questions.length === 0) {
    const p = document.createElement("p");
    p.className = "muted";
    p.textContent = "No questions from the agent.";
    qSec.appendChild(p);
  }
  const openQ = questions.find((q) => !q.answered_at);
  for (const q of questions) {
    const qd = document.createElement("div");
    qd.className = "question" + (!q.answered_at ? " open" : "");
    const qt = document.createElement("div");
    qt.className = "q-text";
    qt.textContent = (!q.answered_at ? "🔔 " : "") + q.text;
    const qf = document.createElement("div");
    qf.className = "q-from";
    qf.textContent = (q.from ? q.from + " · " : "") + timeAgo(q.created_at);
    qd.append(qt, qf);
    if (q.answered_at) {
      const ans = document.createElement("div");
      ans.className = "q-answer";
      ans.textContent = "You answered " + timeAgo(q.answered_at) + ": " + (q.answer || "");
      qd.appendChild(ans);
    }
    qSec.appendChild(qd);
  }
  if (openQ) {
    const row = document.createElement("div");
    row.className = "answer-row";
    const ta = document.createElement("textarea");
    ta.id = "qa-answer-input";
    ta.rows = 3;
    ta.placeholder = "Type your answer — the agent resumes when you send it.";
    const btn = document.createElement("button");
    btn.id = "qa-answer-btn";
    btn.className = "btn primary small";
    btn.textContent = "Answer & resume";
    btn.addEventListener("click", () => answerQuestion(d.id, openQ.id));
    row.append(ta, btn);
    qSec.appendChild(row);
  }
  body.appendChild(qSec);

  // QA actions.
  if (d.column === "qa") {
    const qaSec = document.createElement("div");
    qaSec.className = "drawer-section";
    const qaH = document.createElement("h3");
    qaH.textContent = "QA";
    const row = document.createElement("div");
    row.className = "qa-actions";
    const approve = document.createElement("button");
    approve.className = "btn primary small";
    approve.textContent = "Approve & close";
    approve.addEventListener("click", () => moveCard(d.id, "done"));
    const back = document.createElement("button");
    back.className = "btn small";
    back.textContent = "Send back to working";
    back.addEventListener("click", () => sendBack(d.id));
    row.append(approve, back);
    qaSec.append(qaH, row);
    body.appendChild(qaSec);
  }

  // Messages thread + chat input (sent over the chat WebSocket).
  const mSec = document.createElement("div");
  mSec.className = "drawer-section";
  const mH = document.createElement("h3");
  mH.textContent = "Chat with agent";
  const msgs = document.createElement("div");
  msgs.className = "messages";
  msgs.id = "chat-messages";
  mSec.append(mH, msgs);

  const row = document.createElement("div");
  row.className = "msg-row";
  const input = document.createElement("input");
  input.id = "chat-input";
  input.type = "text";
  input.placeholder = "Message the agent…";
  input.maxLength = 4000;
  const send = document.createElement("button");
  send.id = "chat-send";
  send.className = "btn primary small";
  send.textContent = "Send";
  const doSend = () => sendChatMessage();
  send.addEventListener("click", doSend);
  input.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      doSend();
    }
  });
  row.append(input, send);
  mSec.appendChild(row);
  body.appendChild(mSec);

  for (const m of d.messages || []) appendMessage(m);
  msgs.scrollTop = msgs.scrollHeight;
}

function appendMessage(m) {
  const list = $("chat-messages");
  if (!list) return; // drawer not rendered / closed
  const el = document.createElement("div");
  el.className = "msg " + (m.from === "user" ? "user" : m.from === "system" ? "system" : "agent");
  const txt = document.createElement("div");
  txt.textContent = m.text;
  el.appendChild(txt);
  if (m.created_at) {
    const meta = document.createElement("div");
    meta.className = "msg-meta";
    meta.textContent = timeAgo(m.created_at);
    el.appendChild(meta);
  }
  list.appendChild(el);
  list.scrollTop = list.scrollHeight;
}

/* ---------------- drawer actions ---------------- */

async function answerQuestion(cardId, questionId) {
  const ta = $("qa-answer-input");
  const btn = $("qa-answer-btn");
  const answer = ta.value.trim();
  if (!answer) {
    toast("Write an answer first.");
    return;
  }
  btn.disabled = true;
  try {
    await api("/api/cards/" + encodeURIComponent(cardId) + "/answer", {
      method: "POST",
      body: JSON.stringify({ question_id: questionId, answer }),
    });
    // Typing ends here; safe to re-render.
    await refreshDetail();
  } catch (e) {
    if (e.message !== "unauthorized") toast("Answer failed: " + e.message);
    btn.disabled = false;
  }
}

async function moveCard(cardId, column, reason) {
  try {
    const d = await api("/api/cards/" + encodeURIComponent(cardId) + "/move", {
      method: "POST",
      body: JSON.stringify({ column, reason: reason || "" }),
    });
    state.detail = d;
    state.cards.set(d.id, summaryFromDetail(d));
    renderBoard(true);
    renderDrawer();
  } catch (e) {
    if (e.message !== "unauthorized") toast("Move failed: " + e.message);
  }
}

async function sendBack(cardId) {
  const note = prompt("Note for the agent (what needs to change?):");
  if (note === null || !note.trim()) return;
  try {
    const d = await api("/api/cards/" + encodeURIComponent(cardId) + "/sendback", {
      method: "POST",
      body: JSON.stringify({ note: note.trim() }),
    });
    state.detail = d;
    state.cards.set(d.id, summaryFromDetail(d));
    renderBoard(true);
    renderDrawer();
  } catch (e) {
    if (e.message !== "unauthorized") toast("Send-back failed: " + e.message);
  }
}

/* ---------------- board event stream (GET /api/stream) ---------------- */

function setLive(connected) {
  $("live-dot").classList.toggle("down", !connected);
}

function connectStream() {
  closeStream();
  if (!state.user) return;
  state.streamWanted = true;
  let ws;
  try {
    ws = new WebSocket(wsUrl("/api/stream"));
  } catch {
    scheduleStreamReconnect();
    return;
  }
  state.stream = ws;
  ws.onopen = () => {
    state.streamBackoff = 1000;
    setLive(true);
  };
  ws.onmessage = (ev) => {
    let msg;
    try {
      msg = JSON.parse(ev.data);
    } catch {
      return;
    }
    onStreamMsg(msg);
  };
  ws.onclose = () => {
    if (state.stream === ws) state.stream = null;
    setLive(false);
    if (state.streamWanted && state.user) scheduleStreamReconnect();
  };
  ws.onerror = () => {
    try {
      ws.close();
    } catch { /* onclose handles reconnect */ }
  };
}

function scheduleStreamReconnect() {
  const delay = Math.min(state.streamBackoff, 30000) + Math.random() * 500;
  state.streamBackoff *= 2;
  setTimeout(() => {
    if (state.streamWanted && state.user && !state.stream) connectStream();
  }, delay);
}

function closeStream() {
  state.streamWanted = false;
  if (state.stream) {
    try {
      state.stream.close();
    } catch { /* already closed */ }
    state.stream = null;
  }
  setLive(false);
}

function onStreamMsg(msg) {
  if (!msg || msg.type === "hello") return;
  if (msg.type !== "card" || !msg.card || !msg.card.id) return;
  const c = msg.card;
  // Ignore events for other boards if the payload says which board it belongs to.
  if (c.board_id && state.boardId && c.board_id !== state.boardId) return;

  // Update the board card in place, preserving column scroll positions.
  state.cards.set(c.id, { ...(state.cards.get(c.id) || {}), ...c });
  renderBoard(true);

  // If the drawer is open for this card: refetch the detail — unless the user
  // is typing, in which case apply only DOM-safe updates and defer the rest.
  const cardId = msg.card_id || c.id;
  if (state.openCardId && state.openCardId === cardId) {
    if (state.drawerTyping) {
      if (msg.action === "progress" && typeof c.progress_pct === "number") {
        updateDrawerProgress(c.progress_pct);
      } else {
        state.drawerStale = true; // refetch on blur
      }
    } else {
      refreshDetail();
    }
  }
}

/* ---------------- per-card chat socket (GET /api/cards/:id/chat) ---------------- */

function openChat(cardId) {
  closeChat();
  let ws;
  try {
    ws = new WebSocket(wsUrl("/api/cards/" + encodeURIComponent(cardId) + "/chat"));
  } catch {
    toast("Could not open chat.");
    return;
  }
  state.chat = ws;
  state.chatCardId = cardId;
  ws.onmessage = (ev) => {
    let f;
    try {
      f = JSON.parse(ev.data);
    } catch {
      return;
    }
    onChatFrame(f);
  };
  ws.onclose = () => {
    if (state.chat === ws) {
      state.chat = null;
      state.chatCardId = null;
    }
  };
}

function closeChat() {
  if (state.chat) {
    try {
      state.chat.close();
    } catch { /* already closed */ }
    state.chat = null;
    state.chatCardId = null;
  }
}

function onChatFrame(f) {
  if (!f || state.chatCardId !== state.openCardId) return;
  switch (f.kind) {
    case "history":
      // Full replace of the message list (initial load only); the chat input is untouched.
      {
        const list = $("chat-messages");
        if (list) {
          list.innerHTML = "";
          for (const m of f.messages || []) appendMessage(m);
        }
      }
      break;
    case "message":
      appendMessage({ from: f.from, text: f.text, created_at: new Date().toISOString() });
      break;
    case "progress":
      updateDrawerProgress(f.pct);
      // Keep the board badge in sync too.
      if (state.openCardId && state.cards.has(state.openCardId)) {
        const c = state.cards.get(state.openCardId);
        state.cards.set(state.openCardId, { ...c, progress_pct: f.pct });
        renderBoard(true);
      }
      break;
    case "steps":
    case "question":
    case "state":
      if (state.drawerTyping) state.drawerStale = true;
      else refreshDetail();
      break;
  }
}

function sendChatMessage() {
  const input = $("chat-input");
  if (!input) return;
  const text = input.value.trim();
  if (!text) return;
  if (!state.chat || state.chat.readyState !== WebSocket.OPEN) {
    toast("Chat is not connected yet.");
    return;
  }
  // Optimistically show our own message; the server will echo it back as well.
  appendMessage({ from: "user", text, created_at: new Date().toISOString() });
  state.chat.send(JSON.stringify({ text }));
  input.value = "";
}

/* ---------------- new card modal ---------------- */

function openCardModal() {
  if (!state.boardId) {
    toast("Create a board first.");
    return;
  }
  $("nc-title").value = "";
  $("nc-body").value = "";
  hideError("nc-error");
  $("card-modal").classList.remove("hidden");
  $("nc-title").focus();
}

function closeCardModal() {
  $("card-modal").classList.add("hidden");
}

async function submitNewCard(e) {
  e.preventDefault();
  hideError("nc-error");
  const title = $("nc-title").value.trim();
  const body = $("nc-body").value;
  if (!title) {
    showError("nc-error", "Title is required.");
    return;
  }
  try {
    const d = await api("/api/boards/" + encodeURIComponent(state.boardId) + "/cards", {
      method: "POST",
      body: JSON.stringify({ title, body }),
    });
    closeCardModal();
    // Add immediately so the UI feels instant; the stream event will reconcile it.
    state.cards.set(d.id, summaryFromDetail(d));
    renderBoard(true);
  } catch (err) {
    if (err.message !== "unauthorized") showError("nc-error", err.message);
  }
}

/* ---------------- admin view ----------------
   Sources (RSS), webhooks, and LLM providers. Backend 403s for non-admins;
   the nav button is hidden for them too (see showApp). */

async function loadAdmin() {
  if (!state.user || !state.user.is_admin) return;
  // Board picker for the feed form (defaults to the current board).
  const sel = $("feed-board");
  sel.innerHTML = "";
  for (const b of state.boards) {
    const opt = document.createElement("option");
    opt.value = b.id;
    opt.textContent = b.name;
    sel.appendChild(opt);
  }
  if (state.boardId) sel.value = state.boardId;

  $("webhook-url").textContent = location.origin + "/api/ingest/webhook/:source";

  try {
    const feeds = (await api("/api/admin/feeds")) || [];
    renderFeeds(feeds);
  } catch (e) {
    if (e.message !== "unauthorized") toast("Could not load feeds: " + e.message);
  }
  try {
    const sources = (await api("/api/admin/webhook-sources")) || [];
    renderWebhookSources(sources);
  } catch (e) {
    if (e.message !== "unauthorized") toast("Could not load webhook sources: " + e.message);
  }
  try {
    const providers = (await api("/api/admin/providers")) || [];
    renderProviders(providers);
  } catch (e) {
    if (e.message !== "unauthorized") toast("Could not load providers: " + e.message);
  }
  await loadRejected();
}

/* Inline intake-filter editor shared by feeds and webhook sources.
   onSave(enabled, instructions) persists via the caller's PUT endpoint. */
function filterEditor(current, onSave) {
  const ed = document.createElement("div");
  ed.className = "filter-editor";
  const label = document.createElement("label");
  label.className = "checkline";
  const cb = document.createElement("input");
  cb.type = "checkbox";
  cb.checked = !!current.enabled;
  label.append(cb, document.createTextNode(" Enable LLM intake filter"));
  const ta = document.createElement("textarea");
  ta.rows = 3;
  ta.placeholder = "Intake rules, e.g.: Accept only posts about X; reject ads, job listings, and duplicates.";
  ta.value = current.instructions || "";
  const actions = document.createElement("div");
  actions.className = "filter-editor-actions";
  const save = document.createElement("button");
  save.className = "btn primary small";
  save.textContent = "Save";
  const cancel = document.createElement("button");
  cancel.className = "btn small";
  cancel.textContent = "Cancel";
  actions.append(save, cancel);
  ed.append(label, ta, actions);
  save.addEventListener("click", async () => {
    save.disabled = true;
    try {
      await onSave(cb.checked, ta.value.trim());
      loadAdmin();
    } catch (e) {
      if (e.message !== "unauthorized") toast("Save failed: " + e.message);
      save.disabled = false;
    }
  });
  cancel.addEventListener("click", () => ed.remove());
  return ed;
}

function toggleFilterEditor(wrap, current, onSave) {
  const existing = wrap.querySelector(".filter-editor");
  if (existing) {
    existing.remove();
    return;
  }
  wrap.appendChild(filterEditor(current, onSave));
}

function boardName(id) {
  const b = state.boards.find((x) => x.id === id);
  return b ? b.name : "—";
}

function renderFeeds(feeds) {
  const list = $("feeds-list");
  list.innerHTML = "";
  if (feeds.length === 0) {
    const p = document.createElement("p");
    p.className = "muted";
    p.textContent = "No RSS sources configured.";
    list.appendChild(p);
    return;
  }
  for (const f of feeds) {
    const wrap = document.createElement("div");
    const row = document.createElement("div");
    row.className = "admin-row";

    const main = document.createElement("div");
    main.className = "admin-row-main";
    const name = document.createElement("div");
    name.className = "admin-row-title";
    name.textContent = f.name;
    const url = document.createElement("a");
    url.className = "admin-row-sub link";
    url.href = f.url;
    url.target = "_blank";
    url.rel = "noopener";
    url.textContent = f.url;
    main.append(name, url);

    const meta = document.createElement("div");
    meta.className = "admin-row-meta";
    const bits = [
      boardName(f.board_id),
      "every " + Math.round((f.interval_sec || 900) / 60) + "m",
      "polled " + timeAgo(f.last_polled_at ? f.last_polled_at * 1000 : 0),
      "last item " + timeAgo(f.last_item_at ? f.last_item_at * 1000 : 0),
      f.filter_enabled ? "🔍 filtered" : "unfiltered",
    ];
    meta.textContent = bits.join(" · ");

    const filtBtn = document.createElement("button");
    filtBtn.className = "btn small";
    filtBtn.textContent = "Filter";
    filtBtn.title = "Edit LLM intake filter";
    filtBtn.addEventListener("click", () =>
      toggleFilterEditor(
        wrap,
        { enabled: f.filter_enabled, instructions: f.filter_instructions },
        (enabled, instructions) =>
          api("/api/admin/feeds/" + encodeURIComponent(f.id), {
            method: "PUT",
            body: JSON.stringify({ filter_enabled: enabled, filter_instructions: instructions }),
          })
      )
    );

    const del = document.createElement("button");
    del.className = "btn danger small";
    del.textContent = "Delete";
    del.addEventListener("click", () => deleteFeed(f.id, f.name));

    row.append(main, meta, filtBtn, del);
    wrap.appendChild(row);
    list.appendChild(wrap);
  }
}

async function deleteFeed(id, name) {
  if (!confirm("Delete RSS source \"" + name + "\"?")) return;
  try {
    await api("/api/admin/feeds/" + encodeURIComponent(id), { method: "DELETE" });
    loadAdmin();
  } catch (e) {
    if (e.message !== "unauthorized") toast("Delete failed: " + e.message);
  }
}

async function submitFeed(e) {
  e.preventDefault();
  hideError("feed-error");
  const name = $("feed-name").value.trim();
  const url = $("feed-url").value.trim();
  const boardId = $("feed-board").value;
  const minutes = Math.max(1, parseInt($("feed-interval").value, 10) || 15);
  const filterEnabled = $("feed-filter-enabled").checked;
  const filterInstructions = $("feed-filter-instructions").value.trim();
  if (!name || !url) {
    showError("feed-error", "Name and URL are required.");
    return;
  }
  try {
    await api("/api/admin/feeds", {
      method: "POST",
      body: JSON.stringify({
        name,
        url,
        board_id: boardId,
        interval_sec: minutes * 60,
        filter_enabled: filterEnabled,
        filter_instructions: filterInstructions,
      }),
    });
    $("feed-name").value = "";
    $("feed-url").value = "";
    $("feed-filter-enabled").checked = false;
    $("feed-filter-instructions").value = "";
    toast("Feed added.");
    loadAdmin();
  } catch (err) {
    if (err.message !== "unauthorized") showError("feed-error", err.message);
  }
}

function renderWebhookSources(sources) {
  const el = $("webhook-sources");
  el.innerHTML = "";
  if (sources.length === 0) {
    const p = document.createElement("p");
    p.className = "muted";
    p.textContent = "No webhook events received yet.";
    el.appendChild(p);
    return;
  }
  const h = document.createElement("h3");
  h.textContent = "Sources seen";
  el.appendChild(h);
  for (const s of sources) {
    const wrap = document.createElement("div");
    const row = document.createElement("div");
    row.className = "admin-row slim";
    const name = document.createElement("code");
    name.textContent = s.source;
    const count = document.createElement("span");
    count.className = "muted";
    count.textContent = s.count + (s.count === 1 ? " card" : " cards");
    const badge = document.createElement("span");
    badge.className = "pill";
    badge.textContent = s.filter_enabled ? "🔍 filtered" : "unfiltered";
    const filtBtn = document.createElement("button");
    filtBtn.className = "btn small";
    filtBtn.textContent = "Filter";
    filtBtn.title = "Edit LLM intake filter";
    filtBtn.addEventListener("click", () =>
      toggleFilterEditor(
        wrap,
        { enabled: s.filter_enabled, instructions: s.filter_instructions },
        (enabled, instructions) =>
          api("/api/admin/webhook-sources/" + encodeURIComponent(s.source), {
            method: "PUT",
            body: JSON.stringify({ filter_enabled: enabled, filter_instructions: instructions }),
          })
      )
    );
    row.append(name, count, badge, filtBtn);
    wrap.appendChild(row);
    el.appendChild(wrap);
  }
}

function copyWebhookUrl() {
  const url = location.origin + "/api/ingest/webhook/:source";
  const done = () => toast("Webhook URL copied.");
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(url).then(done, () => toast("Copy failed."));
  } else {
    const ta = document.createElement("textarea");
    ta.value = url;
    document.body.appendChild(ta);
    ta.select();
    try {
      document.execCommand("copy");
      done();
    } catch {
      toast("Copy failed.");
    }
    ta.remove();
  }
}

/* ---------------- rejected items ----------------
   Items the intake filter declined. The tuning loop for filter
   instructions: review what was rejected, or accept one anyway. */

async function loadRejected() {
  try {
    const data = (await api("/api/admin/rejected?limit=50")) || { items: [], filter_fallbacks: 0 };
    renderRejected(data);
  } catch (e) {
    if (e.message !== "unauthorized") toast("Could not load rejected items: " + e.message);
  }
}

function renderRejected(data) {
  const hint = $("filter-fallback-hint");
  const n = data.filter_fallbacks || 0;
  hint.classList.toggle("hidden", n === 0);
  if (n > 0) {
    hint.textContent =
      "⚠ " +
      n +
      " recent card" +
      (n === 1 ? " was" : "s were") +
      " accepted because the intake filter was unavailable (no LLM provider configured or the provider errored). Add a provider above to enable filtering.";
  }
  const el = $("rejected-list");
  el.innerHTML = "";
  const items = data.items || [];
  if (items.length === 0) {
    const p = document.createElement("p");
    p.className = "muted";
    p.textContent = "No rejected items.";
    el.appendChild(p);
    return;
  }
  for (const it of items) {
    const row = document.createElement("div");
    row.className = "admin-row";
    const main = document.createElement("div");
    main.className = "admin-row-main";
    const title = document.createElement("div");
    title.className = "admin-row-title";
    title.textContent = it.title;
    const sub = document.createElement("div");
    sub.className = "admin-row-sub";
    sub.textContent = it.kind + " · " + it.source + " · " + timeAgo(it.created_at * 1000);
    const reason = document.createElement("div");
    reason.className = "admin-row-sub";
    reason.textContent = "Rejected: " + (it.reason || "no reason given");
    main.append(title, sub, reason);
    const accept = document.createElement("button");
    accept.className = "btn small";
    accept.textContent = "Accept anyway";
    accept.title = "Create the card this item would have become";
    accept.addEventListener("click", () => acceptRejected(it.id));
    row.append(main, accept);
    el.appendChild(row);
  }
}

async function acceptRejected(id) {
  try {
    await api("/api/admin/rejected/" + encodeURIComponent(id) + "/accept", { method: "POST" });
    toast("Card created and started.");
    loadAdmin();
  } catch (e) {
    if (e.message !== "unauthorized") toast("Accept failed: " + e.message);
  }
}

function renderProviders(providers) {
  const list = $("providers-list");
  list.innerHTML = "";
  if (providers.length === 0) {
    const p = document.createElement("p");
    p.className = "muted";
    p.textContent = "No LLM providers configured.";
    list.appendChild(p);
    return;
  }
  for (const p of providers) {
    const row = document.createElement("div");
    row.className = "admin-row";

    const main = document.createElement("div");
    main.className = "admin-row-main";
    const name = document.createElement("div");
    name.className = "admin-row-title";
    name.textContent = p.name;
    const sub = document.createElement("div");
    sub.className = "admin-row-sub";
    const bits = [p.provider_type || "openai"];
    if (p.model) bits.push(p.model);
    if (p.endpoint) bits.push(p.endpoint);
    if (p.api_key_masked) bits.push("key " + p.api_key_masked);
    sub.textContent = bits.join(" · ");
    main.append(name, sub);

    const del = document.createElement("button");
    del.className = "btn danger small";
    del.textContent = "Delete";
    del.addEventListener("click", () => deleteProvider(p.id, p.name));

    row.append(main, del);
    list.appendChild(row);
  }
}

async function deleteProvider(id, name) {
  if (!confirm("Delete LLM provider \"" + name + "\"?")) return;
  try {
    await api("/api/admin/providers/" + encodeURIComponent(id), { method: "DELETE" });
    loadAdmin();
  } catch (e) {
    if (e.message !== "unauthorized") toast("Delete failed: " + e.message);
  }
}

async function submitProvider(e) {
  e.preventDefault();
  hideError("provider-error");
  const name = $("prov-name").value.trim();
  if (!name) {
    showError("provider-error", "Name is required.");
    return;
  }
  try {
    await api("/api/admin/providers", {
      method: "POST",
      body: JSON.stringify({
        name,
        provider_type: $("prov-type").value,
        endpoint: $("prov-endpoint").value.trim(),
        api_key: $("prov-key").value,
        model: $("prov-model").value.trim(),
      }),
    });
    $("prov-name").value = "";
    $("prov-endpoint").value = "";
    $("prov-key").value = "";
    $("prov-model").value = "";
    toast("Provider added.");
    loadAdmin();
  } catch (err) {
    if (err.message !== "unauthorized") showError("provider-error", err.message);
  }
}

/* ---------------- init ---------------- */

function init() {
  // Auth tabs.
  $("tab-login").addEventListener("click", () => setAuthTab("login"));
  $("tab-register").addEventListener("click", () => setAuthTab("register"));
  $("login-form").addEventListener("submit", (e) => {
    e.preventDefault();
    submitAuth("/api/auth/login", $("login-username").value, $("login-password").value);
  });
  $("register-form").addEventListener("submit", (e) => {
    e.preventDefault();
    submitAuth("/api/auth/register", $("register-username").value, $("register-password").value);
  });

  // Topbar.
  $("logout-btn").addEventListener("click", async () => {
    try {
      await api("/api/auth/logout", { method: "POST" });
    } catch { /* session may already be gone */ }
    showAuth();
  });
  $("nav-board").addEventListener("click", () => showView("board"));
  $("nav-admin").addEventListener("click", () => showView("admin"));
  $("board-select").addEventListener("change", (e) => {
    state.boardId = e.target.value;
    closeDrawer();
    loadBoard();
  });
  $("new-board-btn").addEventListener("click", createBoard);
  $("new-card-btn").addEventListener("click", openCardModal);

  // Drawer.
  $("drawer-close").addEventListener("click", closeDrawer);
  $("drawer-move").addEventListener("change", (e) => {
    if (!state.openCardId) return;
    const column = e.target.value;
    let reason = "";
    if (column === "waiting") {
      const r = prompt("Why is this card waiting? (e.g. waiting on vendor reply)");
      if (r === null) {
        // Cancelled: restore the selector to the card's actual column.
        e.target.value = state.detail ? state.detail.column : "inbox";
        return;
      }
      reason = r.trim();
    }
    moveCard(state.openCardId, column, reason);
  });

  // Typing guard: while focus is inside a drawer input/textarea we must not
  // re-render the drawer. Deferred refetches fire on blur.
  const drawer = $("drawer");
  drawer.addEventListener("focusin", (e) => {
    if (e.target.matches("input, textarea")) state.drawerTyping = true;
  });
  drawer.addEventListener("focusout", () => {
    // focusout fires before the new element receives focus; defer the check.
    setTimeout(() => {
      const active = document.activeElement;
      const stillInDrawer = active && drawer.contains(active) && active.matches("input, textarea");
      state.drawerTyping = !!stillInDrawer;
      if (!state.drawerTyping && state.drawerStale) {
        state.drawerStale = false;
        refreshDetail();
      }
    }, 0);
  });

  // New card modal.
  $("card-form").addEventListener("submit", submitNewCard);
  $("nc-cancel").addEventListener("click", closeCardModal);
  $("card-modal").addEventListener("click", (e) => {
    if (e.target === $("card-modal")) closeCardModal();
  });

  // Admin view.
  $("feed-form").addEventListener("submit", submitFeed);
  $("provider-form").addEventListener("submit", submitProvider);
  $("webhook-copy").addEventListener("click", copyWebhookUrl);
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape") {
      closeCardModal();
      // Don't auto-close the drawer on Escape: it holds in-progress typing.
    }
  });

  // Bootstrap: check for an existing session cookie.
  (async () => {
    try {
      state.user = await api("/api/auth/me");
      showApp();
    } catch (e) {
      showAuth(); // 401 or network failure -> auth view
    }
  })();
}

init();
