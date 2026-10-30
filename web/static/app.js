// Browser UI for the hub. No build step and no dependencies. All text from the
// server goes into the page through text nodes, so a message can never become markup.

const app = document.getElementById("app");
const origin = location.origin;

const state = {
  me: null,
  config: null,
  workspaces: [],
  workspaceId: null,
  agents: [],
  threads: [],
  threadId: null,
  messages: [],
  search: "",
  socket: null,
  socketRetry: 0,
  socketTimer: null,
  connected: true,
};

// DOM helper: h("div", { class: "x", onclick: fn }, "text", child)
function h(tag, props, ...children) {
  const el = document.createElement(tag);
  for (const [key, value] of Object.entries(props || {})) {
    if (value === undefined || value === null || value === false) continue;
    if (key.startsWith("on")) el.addEventListener(key.slice(2), value);
    else if (key === "class") el.className = value;
    else if (key === "value") el.value = value;
    else el.setAttribute(key, value === true ? "" : value);
  }
  for (const child of children.flat()) {
    if (child === undefined || child === null || child === false) continue;
    el.append(child);
  }
  return el;
}

class ApiError extends Error {
  constructor(status, message) {
    super(message);
    this.status = status;
  }
}

async function api(method, path, body) {
  let res;
  try {
    res = await fetch(path, {
      method,
      headers: body === undefined ? {} : { "Content-Type": "application/json" },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  } catch {
    throw new ApiError(0, "Could not reach the hub. Check your connection and try again.");
  }
  if (res.status === 204) return null;
  let data = null;
  try {
    data = await res.json();
  } catch {
    // A proxy error page, for example. The status below still says what happened.
  }
  if (!res.ok) {
    if (res.status === 401 && state.me) {
      state.me = null;
      closeSocket();
      renderAuth();
    }
    throw new ApiError(res.status, data?.error || `The hub answered with status ${res.status}.`);
  }
  return data;
}

function inviteFromURL() {
  const match = location.hash.match(/^#\/invite\/([\w-]+)$/);
  return match ? match[1] : "";
}

function clearInviteFromURL() {
  if (inviteFromURL()) history.replaceState(null, "", location.pathname);
}

const formatTime = (ms) => {
  const date = new Date(ms);
  const sameDay = date.toDateString() === new Date().toDateString();
  return sameDay
    ? date.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })
    : date.toLocaleDateString([], { month: "short", day: "numeric" }) + ", " + date.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
};

const stateWords = { online: "online", recent: "seen a moment ago", offline: "offline" };

function lamp(agentState) {
  return h("span", { class: `lamp ${agentState}`, title: stateWords[agentState] },
    h("span", { class: "hidden-text" }, stateWords[agentState]));
}

// Sign in and registration

function renderAuth() {
  const invite = inviteFromURL();
  const first = state.config.first_user;
  const canRegister = first || invite || state.config.registration === "open" || state.config.registration === "invite";
  let mode = first || invite ? "register" : "login";

  const story = h("div", { class: "auth-story" },
    h("h1", {}, "Let your coding sessions ask each other."),
    h("p", {}, "The session working on the frontend can ask the one working on the backend, and get the answer right where it is working."),
    demoDiagram());

  const formHost = h("div", { class: "auth-form" });

  const draw = () => {
    formHost.replaceChildren();
    const isRegister = mode === "register";
    const error = h("p", { class: "error", role: "alert" });
    const username = h("input", { name: "username", autocomplete: "username", required: true, autocapitalize: "none", spellcheck: "false" });
    const password = h("input", { name: "password", type: "password", required: true, autocomplete: isRegister ? "new-password" : "current-password", minlength: isRegister ? 10 : undefined });
    const inviteInput = h("input", { name: "invite", value: invite, autocomplete: "off", spellcheck: "false" });
    const submit = h("button", { class: "btn", type: "submit" }, isRegister ? "Create account" : "Log in");
    const needsInvite = isRegister && !first && state.config.registration === "invite";
    const showInvite = isRegister && !first && (needsInvite || invite);

    const form = h("form", {
      class: "dlg",
      onsubmit: async (event) => {
        event.preventDefault();
        submit.disabled = true;
        error.textContent = "";
        try {
          if (isRegister) {
            await api("POST", "/api/auth/register", { username: username.value, password: password.value, invite: inviteInput.value });
            clearInviteFromURL(); // registering used the code already
          } else {
            await api("POST", "/api/auth/login", { username: username.value, password: password.value });
          }
          await start();
        } catch (err) {
          error.textContent = err.message;
          submit.disabled = false;
        }
      },
    },
      h("h2", {}, isRegister ? (first ? "Create the first account" : "Create your account") : "Log in"),
      first && h("p", { class: "note" }, "Nobody has an account on this hub yet. This one will be its administrator."),
      state.config.demo && !first && h("p", { class: "warn" }, "New accounts here are a demo: limited, and deleted after 3 days. Do not send anything private."),
      h("label", {}, "Username",
        isRegister && h("span", { class: "hint" }, "Lowercase letters, digits, - and _"), username),
      h("label", {}, "Password",
        isRegister && h("span", { class: "hint" }, "At least 10 characters"), password),
      showInvite && h("label", {}, "Invite code", h("span", { class: "hint" }, "Ask a workspace owner for one"), inviteInput),
      error,
      submit,
      !first && canRegister && h("button", {
        class: "switch-mode", type: "button",
        onclick: () => { mode = isRegister ? "login" : "register"; draw(); },
      }, isRegister ? "I already have an account" : "Create an account"));
    formHost.append(form);
    username.focus();
  };

  draw();
  app.replaceChildren(h("main", { class: "auth" }, story, formHost));
}

function demoDiagram() {
  const ns = "http://www.w3.org/2000/svg";
  const svg = document.createElementNS(ns, "svg");
  svg.setAttribute("viewBox", "0 0 400 170");
  svg.setAttribute("class", "auth-demo");
  svg.setAttribute("aria-hidden", "true");
  const add = (tag, attrs, text) => {
    const el = document.createElementNS(ns, tag);
    for (const [k, v] of Object.entries(attrs)) el.setAttribute(k, v);
    if (text) el.textContent = text;
    svg.append(el);
  };
  add("text", { x: 60, y: 16, "text-anchor": "middle" }, "frontend");
  add("text", { x: 340, y: 16, "text-anchor": "middle" }, "backend");
  add("line", { class: "life", x1: 60, y1: 26, x2: 60, y2: 168 });
  add("line", { class: "life", x1: 340, y1: 26, x2: 340, y2: 168 });
  add("text", { class: "say draw", x: 74, y: 60 }, "Does /users paginate?");
  add("line", { class: "call draw", x1: 60, y1: 72, x2: 332, y2: 72 });
  add("path", { class: "tip-call draw", d: "M340 72l-9-5v10z" });
  add("text", { class: "say draw d2", x: 326, y: 118, "text-anchor": "end" }, "Yes, cursor based. See api/users.go");
  add("line", { class: "back draw d2", x1: 340, y1: 130, x2: 68, y2: 130 });
  add("path", { class: "tip-back draw d2", d: "M60 130l9-5v10z" });
  return svg;
}

// Main screen

async function start() {
  state.me = await api("GET", "/api/me");
  state.workspaces = await api("GET", "/api/workspaces");
  const invite = inviteFromURL();
  if (invite) {
    try {
      const joined = await api("POST", "/api/invites/accept", { code: invite });
      state.workspaces = await api("GET", "/api/workspaces");
      state.workspaceId = joined.workspace_id;
    } catch (err) {
      alertDialog("Could not join that workspace", err.message);
    }
    clearInviteFromURL();
  }
  const saved = Number(localStorage.getItem("workspace"));
  state.workspaceId ||= state.workspaces.find((w) => w.id === saved)?.id || state.workspaces[0]?.id || null;
  renderShell();
  if (state.workspaceId) await openWorkspace(state.workspaceId);
}

const refs = {};

function renderShell() {
  refs.rail = h("nav", { class: "rail", "aria-label": "Workspace" });
  refs.threads = h("section", { class: "threads", "aria-label": "Conversations" });
  refs.sheet = h("section", { class: "sheet", "aria-label": "Conversation" });
  refs.shell = h("div", { class: "shell" }, refs.rail, refs.threads, refs.sheet);
  app.replaceChildren(refs.shell);
  drawRail();
  drawThreads();
  drawSheet();
}

async function openWorkspace(id) {
  state.workspaceId = id;
  localStorage.setItem("workspace", String(id));
  state.threadId = null;
  state.messages = [];
  state.search = "";
  await reloadWorkspace();
  connectSocket();
}

async function reloadWorkspace() {
  const id = state.workspaceId;
  const [agents, threads] = await Promise.all([
    api("GET", `/api/workspaces/${id}/agents`),
    api("GET", `/api/workspaces/${id}/threads`),
  ]);
  if (id !== state.workspaceId) return;
  state.agents = agents;
  state.threads = threads;
  if (state.threadId) {
    state.messages = await api("GET", `/api/workspaces/${id}/messages?thread=${state.threadId}&limit=500`);
  }
  drawRail();
  drawThreads();
  drawSheet();
}

function myWorkspace() {
  return state.workspaces.find((w) => w.id === state.workspaceId);
}

function drawRail() {
  const workspace = myWorkspace();
  const coding = state.agents.filter((a) => a.kind === "agent");
  const people = state.agents.filter((a) => a.kind === "human");

  refs.rail.replaceChildren(
    h("div", { class: "rail-top" },
      h("label", {},
        h("span", { class: "hidden-text" }, "Workspace"),
        h("select", {
          onchange: (event) => {
            if (event.target.value === "new") return createWorkspaceDialog();
            openWorkspace(Number(event.target.value));
          },
        },
          state.workspaces.map((w) => h("option", { value: w.id, selected: w.id === state.workspaceId }, w.name)),
          h("option", { value: "new" }, "New workspace..."))),
      workspace?.demo && h("p", { class: "rail-note" }, "Demo workspace, deleted after 3 days. Do not send anything private.")),
    h("div", { class: "rail-scroll" },
      h("h2", {}, "Agents"),
      coding.map((a) => h("button", { class: "jack", onclick: () => agentDialog(a) },
        lamp(a.state),
        h("span", { class: "name" }, a.name, a.description && h("span", { class: "what" }, a.description)))),
      h("button", { class: "btn rail-add", onclick: addAgentDialog }, "Add agent"),
      h("h2", {}, "People"),
      people.map((a) => h("button", { class: "jack", onclick: () => peopleDialog() },
        lamp(a.state), h("span", { class: "name" }, a.name))),
      workspace?.role === "owner" && h("button", { class: "btn rail-add", onclick: peopleDialog }, "Invite someone")),
    h("div", { class: "rail-foot" },
      h("button", { class: "link", onclick: accountDialog }, state.me.username),
      h("button", { class: "link", onclick: logout }, "Log out")));
}

function drawThreads() {
  const searchBox = h("input", {
    type: "search", placeholder: "Search all messages", value: state.search, "aria-label": "Search all messages",
    oninput: debounce(async (event) => {
      state.search = event.target.value.trim();
      await drawThreadList();
    }, 250),
  });
  refs.threadList = h("div", { class: "thread-list" });
  refs.threads.replaceChildren(
    h("div", { class: "threads-top" },
      h("button", { class: "btn", onclick: () => openThread(null) }, "New conversation"),
      searchBox),
    refs.threadList);
  drawThreadList();
}

async function drawThreadList() {
  if (!refs.threadList) return;
  if (state.search) {
    const hits = await api("GET", `/api/workspaces/${state.workspaceId}/messages?q=${encodeURIComponent(state.search)}&limit=50`);
    refs.threadList.replaceChildren(...(hits.length
      ? hits.map((m) => threadButton(m.thread_id, `${m.from} to ${m.to}`, m.created_at, m.body))
      : [h("p", { class: "empty" }, "No messages match that search.")]));
    return;
  }
  refs.threadList.replaceChildren(...(state.threads.length
    ? state.threads.map((t) => threadButton(t.id, t.participants.join(", "), t.last_at, t.preview))
    : [h("p", { class: "empty" }, "Conversations between your agents show up here.")]));
}

function threadButton(id, who, when, preview) {
  return h("button", {
    class: "thread-row", "aria-current": id === state.threadId ? "true" : "false",
    onclick: () => openThread(id),
  },
    h("span", { class: "who" }, who, h("span", { class: "when" }, formatTime(when))),
    h("span", { class: "preview" }, preview));
}

async function openThread(id) {
  state.threadId = id;
  state.messages = id ? await api("GET", `/api/workspaces/${state.workspaceId}/messages?thread=${id}&limit=500`) : [];
  refs.shell?.classList.remove("show-list");
  drawThreadList();
  drawSheet();
}

function drawSheet() {
  const banner = !state.connected && h("p", { class: "banner", role: "status" }, "Connection to the hub lost. Reconnecting...");
  const backButton = h("button", { class: "btn plain back-btn", onclick: () => refs.shell.classList.add("show-list") }, "All conversations");

  if (!state.workspaceId) {
    refs.sheet.replaceChildren(h("div", { class: "seq-empty" }, h("h2", {}, "Create a workspace to begin")));
    return;
  }
  const coding = state.agents.filter((a) => a.kind === "agent");
  if (coding.length < 2 && !state.threadId) {
    refs.sheet.replaceChildren(backButton, banner || "", h("div", { class: "seq-empty" },
      h("h2", {}, coding.length ? "Add a second agent" : "Connect your first two sessions"),
      h("p", {}, "Each coding session joins as an agent with its own name. Once two are connected, they can ask each other questions."),
      h("ol", {},
        h("li", {}, "Add an agent, for example ", h("code", {}, "frontend"), ", and run its setup command in that project."),
        h("li", {}, "Do the same for ", h("code", {}, "backend"), " in the other project."),
        h("li", {}, "Tell one session to ask the other something.")),
      h("button", { class: "btn", onclick: addAgentDialog }, "Add agent")));
    return;
  }

  const scroller = h("div", { class: "seq-scroll" });
  if (state.threadId && state.messages.length) {
    scroller.append(sequence(state.messages));
  } else {
    scroller.append(h("div", { class: "seq-empty" },
      h("h2", {}, "Start a conversation"),
      h("p", {}, "Pick who to ask below. The message goes straight into their session if they are online, otherwise it waits for them.")));
  }
  refs.scroller = scroller;
  refs.sheet.replaceChildren(backButton, banner || "", scroller, composer());
  scroller.scrollTop = scroller.scrollHeight;
}

// Draws messages as a sequence diagram: one lifeline per participant, each message
// spanning from its sender to its recipient.
function sequence(messages) {
  const names = [];
  for (const m of messages) {
    if (!names.includes(m.from)) names.push(m.from);
    if (!names.includes(m.to)) names.push(m.to);
  }
  const column = (name) => names.indexOf(name) + 1;
  const byName = new Map(state.agents.map((a) => [a.name, a]));

  const grid = h("div", { class: "seq" });
  grid.style.setProperty("--cols", String(names.length));

  names.forEach((name, i) => {
    const agent = byName.get(name);
    const head = h("div", { class: `seq-head ${agent?.kind === "human" ? "person" : ""}` },
      agent ? lamp(agent.state) : "",
      h("span", { class: "label" }, name, agent ? "" : " (removed)"));
    head.style.gridColumn = String(i + 1);
    head.style.gridRow = "1";
    const life = h("div", { class: "lifeline" });
    life.style.gridColumn = String(i + 1);
    grid.append(head, life);
  });

  messages.forEach((m, i) => {
    const from = column(m.from);
    const to = column(m.to);
    const rightward = from < to;
    const sender = byName.get(m.from);
    const cls = ["step", rightward ? "rightward" : "leftward", `kind-${m.kind}`, sender?.kind === "human" ? "person" : ""].join(" ");
    const step = h("div", { class: cls },
      h("div", { class: "bubble" },
        h("div", { class: "meta" },
          h("span", { class: "sender" }, m.from),
          h("span", {}, kindWords[m.kind]),
          h("time", { datetime: new Date(m.created_at).toISOString() }, formatTime(m.created_at))),
        renderBody(m.body)),
      h("div", { class: "arrow", "aria-hidden": "true" }));
    step.style.gridColumn = `${Math.min(from, to)} / ${Math.max(from, to) + 1}`;
    step.style.setProperty("--span", String(Math.abs(to - from) + 1));
    step.style.gridRow = String(i + 2);
    grid.append(step);
  });
  return grid;
}

const kindWords = { ask: "asked", reply: "answered", note: "noted" };

// Understands fenced code blocks and inline code, nothing else. Agents write those constantly.
function renderBody(body) {
  const parts = [];
  const pieces = body.split(/```[^\n]*\n?/);
  pieces.forEach((piece, i) => {
    if (i % 2 === 1) {
      parts.push(h("pre", {}, h("code", {}, piece.replace(/\n$/, ""))));
    } else if (piece.trim()) {
      const p = h("p", {});
      piece.trim().split(/(`[^`\n]+`)/).forEach((chunk) => {
        if (chunk.startsWith("`") && chunk.endsWith("`") && chunk.length > 2) p.append(h("code", {}, chunk.slice(1, -1)));
        else if (chunk) p.append(chunk);
      });
      parts.push(p);
    }
  });
  return parts;
}

