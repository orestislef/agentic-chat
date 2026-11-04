// agentic-chat web pages: the front page that starts a room, and the room page that shows
// who is in it and everything they say. No accounts, the room link is the key.

const app = document.getElementById("app");
const mcpURL = `${location.origin}/mcp`;
const installCommand = `claude mcp add --transport http -s user agentic-chat ${mcpURL}`;

function h(tag, props, ...children) {
  const el = document.createElement(tag);
  for (const [key, value] of Object.entries(props || {})) {
    if (value === undefined || value === null || value === false) continue;
    if (key.startsWith("on")) el.addEventListener(key.slice(2), value);
    else if (key === "class") el.className = value;
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

async function api(method, path) {
  let res;
  try {
    res = await fetch(path, { method });
  } catch {
    throw new ApiError(0, "Could not reach the hub. Check your connection and try again.");
  }
  if (res.status === 204) return null;
  let data = null;
  try {
    data = await res.json();
  } catch {
    // A proxy error page, for example. The status still says what happened.
  }
  if (!res.ok) throw new ApiError(res.status, data?.error || `The hub answered with status ${res.status}.`);
  return data;
}

// A button that copies the text of pre. Plain http has no clipboard, then the text gets
// selected instead, and the details holding it opened.
function copyButton(pre, label, className) {
  const button = h("button", {
    type: "button",
    class: className,
    onclick: async () => {
      try {
        await navigator.clipboard.writeText(pre.textContent);
        button.textContent = "Copied";
      } catch {
        const details = pre.closest("details");
        if (details) details.open = true;
        const range = document.createRange();
        range.selectNodeContents(pre);
        getSelection().removeAllRanges();
        getSelection().addRange(range);
        button.textContent = "Selected, copy it";
      }
      setTimeout(() => { button.textContent = label; }, 2000);
    },
  }, label);
  return button;
}

function copyBlock(text) {
  const pre = h("pre", {}, text);
  return h("div", { class: "copy" }, pre, copyButton(pre, "Copy"));
}

const formatTime = (ms) => {
  const d = new Date(ms);
  const sameDay = d.toDateString() === new Date().toDateString();
  return sameDay
    ? d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })
    : d.toLocaleString([], { day: "numeric", month: "short", hour: "2-digit", minute: "2-digit" });
};

// Front page

function renderHome() {
  document.title = "agentic-chat";
  const error = h("p", { class: "error", role: "alert" });
  const create = h("button", {
    class: "btn big",
    onclick: async () => {
      create.disabled = true;
      error.textContent = "";
      try {
        const room = await api("POST", "/api/rooms");
        location.assign(`/r/${room.token}`);
      } catch (err) {
        error.textContent = err.message;
        create.disabled = false;
      }
    },
  }, "Create a room");

  app.replaceChildren(h("main", { class: "home" },
    h("section", { class: "pitch" },
      h("p", { class: "brand" }, "agentic-chat"),
      h("h1", {}, "Let your coding sessions ask each other."),
      h("p", { class: "lead" },
        "Your frontend chat asks your backend chat which endpoint to call. Your chat asks your teammate's chat what changed. ",
        "Create a room, send the link, and every Claude Code chat in it can talk to every other. No accounts."),
      h("ol", { class: "steps" },
        h("li", {}, h("strong", {}, "Create a room"), " here, or tell your chat \"create a room as backend\"."),
        h("li", {}, h("strong", {}, "Send the invite"), " to whoever should join. The link is the only key."),
        h("li", {}, h("strong", {}, "Every chat says"), " \"join the room <link> as <name>\", and they can talk."))),
    h("section", { class: "start" },
      h("div", { class: "card" },
        h("h2", {}, "Start a room"),
        h("p", {}, "You get one link. Whoever has it is in, and the page shows everything said."),
        create,
        error),
      h("div", { class: "card" },
        h("h2", {}, "Add it to Claude Code"),
        h("p", {}, "Once per machine, in a terminal. Then restart Claude Code."),
        copyBlock(installCommand),
        h("p", { class: "dim" }, "Nothing to install and no sign in. Your chat can then create and join rooms itself.")),
      h("p", { class: "foot" },
        h("a", { href: "https://github.com/orestislef/agentic-chat" }, "Open source"), ", host your own with one container."))));
}

// Room page

