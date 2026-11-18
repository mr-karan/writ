// The control UI's one script. Pages read and work without it: it adds
// each time in the reader's own zone as the time's title, keeps the
// footer saying how long ago the page loaded, opens the sidebar's menu on
// a wide screen, adds a copy button beside each command a reader runs
// next, adds a button that opens or folds all the changes of a plan, and
// closes an open picker on Escape or a click outside it.
const local = new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short" });
for (const el of document.querySelectorAll("time[datetime]")) {
  const at = new Date(el.dateTime);
  if (!Number.isNaN(at.getTime())) {
    el.title = local.format(at) + " in your time zone";
  }
}

// icon is an icon of the page's sprite.
const svg = "http://www.w3.org/2000/svg";
const icon = (name) => {
  const el = document.createElementNS(svg, "svg");
  el.setAttribute("class", "i");
  el.setAttribute("aria-hidden", "true");
  el.setAttribute("focusable", "false");
  const use = document.createElementNS(svg, "use");
  use.setAttribute("href", "#i-" + name);
  el.append(use);
  return el;
};

// The clipboard exists only on a secure origin or loopback.
if (navigator.clipboard) {
  for (const code of document.querySelectorAll("code.cmd")) {
    const button = document.createElement("button");
    button.type = "button";
    button.className = "ghost small icon-only";
    const label = "Copy the command " + code.textContent;
    button.setAttribute("aria-label", label);
    button.append(icon("copy"));
    button.addEventListener("click", async () => {
      try {
        await navigator.clipboard.writeText(code.textContent);
        button.replaceChildren(icon("check"));
        button.setAttribute("aria-label", "Copied");
      } catch {
        button.setAttribute("aria-label", "Copy failed");
      }
      setTimeout(() => {
        button.replaceChildren(icon("copy"));
        button.setAttribute("aria-label", label);
      }, 2000);
    });
    code.after(" ", button);
  }
}

// A plan's changes get one button that opens or folds all of them, at the
// end of the bar of their card.
const card = document.getElementById("h-changes")?.closest("section");
const end = card?.querySelector(".bar .end");
const changes = card ? [...card.querySelectorAll("details.change")] : [];
if (end && changes.length > 1) {
  const button = document.createElement("button");
  button.type = "button";
  button.className = "ghost";
  const name = () => {
    button.textContent = changes.every((d) => d.open) ? "Fold all" : "Open all";
  };
  button.addEventListener("click", () => {
    const open = !changes.every((d) => d.open);
    for (const d of changes) {
      d.open = open;
    }
  });
  for (const d of changes) {
    d.addEventListener("toggle", name);
  }
  name();
  end.append(button);
}

// A picker is a details element: it closes on Escape, giving the focus
// back to its handle, and on a click anywhere outside it.
const picks = [...document.querySelectorAll("details.pick")];
document.addEventListener("click", (e) => {
  for (const p of picks) {
    if (p.open && !p.contains(e.target)) {
      p.open = false;
    }
  }
});
document.addEventListener("keydown", (e) => {
  if (e.key !== "Escape") {
    return;
  }
  for (const p of picks) {
    if (p.open) {
      p.open = false;
      p.querySelector("summary").focus();
    }
  }
});

const loaded = document.querySelector("time[data-loaded]");
if (loaded) {
  // Counted from this page's load in this browser, so a clock that
  // differs from the server's does not shift it.
  const tick = () => {
    const min = Math.floor(performance.now() / 60000);
    loaded.textContent = min < 1 ? "just now" : min + " min ago";
  };
  tick();
  setInterval(tick, 30000);
}

// CSS shows the menu on a wide screen; opening it also tells a screen
// reader that it is expanded.
const menu = document.querySelector("details.menu");
const wide = matchMedia("(min-width: 64rem)");
const sync = () => {
  if (menu && wide.matches) {
    menu.open = true;
  }
};
sync();
wide.addEventListener("change", sync);