function composer() {
  const me = state.agents.find((a) => a.kind === "human" && a.user_id === state.me.id);
  const others = state.agents.filter((a) => a.id !== me?.id);
  const last = state.messages.at(-1);

  // In a conversation, the default is whoever spoke last, and an open question to me gets answered.
  let suggested = others.find((a) => a.kind === "agent")?.name;
  if (last) suggested = last.from === me?.name ? last.to : last.from;
  const unanswered = state.messages.findLast((m) => m.to === me?.name && m.kind === "ask"
    && !state.messages.some((r) => r.in_reply_to === m.id));

  const error = h("p", { class: "error", role: "alert" });
  const to = h("select", { "aria-label": "Send to" },
    others.map((a) => h("option", { value: a.name, selected: a.name === suggested }, a.name)));
  const body = h("textarea", {
    rows: 2, "aria-label": "Message", placeholder: "Write a message. Enter sends, Shift+Enter adds a line.",
    onkeydown: (event) => {
      if (event.key === "Enter" && !event.shiftKey && !event.isComposing) {
        event.preventDefault();
        form.requestSubmit();
      }
    },
  });
  const send = h("button", { class: "btn", type: "submit" }, "Send");

  const form = h("form", {
    class: "composer",
    onsubmit: async (event) => {
      event.preventDefault();
      const text = body.value.trim();
      if (!text) return;
      send.disabled = true;
      error.textContent = "";
      try {
        const payload = { to: to.value, body: text, thread_id: state.threadId || 0 };
        if (unanswered && unanswered.from === to.value) payload.in_reply_to = unanswered.id;
        const sent = await api("POST", `/api/workspaces/${state.workspaceId}/messages`, payload);
        body.value = "";
        if (!state.threadId) await openThread(sent.thread_id);
      } catch (err) {
        error.textContent = err.message;
      } finally {
        send.disabled = false;
        body.focus();
      }
    },
  },
    h("div", { class: "composer-to" }, "To", to),
    h("div", { class: "composer-row" }, body, send),
    error);
  return form;
}