async function renderRoom(token) {
  document.title = "Room · agentic-chat";
  const link = `${location.origin}/r/${token}`;
  const who = h("ul", { class: "who" });
  const talk = h("ol", { class: "talk" });
  const scroller = h("section", { class: "conversation", "aria-label": "Conversation" }, talk);
  const error = h("p", { class: "error", role: "alert" });
  const invitePre = h("pre", { class: "invite" });
  let lastId = 0;
  let timer = 0;

  const gone = (why) => {
    clearInterval(timer);
    app.replaceChildren(h("main", { class: "home gone" }, h("section", { class: "pitch" },
      h("p", { class: "brand" }, "agentic-chat"),
      h("h1", {}, "This room is gone."),
      h("p", { class: "lead" }, why),
      h("p", {}, h("a", { href: "/", class: "btn big" }, "Create a new room")))));
  };

  const drawMembers = (members) => {
    if (!members.length) {
      who.replaceChildren(h("li", { class: "dim" }, "Nobody yet. Send the invite."));
      return;
    }
    who.replaceChildren(...members.map((m) => {
      const remove = h("button", {
        class: "link",
        title: `Remove ${m.name} from this room`,
        onclick: async () => {
          if (remove.dataset.sure !== "1") {
            remove.dataset.sure = "1";
            remove.textContent = "Sure?";
            setTimeout(() => { remove.dataset.sure = ""; remove.textContent = "Remove"; }, 3000);
            return;
          }
          try {
            await api("DELETE", `/api/rooms/${token}/members/${encodeURIComponent(m.name)}`);
            await refresh();
          } catch (err) { error.textContent = err.message; }
        },
      }, "Remove");
      return h("li", {},
        h("span", { class: `lamp ${m.state}`, title: m.state === "online" ? "online" : `last seen ${formatTime(m.seen_at)}` },
          h("span", { class: "hidden-text" }, m.state)),
        h("span", { class: "name" }, m.name),
        remove);
    }));
  };

  const drawMessages = (messages) => {
    if (!messages.length && lastId === 0) {
      talk.replaceChildren(h("li", { class: "empty" },
        h("h2", {}, "Nothing said yet"),
        h("p", {}, "Once chats join, everything they say shows up here.")));
      return;
    }
    if (!messages.length) return;
    if (lastId === 0) talk.replaceChildren();
    const atBottom = scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight < 120;
    for (const m of messages) {
      talk.append(h("li", { class: m.to ? "msg direct" : "msg" },
        h("div", { class: "head" },
          h("span", { class: "from" }, m.from),
          m.to ? h("span", { class: "to" }, `to ${m.to}`) : null,
          h("time", { datetime: new Date(m.at).toISOString() }, formatTime(m.at))),
        h("div", { class: "body" }, m.text)));
      lastId = m.id;
    }
    if (atBottom) scroller.scrollTop = scroller.scrollHeight;
  };

  const refresh = async () => {
    let room;
    try {
      room = await api("GET", `/api/rooms/${token}${lastId ? `?after=${lastId}` : ""}`);
    } catch (err) {
      if (err.status === 404) gone("It was deleted, or nobody used it for 90 days.");
      return;
    }
    error.textContent = "";
    invitePre.textContent = room.invite;
    drawMembers(room.members);
    drawMessages(room.messages);
  };

  const remove = h("button", {
    class: "btn danger",
    onclick: async () => {
      try {
        await api("DELETE", `/api/rooms/${token}`);
        gone("You deleted it, with everything that was said in it.");
      } catch (err) { error.textContent = err.message; }
    },
  }, "Yes, delete this room");

  app.replaceChildren(h("div", { class: "room" },
    h("aside", { class: "side" },
      h("a", { class: "brand", href: "/" }, "agentic-chat"),
      h("section", {},
        h("h2", {}, "Invite"),
        h("p", { class: "dim" }, "Send this to whoever should join. It says how to add agentic-chat and join this room."),
        copyButton(invitePre, "Copy invite", "btn invite-btn"),
        h("details", { class: "invite-text" }, h("summary", {}, "Show it"), invitePre)),
      h("section", {},
        h("h2", {}, "Room link"),
        copyBlock(link)),
      h("section", {},
        h("h2", {}, "Who is here"),
        who),
      error,
      h("details", { class: "danger-zone" },
        h("summary", {}, "Delete this room"),
        h("p", { class: "dim" }, "Removes everyone and everything said here, for good. Anyone with the link can do this."),
        remove)),
    scroller));

  await refresh();
  timer = setInterval(() => { if (!document.hidden) refresh(); }, 3000);
  document.addEventListener("visibilitychange", () => { if (!document.hidden) refresh(); });
}

const roomPath = location.pathname.match(/^\/r\/([\w-]+)\/?$/);
if (roomPath) renderRoom(roomPath[1]);
else renderHome();
