// The control UI's one script. Pages read and work without it: it adds
// each time in the reader's own zone as the time's title, beside the UTC
// the page prints, keeps the top bar saying how long ago the page loaded,
// opens the sidebar's menu on a wide screen, and adds a copy button
// beside each command a reader runs next.
const local = new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short" });
for (const el of document.querySelectorAll("time[datetime]")) {
  const at = new Date(el.dateTime);
  if (!Number.isNaN(at.getTime())) {
    el.title = local.format(at) + " in your time zone";
  }
}

// The clipboard exists only on a secure origin or loopback.
if (navigator.clipboard) {
  for (const code of document.querySelectorAll("code.cmd")) {
    const button = document.createElement("button");
    button.type = "button";
    button.className = "copy outline";
    button.textContent = "Copy";
    button.setAttribute("aria-label", "Copy the command " + code.textContent);
    button.addEventListener("click", async () => {
      try {
        await navigator.clipboard.writeText(code.textContent);
        button.textContent = "Copied";
      } catch {
        button.textContent = "Copy failed";
      }
    });
    code.after(" ", button);
  }
}

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