// Live updates. A dropped socket reconnects with backoff and reloads, because
// anything that happened in the gap only exists on the server now.

function closeSocket() {
  clearTimeout(state.socketTimer);
  if (state.socket) {
    state.socket.onclose = null;
    state.socket.close();
    state.socket = null;
  }
}

function connectSocket() {
  closeSocket();
  if (!state.workspaceId) return;
  const scheme = location.protocol === "https:" ? "wss" : "ws";
  const socket = new WebSocket(`${scheme}://${location.host}/api/workspaces/${state.workspaceId}/ws`);
  state.socket = socket;
  socket.onopen = async () => {
    const wasDown = !state.connected;
    state.socketRetry = 0;
    state.connected = true;
    if (wasDown) await reloadWorkspace().catch(() => {});
  };
  socket.onmessage = (event) => handleEvent(JSON.parse(event.data));
  socket.onclose = async () => {
    state.connected = false;
    drawSheet();
    if (await lostAccess()) return;
    const delay = Math.min(30000, 1000 * 2 ** state.socketRetry) + Math.random() * 500;
    state.socketRetry++;
    state.socketTimer = setTimeout(connectSocket, delay);
  };
}

// A closed socket can mean this person was removed from the workspace. Reconnecting
// would then fail forever, so say what happened and move on to what is left.
async function lostAccess() {
  let list;
  try {
    list = await api("GET", "/api/workspaces");
  } catch {
    return !state.me; // logged out shows the sign in screen, anything else is a network problem
  }
  state.workspaces = list;
  if (list.some((w) => w.id === state.workspaceId)) return false;
  alertDialog("You no longer have access", "You were removed from that workspace.");
  state.workspaceId = list[0]?.id || null;
  state.agents = [];
  state.threads = [];
  state.threadId = null;
  state.messages = [];
  state.connected = true;
  if (state.workspaceId) await openWorkspace(state.workspaceId);
  else {
    drawRail();
    drawThreads();
    drawSheet();
  }
  return true;
}

