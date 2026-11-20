// threadgroupsctl.js: moving a repo to another group from its right-click
// menu ("Move to group ›"), and a group's "Add a repo here…". What the
// targets, names and the question mean is threadgroups.js; the "+" dialog
// is threadnewrepo.js. A move restarts the repo's running sessions (the
// daemon's repomove.go), so the person is asked first.
(() => {
  "use strict";
  const G = window.ccmuxThreadGroups;
  const enc = encodeURIComponent;

  // create wires it to the page: d.call is its fetch, d.windows() the
  // groups, d.changed() reads the lists again, d.confirm and d.prompt ask
  // the person, d.fail(text) shows what went wrong, d.openTasks(win, r) is
  // how many unfinished tasks r has in win's queue, and d.addRepo(group)
  // opens the "+" dialog for a group.
  function create(d) {
    // repoItems is a repo's own menu entry: its groups, then a new one.
    function repoItems(r) {
      const targets = G.moveTargets(d.windows(), r.id).map((t) => [t.label, () => move(r, t.group)]);
      return [["Move to group", null, [...targets, ["New group…", () => moveToNew(r)]]]];
    }

    function moveToNew(r) {
      const typed = d.prompt(`Name of the new group for ${r.name}:`);
      if (typed === null || typed === undefined) return;
      const n = G.groupName(typed, d.windows());
      if (n.error) { d.fail(n.error); return; }
      move(r, n.name);
    }

    // move asks, then moves. A move to the group it is in is none.
    async function move(r, group) {
      const here = G.windowOf(d.windows(), r.id);
      const from = here ? here.name : "";
      if (from.toLowerCase() === String(group).toLowerCase()) return;
      const open = { tasks: here ? d.openTasks(here, r) : 0, threads: (r.threads || []).length };
      if (!d.confirm(G.moveQuestion(r, from, group, open))) return;
      try {
        await d.call("PUT", `/v1/workspaces/${enc(r.id)}/group`, { group });
      } catch (e) {
        d.fail(`Moving ${r.name} failed: ${e.message}`);
        return;
      }
      d.changed();
    }

    // groupItems is a group's own entry: add a repo to it.
    const groupItems = (g) => (g && g.id ? [["Add a repo here…", () => d.addRepo(g.name)]] : []);

    return { repoItems, groupItems, move };
  }

  window.ccmuxThreadGroupsCtl = { create };
})();
