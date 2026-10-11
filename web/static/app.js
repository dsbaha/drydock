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
  view: "board",           // "board" | "admin" | "wiki"
  boards: [],
  boardId: null,
  wikiPageId: null,       // selected wiki page
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
  $("wiki-view").classList.add("hidden");
  $("admin-view").classList.add("hidden");
  $("nav-board").classList.add("active");
  $("nav-wiki").classList.remove("active");
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
  const wiki = which === "wiki";
  const flows = which === "flows";
  $("board-view").classList.toggle("hidden", admin || wiki || flows);
  $("wiki-view").classList.toggle("hidden", !wiki);
  $("admin-view").classList.toggle("hidden", !admin);
  $("flows-view").classList.toggle("hidden", !flows);
  $("nav-board").classList.toggle("active", which === "board");
  $("nav-wiki").classList.toggle("active", wiki);
  $("nav-flows").classList.toggle("active", flows);
  $("nav-admin").classList.toggle("active", admin);
  // Board controls don't apply to the admin view; the wiki and flows views
  // keep the board picker (both are per-board) but hide the action buttons.
  for (const id of ["new-board-btn", "new-card-btn"]) {
    $(id).classList.toggle("hidden", admin || wiki || flows);
  }
  $("board-select").classList.toggle("hidden", admin);
  if (admin) loadAdmin();
  if (wiki) loadWiki();
  if (flows) loadFlow();
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
  infoBits.push(d.container_id ? "container " + shortContainer(d.container_id) : (d.column === "done" ? "container & volume removed" : "no container"));
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
    stopBtn.textContent = "Remove container & volume";
    stopBtn.addEventListener("click", async () => {
      if (!window.confirm("Remove this card's container and its volume? The transcript stays in the database, but anything in /workspace will be gone.")) return;
      stopBtn.disabled = true;
      try {
        await api("/api/cards/" + encodeURIComponent(d.id) + "/stop", { method: "POST" });
        toast("Container and volume removed.");
        await refreshDetail();
      } catch (e) {
        if (e.message !== "unauthorized") toast("Remove failed: " + e.message);
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
  await loadInstructions();
  await loadProposals();
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

/* ---------------- orphaned volumes ----------------
   Scan lists Drydock-managed volumes whose card is gone or done; prune
   deletes exactly the checked names after a confirm. */

async function scanVolumes() {
  const err = $("volumes-error");
  err.classList.add("hidden");
  const btn = $("volumes-scan");
  btn.disabled = true;
  try {
    const data = await api("/api/admin/volumes/orphaned");
    renderOrphanedVolumes(data);
  } catch (e) {
    if (e.message !== "unauthorized") {
      err.textContent = "Scan failed: " + e.message;
      err.classList.remove("hidden");
    }
  } finally {
    btn.disabled = false;
  }
}

function renderOrphanedVolumes(data) {
  const el = $("volumes-list");
  el.innerHTML = "";
  if (data && data.unavailable) {
    const p = document.createElement("p");
    p.className = "muted";
    p.textContent = "Docker is not reachable from the server — nothing to scan.";
    el.appendChild(p);
    return;
  }
  const vols = Array.isArray(data) ? data : [];
  const head = document.createElement("p");
  head.className = "muted";
  head.textContent = vols.length === 0
    ? "No orphaned volumes found."
    : vols.length + " orphaned volume" + (vols.length === 1 ? "" : "s") + " found.";
  el.appendChild(head);
  if (vols.length === 0) return;

  const selAll = document.createElement("label");
  selAll.className = "checkline";
  const selAllCb = document.createElement("input");
  selAllCb.type = "checkbox";
  selAll.append(selAllCb, document.createTextNode(" Select all"));
  el.appendChild(selAll);

  const boxes = [];
  for (const v of vols) {
    const row = document.createElement("div");
    row.className = "admin-row";
    const main = document.createElement("div");
    main.className = "admin-row-main";
    const label = document.createElement("label");
    label.className = "checkline";
    const cb = document.createElement("input");
    cb.type = "checkbox";
    cb.value = v.name;
    boxes.push(cb);
    const title = document.createElement("span");
    title.className = "admin-row-title";
    title.textContent = v.name;
    label.append(cb, title);
    const sub = document.createElement("div");
    sub.className = "admin-row-sub";
    const cardBit = v.card_title ? "done card \u201C" + v.card_title + "\u201D" : "card gone";
    sub.textContent = cardBit + " \u00B7 " + timeAgo(v.created_at);
    main.append(label, sub);
    row.appendChild(main);
    el.appendChild(row);
  }
  selAllCb.addEventListener("change", () => {
    for (const cb of boxes) cb.checked = selAllCb.checked;
  });

  const del = document.createElement("button");
  del.className = "btn small danger";
  del.textContent = "Delete selected";
  del.addEventListener("click", async () => {
    const names = boxes.filter((cb) => cb.checked).map((cb) => cb.value);
    if (names.length === 0) {
      toast("Nothing selected.");
      return;
    }
    if (!confirm("Delete " + names.length + " volume(s)? This cannot be undone.")) return;
    del.disabled = true;
    try {
      const results = await api("/api/admin/volumes/prune", {
        method: "POST",
        body: JSON.stringify({ volumes: names }),
      });
      const ok = results.filter((r) => r.ok).length;
      const failed = results.filter((r) => !r.ok);
      toast("Deleted " + ok + " volume(s)" + (failed.length ? "; " + failed.length + " failed" : "") + ".");
      for (const f of failed) {
        if (f.error) toast(f.name + ": " + f.error);
      }
      scanVolumes();
    } catch (e) {
      if (e.message !== "unauthorized") toast("Prune failed: " + e.message);
      del.disabled = false;
    }
  });
  el.appendChild(del);
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

/* ---------------- wiki ----------------
   Per-board persistent wiki. Agents read pages at run start (see the run
   briefing) and propose edits through the approval queue; admins edit
   directly. Dependency-free markdown subset renderer below. */

function escHtml(s) {
  return s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
}

// md renders a small markdown subset: fenced code blocks, #/##/### headers,
// bold, italic, inline code, links, and - / 1. ordered lists.
function md(src) {
  const blocks = [];
  // Extract fenced code blocks first so their contents aren't processed.
  let text = src.replace(/```(\w*)\n([\s\S]*?)```/g, (m, lang, code) => {
    blocks.push('<pre><code>' + escHtml(code.replace(/\n$/, "")) + "</code></pre>");
    return "\u0000" + (blocks.length - 1) + "\u0000";
  });
  const inline = (s) => {
    s = escHtml(s);
    s = s.replace(/`([^`]+)`/g, "<code>$1</code>");
    s = s.replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>");
    s = s.replace(/(^|[^*])\*([^*\n]+)\*/g, "$1<em>$2</em>");
    s = s.replace(/\[([^\]]+)\]\((https?:[^)\s]+)\)/g, '<a href="$2" target="_blank" rel="noopener">$1</a>');
    return s;
  };
  const html = [];
  let list = null; // "ul" | "ol" | null
  const closeList = () => {
    if (list) {
      html.push(list === "ul" ? "</ul>" : "</ol>");
      list = null;
    }
  };
  for (const line of text.split("\n")) {
    const t = line.trim();
    if (/^\u0000\d+\u0000$/.test(t)) {
      closeList();
      html.push(blocks[parseInt(t.slice(1, -1), 10)]);
      continue;
    }
    let m;
    if ((m = t.match(/^(#{1,3})\s+(.*)/))) {
      closeList();
      html.push("<h" + m[1].length + ">" + inline(m[2]) + "</h" + m[1].length + ">");
    } else if ((m = t.match(/^[-*]\s+(.*)/))) {
      if (list !== "ul") {
        closeList();
        html.push("<ul>");
        list = "ul";
      }
      html.push("<li>" + inline(m[1]) + "</li>");
    } else if ((m = t.match(/^\d+\.\s+(.*)/))) {
      if (list !== "ol") {
        closeList();
        html.push("<ol>");
        list = "ol";
      }
      html.push("<li>" + inline(m[1]) + "</li>");
    } else if (t === "") {
      closeList();
    } else {
      closeList();
      html.push("<p>" + inline(t) + "</p>");
    }
  }
  closeList();
  return html.join("\n");
}

function wikiBase() {
  return "/api/boards/" + encodeURIComponent(state.boardId) + "/wiki";
}

async function loadWiki() {
  if (!state.boardId) return;
  state.wikiPageId = state.wikiPageId || null;
  const isAdmin = !!(state.user && state.user.is_admin);
  $("wiki-new-btn").classList.toggle("hidden", !isAdmin);
  $("wiki-edit-btn").classList.toggle("hidden", !isAdmin);
  $("wiki-delete-btn").classList.toggle("hidden", !isAdmin);
  try {
    const pages = (await api(wikiBase())) || [];
    renderWikiList(pages);
    if (state.wikiPageId) {
      await openWikiPage(state.wikiPageId);
    } else {
      showWikiEmpty();
    }
  } catch (e) {
    if (e.message !== "unauthorized") toast("Could not load wiki: " + e.message);
  }
}

function renderWikiList(pages) {
  const list = $("wiki-list");
  list.innerHTML = "";
  if (pages.length === 0) {
    const p = document.createElement("p");
    p.className = "muted";
    p.textContent = "No pages yet.";
    list.appendChild(p);
    return;
  }
  for (const p of pages) {
    const b = document.createElement("button");
    b.className = "wiki-page-link" + (p.id === state.wikiPageId ? " active" : "");
    b.textContent = p.title;
    b.title = "Updated " + timeAgo(p.updated_at * 1000);
    b.addEventListener("click", () => openWikiPage(p.id));
    list.appendChild(b);
  }
}

function showWikiEmpty() {
  $("wiki-empty").classList.remove("hidden");
  $("wiki-page").classList.add("hidden");
}

async function openWikiPage(id) {
  state.wikiPageId = id;
  document.querySelectorAll(".wiki-page-link").forEach((el) =>
    el.classList.toggle("active", false)
  );
  try {
    const data = await api(wikiBase() + "/" + encodeURIComponent(id));
    renderWikiPage(data);
  } catch (e) {
    if (e.message !== "unauthorized") toast("Could not open page: " + e.message);
  }
}

function renderWikiPage(data) {
  const page = data.page;
  const revs = data.revisions || [];
  $("wiki-empty").classList.add("hidden");
  $("wiki-page").classList.remove("hidden");
  $("wiki-title").textContent = page.title;
  $("wiki-meta").textContent =
    "Updated " + timeAgo(page.updated_at * 1000) + " · " + revs.length + " revision" + (revs.length === 1 ? "" : "s");
  $("wiki-body").innerHTML = md(page.body || "*Empty page.*");
  $("wiki-body").classList.remove("hidden");
  $("wiki-editor").classList.add("hidden");
  $("wiki-history").classList.add("hidden");
  $("wiki-history").innerHTML = "";
  document.querySelectorAll(".wiki-page-link").forEach((el) => {
    el.classList.toggle("active", el.textContent === page.title);
  });
  renderWikiHistory(revs);
}

function renderWikiHistory(revs) {
  const box = $("wiki-history");
  box.innerHTML = "";
  const h = document.createElement("h3");
  h.textContent = "Revision history";
  box.appendChild(h);
  if (revs.length === 0) {
    const p = document.createElement("p");
    p.className = "muted";
    p.textContent = "No revisions recorded.";
    box.appendChild(p);
    return;
  }
  for (const r of revs) {
    const wrap = document.createElement("div");
    wrap.className = "wiki-rev";
    const head = document.createElement("button");
    head.className = "wiki-rev-head";
    head.textContent = new Date(r.created_at * 1000).toLocaleString() + " · " + (r.edited_by || "unknown");
    const body = document.createElement("div");
    body.className = "wiki-body hidden";
    body.innerHTML = md(r.body || "*Empty.*");
    head.addEventListener("click", () => body.classList.toggle("hidden"));
    wrap.append(head, body);
    box.appendChild(wrap);
  }
}

function startWikiEdit() {
  const title = $("wiki-title").textContent;
  const id = state.wikiPageId;
  $("wiki-edit-title").value = title;
  $("wiki-edit-body").value = "";
  // Fetch the raw body via the page endpoint (already rendered otherwise).
  api(wikiBase() + "/" + encodeURIComponent(id)).then((data) => {
    $("wiki-edit-body").value = data.page.body || "";
  });
  $("wiki-body").classList.add("hidden");
  $("wiki-history").classList.add("hidden");
  $("wiki-editor").classList.remove("hidden");
  hideError("wiki-error");
}

async function saveWikiPage() {
  hideError("wiki-error");
  const title = $("wiki-edit-title").value.trim();
  const body = $("wiki-edit-body").value;
  if (!title) {
    showError("wiki-error", "Title is required.");
    return;
  }
  try {
    if (state.wikiPageId) {
      const page = await api(wikiBase() + "/" + encodeURIComponent(state.wikiPageId), {
        method: "PUT",
        body: JSON.stringify({ title, body }),
      });
      state.wikiPageId = page.id;
    } else {
      const page = await api(wikiBase(), {
        method: "POST",
        body: JSON.stringify({ title, body }),
      });
      state.wikiPageId = page.id;
    }
    toast("Page saved.");
    await loadWiki();
  } catch (e) {
    if (e.message !== "unauthorized") showError("wiki-error", e.message);
  }
}

async function deleteWikiPage() {
  if (!state.wikiPageId) return;
  if (!confirm("Delete wiki page \"" + $("wiki-title").textContent + "\" and its history?")) return;
  try {
    await api(wikiBase() + "/" + encodeURIComponent(state.wikiPageId), { method: "DELETE" });
    state.wikiPageId = null;
    toast("Page deleted.");
    await loadWiki();
  } catch (e) {
    if (e.message !== "unauthorized") toast("Delete failed: " + e.message);
  }
}

/* ---------------- instructions (admin) ---------------- */

async function loadInstructions() {
  // Global instructions.
  try {
    const s = await api("/api/admin/settings");
    $("global-instructions").value = s.instructions || "";
  } catch (e) {
    if (e.message !== "unauthorized") toast("Could not load settings: " + e.message);
  }
  // Per-board picker + editor.
  const sel = $("instr-board");
  sel.innerHTML = "";
  for (const b of state.boards) {
    const opt = document.createElement("option");
    opt.value = b.id;
    opt.textContent = b.name;
    sel.appendChild(opt);
  }
  if (state.boardId) sel.value = state.boardId;
  loadBoardInstructions();
}

function loadBoardInstructions() {
  const b = state.boards.find((x) => x.id === $("instr-board").value);
  $("board-instructions").value = (b && b.instructions) || "";
}

async function saveGlobalInstructions() {
  hideError("instr-error");
  try {
    await api("/api/admin/settings", {
      method: "PUT",
      body: JSON.stringify({ instructions: $("global-instructions").value.trim() }),
    });
    toast("Global instructions saved.");
  } catch (e) {
    if (e.message !== "unauthorized") showError("instr-error", e.message);
  }
}

async function saveBoardInstructions() {
  hideError("instr-error");
  const boardId = $("instr-board").value;
  if (!boardId) return;
  try {
    const b = await api("/api/boards/" + encodeURIComponent(boardId), {
      method: "PUT",
      body: JSON.stringify({ instructions: $("board-instructions").value.trim() }),
    });
    const local = state.boards.find((x) => x.id === boardId);
    if (local) local.instructions = b.instructions;
    toast("Board instructions saved.");
  } catch (e) {
    if (e.message !== "unauthorized") showError("instr-error", e.message);
  }
}

/* ---------------- wiki proposals queue (admin) ---------------- */

async function loadProposals() {
  try {
    const pending = (await api("/api/admin/wiki-proposals?status=pending")) || [];
    renderProposals(pending);
    updateAdminBadge(pending.length);
  } catch (e) {
    if (e.message !== "unauthorized") toast("Could not load proposals: " + e.message);
  }
}

function updateAdminBadge(count) {
  const badge = $("nav-admin-badge");
  if (count > 0) {
    badge.textContent = count;
    badge.classList.remove("hidden");
  } else {
    badge.classList.add("hidden");
  }
}

function renderProposals(pending) {
  const list = $("proposals-list");
  list.innerHTML = "";
  if (pending.length === 0) {
    const p = document.createElement("p");
    p.className = "muted";
    p.textContent = "No pending proposals.";
    list.appendChild(p);
    return;
  }
  for (const p of pending) {
    const wrap = document.createElement("div");
    wrap.className = "proposal";
    const head = document.createElement("div");
    head.className = "admin-row-title";
    head.textContent = (p.page_id ? "Edit: " : "New page: ") + p.title;
    const meta = document.createElement("div");
    meta.className = "admin-row-meta";
    const bits = ["board: " + boardName(p.board_id), "proposed " + timeAgo(p.created_at * 1000)];
    if (p.card_id) bits.push("card " + p.card_id.slice(0, 8));
    meta.textContent = bits.join(" · ");
    const rationale = document.createElement("p");
    rationale.className = "muted";
    rationale.textContent = p.rationale || "No rationale given.";
    const body = document.createElement("div");
    body.className = "wiki-body proposal-body";
    body.innerHTML = md(p.proposed_body || "*Empty.*");
    const actions = document.createElement("div");
    actions.className = "admin-row";
    const approve = document.createElement("button");
    approve.className = "btn primary small";
    approve.textContent = "Approve & apply";
    approve.addEventListener("click", () => decideProposal(p.id, true));
    const reject = document.createElement("button");
    reject.className = "btn danger small";
    reject.textContent = "Reject";
    reject.addEventListener("click", () => decideProposal(p.id, false));
    actions.append(approve, reject);
    wrap.append(head, meta, rationale, body, actions);
    list.appendChild(wrap);
  }
}

async function decideProposal(id, approve) {
  try {
    await api("/api/admin/wiki-proposals/" + encodeURIComponent(id) + (approve ? "/approve" : "/reject"), {
      method: "POST",
    });
    toast(approve ? "Proposal applied." : "Proposal rejected.");
    loadProposals();
  } catch (e) {
    if (e.message !== "unauthorized") toast("Decision failed: " + e.message);
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
  $("nav-wiki").addEventListener("click", () => showView("wiki"));
  $("nav-flows").addEventListener("click", () => showView("flows"));
  $("nav-admin").addEventListener("click", () => showView("admin"));
  $("board-select").addEventListener("change", (e) => {
    state.boardId = e.target.value;
    closeDrawer();
    if (state.view === "wiki") {
      state.wikiPageId = null;
      loadWiki();
    } else if (state.view === "flows") {
      loadFlow();
    } else {
      loadBoard();
    }
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

  // Wiki view.
  $("wiki-new-btn").addEventListener("click", () => {
    state.wikiPageId = null;
    $("wiki-empty").classList.add("hidden");
    $("wiki-page").classList.remove("hidden");
    $("wiki-title").textContent = "New page";
    $("wiki-meta").textContent = "";
    $("wiki-body").classList.add("hidden");
    $("wiki-history").classList.add("hidden");
    $("wiki-edit-title").value = "";
    $("wiki-edit-body").value = "";
    $("wiki-editor").classList.remove("hidden");
    hideError("wiki-error");
  });
  $("wiki-edit-btn").addEventListener("click", startWikiEdit);
  $("wiki-save-btn").addEventListener("click", saveWikiPage);
  $("wiki-cancel-btn").addEventListener("click", () => {
    if (state.wikiPageId) openWikiPage(state.wikiPageId);
    else showWikiEmpty();
  });
  $("wiki-delete-btn").addEventListener("click", deleteWikiPage);
  $("wiki-history-btn").addEventListener("click", () => {
    $("wiki-history").classList.toggle("hidden");
  });

  // Admin: instructions.
  $("save-global-instructions").addEventListener("click", saveGlobalInstructions);
  $("save-board-instructions").addEventListener("click", saveBoardInstructions);
  $("instr-board").addEventListener("change", loadBoardInstructions);

  // Admin view.
  $("feed-form").addEventListener("submit", submitFeed);
  $("provider-form").addEventListener("submit", submitProvider);
  $("webhook-copy").addEventListener("click", copyWebhookUrl);
  $("volumes-scan").addEventListener("click", scanVolumes);
  flowBindUI();
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

/* ---------------- flows editor ---------------- */
// Node-RED-style visual pipeline builder. Dependency-free SVG canvas:
// click palette to add nodes, drag nodes to move, drag output→input ports
// to wire, click a wire to delete it, drag the background to pan.

const FLOW_TYPES = [
  { type: "inbox", label: "Inbox", cat: "input", desc: "Item accepted by intake filtering" },
  { type: "manual", label: "Manual", cat: "input", desc: "\u201cNew card\u201d button" },
  { type: "llm_filter", label: "LLM filter", cat: "processor", desc: "Triage with the LLM" },
  { type: "llm", label: "LLM", cat: "processor", desc: "Transform with the LLM" },
  { type: "worker", label: "Worker", cat: "processor", desc: "Create card + start agent" },
  { type: "notify", label: "Notify", cat: "output", desc: "Webhook on card close" },
];
const FLOW_PORTS = {
  inbox: [[], ["out"]],
  manual: [[], ["out"]],
  llm_filter: [["in"], ["accept", "reject"]],
  llm: [["in"], ["out"]],
  worker: [["in"], ["out"]], // out is optional: fires after the card is created
  notify: [["in"], []],
};
const FLOW_NODE_W = 180;
const FLOW_NODE_H = 78;

const flowEd = {
  nodes: new Map(),
  edges: [],
  selected: null,
  pan: { x: 24, y: 24 },
  drag: null,   // {kind:"node"|"pan"|"link", ...}
  moved: false, // pointer traveled: suppress click actions
  nextNum: 1,
  runtimes: [],
  readonly: true,
};

function flowTypeLabel(t) {
  const f = FLOW_TYPES.find((x) => x.type === t);
  return f ? f.label : t;
}

function flowSummary(n) {
  const cfg = n.config || {};
  const s = (v) => (v || "").toString().trim();
  switch (n.type) {
    case "inbox": return "accepted intake items";
    case "manual": return "\u201cNew card\u201d button";
    case "llm_filter": return s(cfg.instructions) ? s(cfg.instructions).slice(0, 42) : "no instructions set";
    case "llm": {
      const out = s(cfg.output) || "body";
      return "\u2192 " + out;
    }
    case "worker": {
      const rt = s(cfg.runtime) || "default";
      const v = cfg.verify === false ? "verify off" : "verify on";
      return rt + " \u00b7 " + v;
    }
    case "notify": return s(cfg.url) || "no URL set";
    default: return "";
  }
}

function flowIsAdmin() {
  return !!(state.user && state.user.is_admin);
}

/* ----- loading ----- */

async function loadFlow() {
  if (!state.boardId) return;
  flowEd.readonly = !flowIsAdmin();
  hideError("flow-error");
  try {
    const f = await api("/api/boards/" + encodeURIComponent(state.boardId) + "/flow");
    const def = JSON.parse(f.definition || '{"nodes":[],"edges":[]}');
    flowEd.nodes = new Map(
      (def.nodes || []).map((n) => [n.id, { id: n.id, type: n.type, x: +n.x || 0, y: +n.y || 0, config: n.config || {} }])
    );
    flowEd.edges = def.edges || [];
    flowEd.selected = null;
    flowEd.pan = { x: 24, y: 24 };
    flowEd.nextNum = flowEd.nodes.size + 1;
    if (!flowEd.readonly) {
      try {
        flowEd.runtimes = (await api("/api/admin/runtimes")) || [];
      } catch { flowEd.runtimes = []; }
    }
    flowRenderPalette();
    flowRender();
    flowRenderConfig();
    flowValidateRender();
    if (!flowEd.readonly) {
      $("flow-runs-wrap").classList.remove("hidden");
      flowLoadRuns();
    } else {
      $("flow-runs-wrap").classList.add("hidden");
    }
    // Read-only mode: hide editing affordances.
    $("flow-palette-items").classList.toggle("hidden", flowEd.readonly);
    for (const id of ["flow-save", "flow-reset"]) {
      $(id).classList.toggle("hidden", flowEd.readonly);
    }
  } catch (e) {
    if (e.message !== "unauthorized") showError("flow-error", "Could not load flow: " + e.message);
  }
}

async function flowLoadRuns() {
  try {
    const runs = (await api("/api/admin/flow-runs?board_id=" + encodeURIComponent(state.boardId))) || [];
    const wrap = $("flow-runs");
    wrap.innerHTML = "";
    if (runs.length === 0) {
      wrap.innerHTML = '<p class="muted small">No runs yet.</p>';
      return;
    }
    for (const r of runs.slice(0, 20)) {
      const div = document.createElement("div");
      div.className = "flow-run";
      const pill = document.createElement("span");
      pill.className = "flow-run-status " + r.status;
      pill.textContent = r.status;
      const det = document.createElement("span");
      det.className = "flow-run-detail";
      det.textContent = r.detail || "";
      det.title = r.detail || "";
      const ts = document.createElement("span");
      ts.className = "muted small";
      ts.textContent = timeAgo(r.created_at);
      div.append(pill, det, ts);
      wrap.appendChild(div);
    }
  } catch { /* non-fatal */ }
}

/* ----- palette ----- */

function flowRenderPalette() {
  const wrap = $("flow-palette-items");
  wrap.innerHTML = "";
  for (const t of FLOW_TYPES) {
    const b = document.createElement("button");
    b.className = "btn small flow-palette-btn cat-" + t.cat;
    b.innerHTML = "<strong></strong><span></span>";
    b.querySelector("strong").textContent = t.label;
    b.querySelector("span").textContent = t.desc;
    b.title = "Add " + t.label + " node";
    b.addEventListener("click", () => flowAddNode(t.type));
    wrap.appendChild(b);
  }
}

function flowAddNode(type) {
  const id = "n" + flowEd.nextNum++;
  const k = flowEd.nodes.size % 5;
  flowEd.nodes.set(id, { id, type, x: 120 + k * 44, y: 120 + k * 44, config: defaultNodeConfig(type) });
  flowSelect(id);
  flowRender();
  flowValidateRender();
}

function defaultNodeConfig(type) {
  switch (type) {
    case "worker": return { runtime: "", verify: true };
    case "llm": return { prompt: "", output: "body" };
    case "llm_filter": return { instructions: "" };
    case "notify": return { url: "" };
    default: return {};
  }
}

/* ----- canvas rendering ----- */

function svgPoint(evt) {
  const svg = $("flow-canvas");
  const pt = svg.createSVGPoint();
  pt.x = evt.clientX;
  pt.y = evt.clientY;
  return pt.matrixTransform(svg.getScreenCTM().inverse());
}

function flowPortPos(n, port, kind) {
  const outs = FLOW_PORTS[n.type][1];
  if (kind === "in") return { x: n.x, y: n.y + 44 };
  const i = Math.max(0, outs.indexOf(port));
  const y = outs.length === 1 ? n.y + 44 : n.y + 30 + i * 28;
  return { x: n.x + FLOW_NODE_W, y };
}

function flowEdgePath(p1, p2) {
  const dx = Math.max(40, Math.abs(p2.x - p1.x) / 2);
  return `M ${p1.x} ${p1.y} C ${p1.x + dx} ${p1.y}, ${p2.x - dx} ${p2.y}, ${p2.x} ${p2.y}`;
}

function esc(s) {
  return (s || "").toString().replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
}

function flowNodeHTML(n) {
  const t = FLOW_TYPES.find((x) => x.type === n.type) || { label: n.type, cat: "processor" };
  const sel = flowEd.selected === n.id ? " selected" : "";
  const outs = FLOW_PORTS[n.type][1];
  let ports = "";
  if (FLOW_PORTS[n.type][0].includes("in")) {
    const p = flowPortPos(n, "in", "in");
    ports += `<circle class="flow-port-in" data-node="${esc(n.id)}" data-port="in" cx="${p.x - n.x}" cy="${p.y - n.y}" r="6"><title>in</title></circle>`;
  }
  for (const port of outs) {
    const p = flowPortPos(n, port, "out");
    ports += `<circle class="flow-port-out" data-node="${esc(n.id)}" data-port="${esc(port)}" cx="${p.x - n.x}" cy="${p.y - n.y}" r="6"><title>${esc(port)}</title></circle>
      <text class="flow-port-label" x="${FLOW_NODE_W - 8}" y="${p.y - n.y - 9}" text-anchor="end">${esc(port)}</text>`;
  }
  return `<g class="flow-node cat-${t.cat}${sel}" data-id="${esc(n.id)}" transform="translate(${n.x},${n.y})">
    <rect class="flow-node-box" width="${FLOW_NODE_W}" height="${FLOW_NODE_H}" rx="8"></rect>
    <text class="flow-node-type" x="12" y="18">${esc(t.label)}</text>
    <text class="flow-node-summary" x="12" y="42">${esc(flowSummary(n))}</text>
    <text class="flow-node-hint" x="12" y="62">${esc(n.id)}</text>
    ${ports}
  </g>`;
}

function flowRender() {
  const svg = $("flow-canvas");
  let h = `<g transform="translate(${flowEd.pan.x},${flowEd.pan.y})"><g class="flow-edges">`;
  flowEd.edges.forEach((e, i) => {
    const a = flowEd.nodes.get(e.from), b = flowEd.nodes.get(e.to);
    if (!a || !b) return;
    const p1 = flowPortPos(a, e.from_port, "out"), p2 = flowPortPos(b, e.to_port, "in");
    h += `<path class="flow-edge" data-i="${i}" d="${flowEdgePath(p1, p2)}"><title>Wire ${esc(e.from_port)} → ${esc(e.to_port)} (click to delete)</title></path>`;
  });
  if (flowEd.drag && flowEd.drag.kind === "link") {
    const a = flowEd.nodes.get(flowEd.drag.from);
    if (a) {
      const p1 = flowPortPos(a, flowEd.drag.fromPort, "out");
      h += `<path class="flow-edge temp" d="${flowEdgePath(p1, { x: flowEd.drag.x, y: flowEd.drag.y })}"/>`;
    }
  }
  h += `</g><g class="flow-nodes">`;
  for (const n of flowEd.nodes.values()) h += flowNodeHTML(n);
  h += `</g></g>`;
  svg.innerHTML = h;
}

/* ----- canvas interaction ----- */

function flowBindCanvas() {
  const svg = $("flow-canvas");
  svg.addEventListener("pointerdown", (e) => {
    const outPort = e.target.closest(".flow-port-out");
    const nodeEl = e.target.closest(".flow-node");
    flowEd.moved = false;
    if (outPort && !flowEd.readonly) {
      flowEd.drag = { kind: "link", from: outPort.dataset.node, fromPort: outPort.dataset.port, x: 0, y: 0 };
      const p = svgPoint(e);
      flowEd.drag.x = p.x - flowEd.pan.x;
      flowEd.drag.y = p.y - flowEd.pan.y;
      svg.setPointerCapture(e.pointerId);
      e.preventDefault();
      return;
    }
    if (nodeEl) {
      const nid = nodeEl.dataset.id;
      flowSelect(nid);
      if (!flowEd.readonly) {
        const n = flowEd.nodes.get(nid);
        const p = svgPoint(e);
        flowEd.drag = { kind: "node", id: nid, dx: p.x - flowEd.pan.x - n.x, dy: p.y - flowEd.pan.y - n.y, sx: e.clientX, sy: e.clientY };
        svg.setPointerCapture(e.pointerId);
      }
      return;
    }
    flowEd.drag = { kind: "pan", sx: e.clientX, sy: e.clientY, px: flowEd.pan.x, py: flowEd.pan.y };
    svg.setPointerCapture(e.pointerId);
  });
  svg.addEventListener("pointermove", (e) => {
    const d = flowEd.drag;
    if (!d) return;
    if (d.kind === "pan") {
      if (Math.abs(e.clientX - d.sx) + Math.abs(e.clientY - d.sy) > 4) flowEd.moved = true;
      flowEd.pan.x = d.px + (e.clientX - d.sx);
      flowEd.pan.y = d.py + (e.clientY - d.sy);
      flowRender();
    } else if (d.kind === "node") {
      if (Math.abs(e.clientX - d.sx) + Math.abs(e.clientY - d.sy) > 4) flowEd.moved = true;
      const p = svgPoint(e);
      const n = flowEd.nodes.get(d.id);
      n.x = Math.round(p.x - flowEd.pan.x - d.dx);
      n.y = Math.round(p.y - flowEd.pan.y - d.dy);
      flowRender();
    } else if (d.kind === "link") {
      flowEd.moved = true;
      const p = svgPoint(e);
      d.x = p.x - flowEd.pan.x;
      d.y = p.y - flowEd.pan.y;
      flowRender();
    }
  });
  svg.addEventListener("pointerup", (e) => {
    const d = flowEd.drag;
    flowEd.drag = null;
    if (!d || d.kind !== "link") return;
    const inPort = e.target.closest(".flow-port-in");
    if (inPort) flowAddEdge(d.from, d.fromPort, inPort.dataset.node, inPort.dataset.port);
    else flowRender();
  });
  svg.addEventListener("click", (e) => {
    if (flowEd.moved || flowEd.readonly) return;
    const edgeEl = e.target.closest(".flow-edge");
    if (edgeEl && !edgeEl.classList.contains("temp")) {
      flowEd.edges.splice(+edgeEl.dataset.i, 1);
      flowRender();
      flowValidateRender();
      toast("Wire deleted");
    }
  });
  document.addEventListener("keydown", (e) => {
    if (state.view !== "flows" || flowEd.readonly) return;
    const tag = (document.activeElement || {}).tagName;
    if (tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT") return;
    if ((e.key === "Delete" || e.key === "Backspace") && flowEd.selected) {
      flowDeleteNode(flowEd.selected);
    } else if (e.key === "Escape") {
      flowSelect(null);
    }
  });
}

function flowAddEdge(from, fromPort, to, toPort) {
  const a = flowEd.nodes.get(from), b = flowEd.nodes.get(to);
  if (!a || !b) { flowRender(); return; }
  if (from === to) { toast("A node cannot wire to itself"); flowRender(); return; }
  if (!FLOW_PORTS[a.type][1].includes(fromPort) || !FLOW_PORTS[b.type][0].includes(toPort)) {
    toast("Invalid ports"); flowRender(); return;
  }
  if (flowEd.edges.some((e) => e.to === to && e.to_port === toPort)) {
    toast("That input port already has a wire"); flowRender(); return;
  }
  flowEd.edges.push({ from, from_port: fromPort, to, to_port: toPort });
  flowRender();
  flowValidateRender();
}

function flowDeleteNode(id) {
  flowEd.nodes.delete(id);
  flowEd.edges = flowEd.edges.filter((e) => e.from !== id && e.to !== id);
  if (flowEd.selected === id) flowEd.selected = null;
  flowRender();
  flowRenderConfig();
  flowValidateRender();
}

/* ----- selection + config panel ----- */

function flowSelect(id) {
  flowEd.selected = id;
  flowRender();
  flowRenderConfig();
}

function flowRenderConfig() {
  const wrap = $("flow-config");
  const body = $("flow-config-body");
  const n = flowEd.selected ? flowEd.nodes.get(flowEd.selected) : null;
  wrap.classList.toggle("hidden", !n);
  if (!n) { body.innerHTML = ""; return; }
  const ro = flowEd.readonly;
  const dis = ro ? "disabled" : "";
  let h = `<h3>${esc(flowTypeLabel(n.type))}</h3><p class="muted small">id: ${esc(n.id)}</p>`;
  const t = FLOW_TYPES.find((x) => x.type === n.type);
  if (t) h += `<p class="muted small">${esc(t.desc)}</p>`;
  const cfg = n.config || {};
  if (n.type === "llm_filter") {
    h += `<label>Instructions<textarea id="fc-instructions" rows="5" ${dis} placeholder="Accept only…; reject…">${esc(cfg.instructions || "")}</textarea></label>`;
  } else if (n.type === "llm") {
    h += `<label>Prompt (use {{title}}, {{body}}, {{var:name}})<textarea id="fc-prompt" rows="5" ${dis} placeholder="Rewrite the item…">${esc(cfg.prompt || "")}</textarea></label>`;
    const out = (cfg.output || "body").toString();
    const isVar = out.startsWith("var:");
    h += `<label>Output<select id="fc-output" ${dis}>
      <option value="body"${!isVar ? " selected" : ""}>Replace body</option>
      <option value="var"${isVar ? " selected" : ""}>Store in variable</option>
    </select></label>`;
    h += `<label id="fc-varwrap" class="${isVar ? "" : "hidden"}">Variable name<input id="fc-varname" type="text" value="${esc(isVar ? out.slice(4) : "")}" ${dis} placeholder="summary"></label>`;
  } else if (n.type === "worker") {
    const rt = (cfg.runtime || "").toString();
    h += `<label>Runtime<select id="fc-runtime" ${dis}>
      <option value="">Board default</option>
      ${flowEd.runtimes.map((r) => `<option value="${esc(r)}"${r === rt ? " selected" : ""}>${esc(r)}</option>`).join("")}
    </select></label>
    <label class="checkline"><input id="fc-verify" type="checkbox"${cfg.verify !== false ? " checked" : ""} ${dis}> Run verifier before done</label>`;
  } else if (n.type === "notify") {
    h += `<label>Webhook URL<input id="fc-url" type="url" value="${esc(cfg.url || "")}" ${dis} placeholder="https://example.com/hook"></label>
    <p class="muted small">POSTed <code>{card_id, board_id, title, transcript}</code> when the card closes.</p>`;
  }
  if (!ro) h += `<div class="admin-row" style="margin-top:12px"><button id="fc-delete" class="btn danger small">Delete node</button></div>`;
  body.innerHTML = h;
  if (ro) return;
  const upd = (fn) => { fn(); flowRender(); flowValidateRender(); };
  const bind = (id, fn) => { const el = $(id); if (el) el.addEventListener("change", () => upd(fn)); };
  bind("fc-instructions", () => { n.config.instructions = $("fc-instructions").value; });
  bind("fc-prompt", () => { n.config.prompt = $("fc-prompt").value; });
  bind("fc-output", () => {
    const v = $("fc-output").value;
    n.config.output = v === "var" ? "var:" + ($("fc-varname").value.trim() || "out") : "body";
    flowRenderConfig();
  });
  bind("fc-varname", () => { n.config.output = "var:" + ($("fc-varname").value.trim() || "out"); });
  bind("fc-runtime", () => { n.config.runtime = $("fc-runtime").value; });
  bind("fc-verify", () => { n.config.verify = $("fc-verify").checked; });
  bind("fc-url", () => { n.config.url = $("fc-url").value.trim(); });
  const del = $("fc-delete");
  if (del) del.addEventListener("click", () => flowDeleteNode(n.id));
}

/* ----- client-side validation (mirrors the server; the server is authoritative) ----- */

function flowValidate() {
  const errs = [];
  const nodes = [...flowEd.nodes.values()];
  const byId = new Map(nodes.map((n) => [n.id, n]));
  const outEdges = new Map(), inEdges = new Map();
  for (const e of flowEd.edges) {
    if (!byId.has(e.from) || !byId.has(e.to)) { errs.push(`Wire references a missing node (${e.from} → ${e.to})`); continue; }
    if (!outEdges.has(e.from)) outEdges.set(e.from, []);
    if (!inEdges.has(e.to)) inEdges.set(e.to, []);
    outEdges.get(e.from).push(e);
    inEdges.get(e.to).push(e);
  }
  const inputs = nodes.filter((n) => n.type === "inbox" || n.type === "manual");
  if (inputs.length === 0) errs.push("Flow needs at least one input node (inbox or manual)");
  for (const kind of ["inbox", "manual"]) {
    if (nodes.filter((n) => n.type === kind).length > 1) errs.push(`At most one ${kind} input node is allowed`);
  }
  const wiredIn = new Set(flowEd.edges.map((e) => e.to + ":" + e.to_port));
  const optionalOut = (t, p) => (t === "llm_filter" && p === "reject") || (t === "worker" && p === "out");
  for (const n of nodes) {
    const [ins, outs] = FLOW_PORTS[n.type] || [[], []];
    for (const p of ins) if (!wiredIn.has(n.id + ":" + p)) errs.push(`Node "${n.id}" (${n.type}): input "${p}" is not connected`);
    for (const p of outs) {
      if (optionalOut(n.type, p)) continue;
      if (!(outEdges.get(n.id) || []).some((e) => e.from_port === p)) errs.push(`Node "${n.id}" (${n.type}): output "${p}" is not connected`);
    }
  }
  // Cycle check.
  const WHITE = 0, GRAY = 1, BLACK = 2;
  const color = new Map(nodes.map((n) => [n.id, WHITE]));
  let cyclic = false;
  const dfs = (id) => {
    color.set(id, GRAY);
    for (const e of outEdges.get(id) || []) {
      const c = color.get(e.to);
      if (c === GRAY) { cyclic = true; return; }
      if (c === WHITE) dfs(e.to);
    }
    color.set(id, BLACK);
  };
  for (const n of nodes) if (color.get(n.id) === WHITE) dfs(n.id);
  if (cyclic) errs.push("Flow contains a cycle");
  // Terminal paths + notify-after-worker.
  const isTerminal = (t) => t === "worker" || t === "notify";
  for (const n of nodes) {
    if ((outEdges.get(n.id) || []).length === 0 && !isTerminal(n.type)) {
      errs.push(`Node "${n.id}" (${n.type}): paths must end at a worker or notify node`);
    }
  }
  const reachesInput = (startId) => {
    const seen = new Set([startId]);
    const stack = [startId];
    while (stack.length) {
      const id = stack.pop();
      for (const e of inEdges.get(id) || []) {
        const prev = byId.get(e.from);
        if (!prev || prev.type === "worker") continue;
        if (prev.type === "inbox" || prev.type === "manual") return true;
        if (!seen.has(e.from)) { seen.add(e.from); stack.push(e.from); }
      }
    }
    return false;
  };
  for (const n of nodes) {
    if (n.type === "notify" && reachesInput(n.id)) {
      errs.push(`Node "${n.id}" (notify): must come after a worker — a card has to exist first`);
    }
  }
  // Config schema.
  for (const n of nodes) {
    const cfg = n.config || {};
    const s = (v) => (v || "").toString().trim();
    if (n.type === "llm_filter" && !s(cfg.instructions)) errs.push(`Node "${n.id}" (llm_filter): instructions are required`);
    if (n.type === "llm") {
      if (!s(cfg.prompt)) errs.push(`Node "${n.id}" (llm): prompt is required`);
      const out = s(cfg.output) || "body";
      if (out !== "body" && !out.startsWith("var:")) errs.push(`Node "${n.id}" (llm): output must be "body" or "var:<name>"`);
    }
    if (n.type === "notify") {
      const u = s(cfg.url);
      if (!u) errs.push(`Node "${n.id}" (notify): url is required`);
      else if (!/^https?:\/\/.+\..+/.test(u)) errs.push(`Node "${n.id}" (notify): url must be an http(s) URL`);
    }
  }
  return errs;
}

function flowValidateRender() {
  const wrap = $("flow-validation");
  const errs = flowValidate();
  if (errs.length === 0) {
    wrap.innerHTML = '<p class="flow-valid">\u2713 Flow looks valid</p>';
  } else {
    wrap.innerHTML = "<ul>" + errs.map((e) => `<li>${esc(e)}</li>`).join("") + "</ul>";
  }
}

/* ----- save / reset ----- */

function flowDefinitionJSON() {
  return JSON.stringify({
    nodes: [...flowEd.nodes.values()].map((n) => ({
      id: n.id, type: n.type, x: Math.round(n.x), y: Math.round(n.y), config: n.config || {},
    })),
    edges: flowEd.edges.map((e) => ({ from: e.from, from_port: e.from_port, to: e.to, to_port: e.to_port })),
  });
}

async function flowSave() {
  hideError("flow-error");
  const path = "/api/boards/" + encodeURIComponent(state.boardId) + "/flow";
  try {
    const res = await fetch(path, {
      method: "PUT",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name: "Main flow", definition: flowDefinitionJSON() }),
    });
    if (!res.ok) {
      let msg = res.statusText, errs = null;
      try { const j = await res.json(); msg = j.error || msg; errs = j.errors || null; } catch { /* ignore */ }
      showError("flow-error", msg + (errs ? ": " + errs.join("; ") : ""));
      return;
    }
    toast("Flow saved");
    flowValidateRender();
    flowLoadRuns();
  } catch (e) {
    showError("flow-error", "Could not save flow: " + e.message);
  }
}

async function flowReset() {
  if (!confirm("Reset this board's flow to the default pipeline? Your custom flow will be replaced.")) return;
  hideError("flow-error");
  try {
    await api("/api/boards/" + encodeURIComponent(state.boardId) + "/flow/reset", { method: "POST" });
    toast("Flow reset to default");
    await loadFlow();
  } catch (e) {
    if (e.message !== "unauthorized") showError("flow-error", "Could not reset flow: " + e.message);
  }
}

function flowBindUI() {
  flowBindCanvas();
  $("flow-save").addEventListener("click", flowSave);
  $("flow-reset").addEventListener("click", flowReset);
  $("flow-config-close").addEventListener("click", () => flowSelect(null));
}