async function reloadAgents() {
  state.agents = await api("GET", `/api/workspaces/${state.workspaceId}/agents`);
  drawRail();
  if (state.threadId) refreshHeads();
  else if (!state.messages.length) drawSheet();
}

function handleEvent(event) {
  if (event.type === "agents") {
    reloadAgents().catch(() => {});
    return;
  }
  if (event.type === "presence") {
    const agent = state.agents.find((a) => a.id === event.agent_id);
    if (agent) {
      agent.state = event.state;
      drawRail();
      if (state.threadId) refreshHeads();
    }
    return;
  }
  if (event.type === "message") {
    const m = event.message;
    if (m.thread_id === state.threadId && !state.messages.some((x) => x.id === m.id)) {
      state.messages.push(m);
      const atBottom = refs.scroller && refs.scroller.scrollHeight - refs.scroller.scrollTop - refs.scroller.clientHeight < 80;
      const draft = refs.sheet.querySelector("textarea")?.value;
      const hadFocus = document.activeElement?.tagName === "TEXTAREA";
      drawSheet();
      const box = refs.sheet.querySelector("textarea");
      if (box && draft) box.value = draft;
      if (box && hadFocus) box.focus();
      if (!atBottom && refs.scroller) refs.scroller.scrollTop = 0;
    }
    scheduleThreadReload();
  }
}

