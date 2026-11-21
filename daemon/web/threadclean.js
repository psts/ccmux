// threadclean.js: the question the thread page's composer asks before a
// message on a cold prompt cache (threadgauge.js coldAsk): start clean, go
// on in the same conversation, or not send yet. A <dialog> in threads.html
// whose buttons close it with their value; Escape closes it with none.
(function (root) {
  "use strict";

  // pickOf is what a closed dialog's value means: "clean", "continue", or
  // null for not sending.
  const pickOf = (value) => (value === "clean" || value === "continue" ? value : null);

  // create wires it to the page: d.dialog() is the <dialog>, d.say(text)
  // puts the question's words in it. ask(text) answers with pickOf's value;
  // a question while one is open answers null at once.
  function create(d) {
    let open = false;

    function ask(text) {
      const box = d.dialog();
      if (open || !box || typeof box.showModal !== "function") return Promise.resolve(open ? null : "continue");
      open = true;
      d.say(text);
      box.returnValue = "";
      return new Promise((resolve) => {
        box.addEventListener("close", () => { open = false; resolve(pickOf(box.returnValue)); }, { once: true });
        box.showModal();
      });
    }

    return { ask };
  }

  const api = { create, pickOf };
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  else root.ccmuxThreadClean = api;
})(typeof window !== "undefined" ? window : this);
