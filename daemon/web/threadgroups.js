// threadgroups.js: what moving a repo between groups, and adding one, mean
// on the thread page. A group is a shared window (GET /v1/windows); a repo
// in none is listed under "Other". Moving is PUT /v1/workspaces/{id}/group
// with the target's name ("" for none), and a name no window has yet makes
// that window: a group exists while something is in it. Nothing here
// fetches or draws; threadgroupsctl.js and threadnewrepo.js do.
(() => {
  "use strict";
  const OTHER = "Other";
  const byName = (a, b) => a.name.localeCompare(b.name);
  const same = (a, b) => String(a || "").toLowerCase() === String(b || "").toLowerCase();

  // windowOf is the window a workspace is in, null for none.
  function windowOf(windows, wsId) {
    return (windows || []).find((w) => (w.workspaceIds || []).includes(wsId)) || null;
  }

  // moveTargets are where a repo can go, as {label, group}: every other
  // group by name, then "No group" for one that is in a group now.
  function moveTargets(windows, wsId) {
    const here = windowOf(windows, wsId);
    const list = (windows || []).filter((w) => w.name && (!here || w.id !== here.id)).sort(byName).map((w) => ({ label: w.name, group: w.name }));
    if (here) list.push({ label: "No group (" + OTHER + ")", group: "" });
    return list;
  }

  // groupName is what a typed new group's name means: {name} for a new
  // group, {name, existing: true} for one that is there already in another
  // case (the daemon matches names that way), {error} when it cannot be one.
  function groupName(typed, windows) {
    const name = String(typed || "").trim();
    if (!name) return { error: "A group needs a name." };
    if (same(name, OTHER)) return { error: `"${OTHER}" is where repos with no group are listed. Pick another name.` };
    const there = (windows || []).find((w) => same(w.name, name));
    return there ? { name: there.name, existing: true } : { name };
  }

  // moveQuestion is what the person is asked before a move: where from and
  // to, that its running sessions restart, and what stays behind.
  function moveQuestion(repo, fromName, toName, open) {
    const from = fromName || "no group", to = toName || "no group";
    const lines = [`Move ${repo.name} from ${from} to ${to}?`,
      `Its running sessions restart to pick up ${toName ? to + "'s" : "their new"} sessions and shared folder. A terminal session that is busy gets a note instead.`];
    const tasks = (open && open.tasks) || 0, threads = (open && open.threads) || 0;
    if (tasks) lines.push(`${tasks} unfinished task${tasks === 1 ? "" : "s"} stay${tasks === 1 ? "s" : ""} in ${from}'s queue.`);
    if (threads) lines.push(`${threads} open thread${threads === 1 ? "" : "s"} can no longer reach helpers left in ${from}.`);
    return lines.join("\n\n");
  }

  // groupChoices are the "+" dialog's group picker: no group, every group
  // by name, and a new one; picked is the one to show first.
  function groupChoices(windows, picked) {
    const names = (windows || []).filter((w) => w.name).sort(byName).map((w) => w.name);
    const value = names.find((n) => same(n, picked)) || "";
    return { value, options: [{ value: "", label: "No group" }, ...names.map((n) => ({ value: n, label: n })), { value: "\n", label: "New group…" }] };
  }

  // NEW is the picker value that asks for a typed name.
  const NEW = "\n";

  // createBody is the POST /v1/workspaces body for a folder from the
  // projects list: a plain repo session (its first pane a shell), in group.
  function createBody(project, group) {
    const body = { name: project.name, repoPath: project.path, createdBy: "web", startupCommand: "" };
    if (group) body.group = group;
    return body;
  }

  const api = { OTHER, NEW, windowOf, moveTargets, groupName, moveQuestion, groupChoices, createBody };
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  if (typeof window !== "undefined") window.ccmuxThreadGroups = api;
})();