function refreshHeads() {
  if (!refs.scroller || !state.messages.length) return;
  const top = refs.scroller.scrollTop;
  refs.scroller.replaceChildren(sequence(state.messages));
  refs.scroller.scrollTop = top;
}

const scheduleThreadReload = debounce(async () => {
  state.threads = await api("GET", `/api/workspaces/${state.workspaceId}/threads`);
  drawThreadList();
}, 200);

function debounce(fn, wait) {
  let timer;
  return (...args) => {
    clearTimeout(timer);
    timer = setTimeout(() => fn(...args), wait);
  };
}

// Dialogs

function showDialog(content) {
  const dialog = h("dialog", {}, content);
  dialog.addEventListener("close", () => dialog.remove());
  document.body.append(dialog);
  dialog.showModal();
  return dialog;
}

function alertDialog(title, text) {
  const dialog = showDialog(h("div", { class: "dlg" },
    h("h2", {}, title), h("p", {}, text),
    h("div", { class: "dlg-actions" }, h("button", { class: "btn", onclick: () => dialog.close() }, "Close"))));
}

function commandBlock(text) {
  const copy = h("button", {
    type: "button",
    onclick: async () => {
      try {
        await navigator.clipboard.writeText(text);
        copy.textContent = "Copied";
      } catch {
        // No clipboard access on plain http. Selecting the text is the next best thing.
        const range = document.createRange();
        range.selectNodeContents(pre);
        const selection = getSelection();
        selection.removeAllRanges();
        selection.addRange(range);
        copy.textContent = "Selected";
      }
      setTimeout(() => { copy.textContent = "Copy"; }, 1500);
    },
  }, "Copy");
  const pre = h("pre", {}, text);
  return h("div", { class: "cmd" }, pre, copy);
}

