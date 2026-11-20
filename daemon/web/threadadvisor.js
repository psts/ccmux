// threadadvisor.js: the thread page's Advisor picker, worked out from the
// session's row (the advisor it asks for, "" for the person's settings)
// and what its runner says it runs with (session.info advisor:
// claudeserve/sessionself.mjs advisorOf). Pure, so threadadvisor_test.js
// runs it without a browser; threads.js owns the DOM.
//
// Like a model, an advisor applies when the session next starts: a running
// Claude Code keeps the advisor it started with.
(function (root) {
  "use strict";

  // ADVISORS are the ones offered, by Claude Code's aliases (each runs the
  // newest of its family), strongest first.
  const ADVISORS = [{ value: "fable", name: "Fable" }, { value: "opus", name: "Opus" }, { value: "sonnet", name: "Sonnet" }];

  // An advisor must rank at or above the session's model, or Claude Code
  // runs with none (code.claude.com/docs/en/advisor). The finer rules (an
  // Opus 4.6 takes a Sonnet 5 but not a Sonnet 4.6) are left to it: these
  // are the families of today's models.
  const rank = { haiku: 1, sonnet: 2, opus: 3, fable: 4 };
  const family = (id) => (String(id || "").toLowerCase().match(/fable|opus|sonnet|haiku/) || [""])[0];
  const nameOf = (id) => {
    const a = ADVISORS.find((x) => x.value === family(id));
    return a ? a.name : String(id || "");
  };

  // advisorView is the picker for a session on model (the one it runs on
  // after its next start): the options, the one its row asks for (wanted),
  // the names to show, and whether the row asks for another advisor than
  // the runner started with (pending, which is when "applies on the next
  // restart" shows). known is false while the runner has not said (an older
  // runner, or none up): the page then shows no picker.
  function advisorView(session, info, model) {
    const now = info && info.advisor;
    if (!now) return { known: false, options: [], wanted: "", wantedName: "", runningName: "", pending: false };
    const settings = now.settings || "";
    const wanted = (session && session.advisor) || "";
    const startedWith = now.from === "session" ? now.model : "";
    return {
      known: true, options: optionsFor(settings, model, wanted), wanted,
      wantedName: nameOf(wanted || settings) || "no advisor",
      runningName: nameOf(now.model) || "no advisor",
      pending: startedWith !== wanted,
    };
  }

  // optionsFor lists the settings' advisor first, then the ones that rank at
  // or above model, and the row's own pick when it is not among them.
  function optionsFor(settings, model, wanted) {
    const first = { value: "", label: settings ? `Settings (${nameOf(settings)})` : "Settings (none)" };
    const floor = rank[family(model)] || 0;
    const options = [first].concat(ADVISORS.filter((a) => rank[a.value] >= floor).map((a) => ({ value: a.value, label: a.name })));
    if (!options.some((o) => o.value === wanted)) options.push({ value: wanted, label: nameOf(wanted) });
    return options;
  }

  // restartBar is what the bar says when a model (mv, threadmodel.js) or an
  // advisor (av) was picked that applies on the next restart: what applies,
  // and what the session keeps until then.
  function restartBar(mv, av, modelPending, advisorPending) {
    const what = [modelPending && mv.wantedName, advisorPending && `Advisor ${av.wantedName}`].filter(Boolean).join(" and ");
    const keeps = modelPending ? `keeps running on ${mv.runningName || "its current model"}` : `keeps ${av.runningName} as its advisor`;
    return { pending: modelPending || advisorPending, what, keeps };
  }

  const api = { ADVISORS, advisorView, restartBar };
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  else root.ccmuxThreadAdvisor = api;
})(typeof window !== "undefined" ? window : this);
