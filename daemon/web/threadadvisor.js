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
  // restart" shows). A runner from before the picker started with none of
  // its own, and runs what the settings say, which only it can read; one
  // that has said nothing yet (a new session) is not known to run anything,
  // so nothing is pending (said, below).
  function advisorView(session, info, model) {
    const now = said(info);
    const wanted = (session && session.advisor) || "";
    return {
      options: optionsFor(now.settings, model, wanted), wanted,
      wantedName: nameOf(wanted || now.settings) || now.none,
      runningName: nameOf(now.model) || now.none,
      pending: now.started !== null && now.started !== wanted,
    };
  }

  // said is what the runner says of its advisor (session.info advisor): the
  // one it runs, the one it was started with ("" for none of its own), what
  // the settings name (null when it has not said), and the word for none.
  // A runner that says other things but no advisor is from before the
  // picker, and was started with none of its own. One that has said nothing
  // yet (its stream not up, a new session) is not known to run anything:
  // started is null, and nothing is pending, as with a model.
  function said(info) {
    const now = info && info.advisor;
    const spoke = !!info && !!(info.model || info.claudeCode);
    if (!now) return { model: "", started: spoke ? "" : null, settings: null, none: "the settings' advisor" };
    return { model: now.model || "", started: now.from === "session" ? now.model : "", settings: now.settings || "", none: "no advisor" };
  }

  // optionsFor lists the settings' advisor first, then the ones that rank at
  // or above model, and the row's own pick when it is not among them.
  function optionsFor(settings, model, wanted) {
    const first = { value: "", label: settings === null ? "Settings" : settings ? `Settings (${nameOf(settings)})` : "Settings (none)" };
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