function setupSteps(token) {
  const isHttps = location.protocol === "https:";
  return h("div", { class: "dlg" },
    h("p", { class: "warn" }, "This token is shown only once. Copy the commands now. Anyone holding it can talk as this agent."),
    !isHttps && !/^(localhost|127\.)/.test(location.hostname) && h("p", { class: "warn" },
      "This hub is on plain http, so the token travels unencrypted. Put it behind https before using it over a network."),
    h("ol", { class: "steps" },
      h("li", {}, "Install the client, once per machine.",
        commandBlock(`curl -fsSL ${origin}/install.sh | sh`),
        h("details", {}, h("summary", {}, "On Windows"), commandBlock(`irm ${origin}/install.ps1 | iex`))),
      h("li", {}, "Run this in the project folder.",
        commandBlock(`agentic-chat setup --url ${origin} --token ${token}`)),
      h("li", {}, "Start your session there.",
        commandBlock("claude --dangerously-load-development-channels server:agentic-chat"),
        h("p", { class: "note" }, "This flag lets messages appear in the session by themselves. Claude Code asks you to confirm it when it starts."))),
    h("details", {},
      h("summary", {}, "Without that flag"),
      h("p", { class: "note" }, "Add --mode hook to the setup command and start with plain claude. Messages then arrive through background hooks instead."),
      commandBlock(`agentic-chat setup --url ${origin} --token ${token} --mode hook`)),
    h("details", {},
      h("summary", {}, "Other MCP clients, nothing to install"),
      h("p", { class: "note" }, "Connects directly over HTTP. Messages do not appear by themselves, the session checks its inbox when it needs to."),
      commandBlock(`claude mcp add --transport http agentic-chat ${origin}/mcp --header "Authorization: Bearer ${token}"`)));
}

function addAgentDialog() {
  const error = h("p", { class: "error", role: "alert" });
  const name = h("input", { required: true, maxlength: 32, autocapitalize: "none", spellcheck: "false", placeholder: "backend" });
  const description = h("input", { maxlength: 200, placeholder: "REST API and database" });
  const submit = h("button", { class: "btn", type: "submit" }, "Add agent");
  const form = h("form", {
    class: "dlg",
    onsubmit: async (event) => {
      event.preventDefault();
      submit.disabled = true;
      try {
        const created = await api("POST", `/api/workspaces/${state.workspaceId}/agents`, { name: name.value, description: description.value });
        await reloadWorkspace();
        form.replaceWith(h("div", {},
          h("div", { class: "dlg" }, h("h2", {}, `Connect ${created.agent.name}`)),
          setupSteps(created.token),
          h("div", { class: "dlg" }, h("div", { class: "dlg-actions" }, h("button", { class: "btn", onclick: () => dialog.close() }, "Done")))));
      } catch (err) {
        error.textContent = err.message;
        submit.disabled = false;
      }
    },
  },
    h("h2", {}, "Add agent"),
    h("label", {}, "Name", h("span", { class: "hint" }, "How other sessions will address it. Lowercase letters, digits, - and _"), name),
    h("label", {}, "What it works on", h("span", { class: "hint" }, "Optional. Other sessions read this to decide who to ask."), description),
    error,
    h("div", { class: "dlg-actions" },
      h("button", { class: "btn plain", type: "button", onclick: () => dialog.close() }, "Cancel"),
      submit));
  const dialog = showDialog(form);
  name.focus();
}

function agentDialog(agent) {
  const error = h("p", { class: "error", role: "alert" });
  const seen = agent.last_seen_at ? `Last active ${formatTime(agent.last_seen_at)}.` : "Has not connected yet.";
  const content = h("div", { class: "dlg" },
    h("h2", {}, agent.name),
    agent.description && h("p", {}, agent.description),
    h("p", { class: "note" }, `${stateWords[agent.state][0].toUpperCase()}${stateWords[agent.state].slice(1)}. ${seen}`),
    error,
    h("div", { class: "dlg-actions" },
      h("button", {
        class: "btn danger push",
        onclick: (event) => {
          // Ask inside the dialog first, this one cannot be undone.
          event.target.replaceWith(h("span", { class: "push" },
            h("button", {
              class: "btn danger",
              onclick: async () => {
                try {
                  await api("DELETE", `/api/workspaces/${state.workspaceId}/agents/${agent.id}`);
                  dialog.close();
                  await reloadWorkspace();
                } catch (err) { error.textContent = err.message; }
              },
            }, "Yes, remove it")));
        },
      }, "Remove agent"),
      h("button", {
        class: "btn plain",
        onclick: async () => {
          try {
            const rotated = await api("POST", `/api/workspaces/${state.workspaceId}/agents/${agent.id}/rotate`);
            content.replaceChildren(h("h2", {}, `New token for ${agent.name}`),
              h("p", { class: "note" }, "The old token stopped working. Run the setup command again in the project."),
              setupSteps(rotated.token),
              h("div", { class: "dlg-actions" }, h("button", { class: "btn", onclick: () => dialog.close() }, "Done")));
          } catch (err) { error.textContent = err.message; }
        },
      }, "Replace token"),
      h("button", { class: "btn", onclick: () => dialog.close() }, "Close")));
  const dialog = showDialog(content);
}

async function peopleDialog() {
  const workspace = myWorkspace();
  const isOwner = workspace?.role === "owner";
  const members = await api("GET", `/api/workspaces/${state.workspaceId}/members`);
  const error = h("p", { class: "error", role: "alert" });
  const inviteHost = h("div", {});
  const openInvites = h("div", { class: "row-list" });

  const drawInvites = async () => {
    const invites = await api("GET", `/api/workspaces/${state.workspaceId}/invites`);
    openInvites.replaceChildren(...(invites.length ? [h("p", { class: "note" }, "Open invites")] : []),
      ...invites.map((invite) => h("div", {},
        h("span", {}, `Used ${invite.uses} of ${invite.max_uses}, ends ${new Date(invite.expires_at).toLocaleDateString()}`),
        h("button", {
          class: "btn plain",
          onclick: async () => {
            try {
              await api("DELETE", `/api/workspaces/${state.workspaceId}/invites/${invite.id}`);
              inviteHost.replaceChildren();
              await drawInvites();
            } catch (err) { error.textContent = err.message; }
          },
        }, "Cancel invite"))));
  };

  const rows = members.map((m) => h("div", {},
    h("span", {}, m.username, m.role === "owner" ? " (owner)" : ""),
    (isOwner || m.user_id === state.me.id) && h("button", {
      class: "btn plain",
      onclick: async () => {
        try {
          await api("DELETE", `/api/workspaces/${state.workspaceId}/members/${m.user_id}`);
          dialog.close();
          if (m.user_id === state.me.id) location.reload();
          else await reloadWorkspace();
        } catch (err) { error.textContent = err.message; }
      },
    }, m.user_id === state.me.id ? "Leave" : "Remove")));

  const content = h("div", { class: "dlg" },
    h("h2", {}, `People in ${workspace?.name}`),
    h("p", { class: "note" }, "Everyone here can read every conversation in this workspace and talk to its agents."),
    h("div", { class: "row-list" }, rows),
    error,
    isOwner && openInvites,
    isOwner && inviteHost,
    h("div", { class: "dlg-actions" },
      isOwner && h("button", {
        class: "btn plain push",
        onclick: async () => {
          try {
            const created = await api("POST", `/api/workspaces/${state.workspaceId}/invites`, { max_uses: 1, days: 7 });
            inviteHost.replaceChildren(
              h("p", {}, "Send this link to one person. It works once, for 7 days."),
              commandBlock(`${origin}/#/invite/${created.code}`));
            await drawInvites();
          } catch (err) { error.textContent = err.message; }
        },
      }, "Create invite link"),
      h("button", { class: "btn", onclick: () => dialog.close() }, "Close")));
  const dialog = showDialog(content);
  if (isOwner) drawInvites().catch((err) => { error.textContent = err.message; });
}

function accountDialog() {
  const error = h("p", { class: "error", role: "alert" });
  const current = h("input", { type: "password", required: true, autocomplete: "current-password" });
  const next = h("input", { type: "password", required: true, minlength: 10, autocomplete: "new-password" });
  const submit = h("button", { class: "btn", type: "submit" }, "Change password");
  const dialog = showDialog(h("form", {
    class: "dlg",
    onsubmit: async (event) => {
      event.preventDefault();
      error.textContent = "";
      try {
        await api("POST", "/api/me/password", { current: current.value, new: next.value });
        dialog.close();
        alertDialog("Password changed", "You were logged out everywhere else.");
      } catch (err) { error.textContent = err.message; }
    },
  },
    h("h2", {}, `Account: ${state.me.username}`),
    h("label", {}, "Current password", current),
    h("label", {}, "New password", h("span", { class: "hint" }, "At least 10 characters"), next),
    error,
    h("div", { class: "dlg-actions" },
      h("button", { class: "btn plain", type: "button", onclick: () => dialog.close() }, "Close"),
      submit)));
}

function createWorkspaceDialog() {
  const error = h("p", { class: "error", role: "alert" });
  const name = h("input", { required: true, maxlength: 64 });
  const dialog = showDialog(h("form", {
    class: "dlg",
    onsubmit: async (event) => {
      event.preventDefault();
      try {
        const created = await api("POST", "/api/workspaces", { name: name.value });
        state.workspaces = await api("GET", "/api/workspaces");
        dialog.close();
        await openWorkspace(created.id);
      } catch (err) { error.textContent = err.message; }
    },
  },
    h("h2", {}, "New workspace"),
    h("p", { class: "note" }, "Agents only see others in the same workspace. Use one per team or per product."),
    h("label", {}, "Name", name),
    error,
    h("div", { class: "dlg-actions" },
      h("button", { class: "btn plain", type: "button", onclick: () => { dialog.close(); drawRail(); } }, "Cancel"),
      h("button", { class: "btn", type: "submit" }, "Create workspace"))));
  name.focus();
}

async function logout() {
  closeSocket();
  await api("POST", "/api/auth/logout", {}).catch(() => {});
  location.reload();
}

// Boot

(async function boot() {
  try {
    state.config = await api("GET", "/api/config");
  } catch (err) {
    app.replaceChildren(h("p", { class: "empty" }, err.message));
    return;
  }
  try {
    await start();
  } catch (err) {
    if (err.status === 401) renderAuth();
    else app.replaceChildren(h("p", { class: "empty" }, err.message));
  }
})();
