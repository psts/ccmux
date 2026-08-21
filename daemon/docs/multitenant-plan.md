# Multi-tenant plan (per-user windows → shared windows)

> **Status v2 (2026-08-21, same day): superseded in part.** The per-user-views model
> below shipped as v0.1.31 and immediately failed its first real test: a two-person
> team does not want two private arrangements — they want ONE shared arrangement and
> personal open/closed. See the **v2: shared windows** section at the bottom, which
> replaces the "views are per-login rows" decision while keeping the identity layer,
> the owner attribution, the guard shape, and most of the plumbing. Rows in `views`
> become the migration source and are then retired.
>
> **Status v1 (2026-08-21): built, unit-tested, awaiting the two-box E2E.** All daemon
> phases (owner setting + identity tiers + tagged-caller guard; hub views + per-caller
> stamping + one-time legacy import; archive/delete guard; owner-view peers grouping),
> the web lens, and the Mac lens (view-scoped adoption + AVAILABLE section + put-away
> close + claim-on-revive + window-name unification) are committed. Two deltas from the
> plan as written, both recorded inline: the peers bus stays name-based (§ decisions),
> and an UNOWNED host serves its legacy shared groups unchanged, so upgrading before
> setting an owner changes nothing (compat rule in `api/views.go`).
>
> **To switch it on:** set each host's owner — `owner` in the lens Settings, or
> `--owner you@example.com` on the installer — then restart lenses. Verification list
> below is the E2E script.
>
> Written after the first real two-person deployment (patric-ccmux + dasha-ccmux, one
> hub) surfaced the interference: user B's fresh lens adopted user A's sessions, B
> closing "her" windows archived A's sessions globally, and A reviving a cold repo
> landed it on B's newly created window.

Two people share one hub today and the daemon cannot tell their arrangements apart,
because there is nothing to tell apart: a *window* exists only inside one Mac's
`state.json` (`Sources/ccmux/Models/AppState.swift:73`), and the only shared placement
signal is a single global string per workspace (`Workspace.Group`, `model.go:94`) that
every connected Mac rewrites last-writer-wins (`WindowManager.swift:818-827`). Closing a
window archives its hosted workspaces for everyone (`WindowManager.swift:225-292` →
`manager.go:565`). Meanwhile the daemon already resolves every tailnet caller to a
verified login (`api/identity.go:32`) and then uses it for nothing but push keying and
git attribution.

The shape is **shared sessions, personal views**:

- **A session is the work.** Global, visible to everyone on the tailnet, runs in one
  host's tmux. Unchanged.
- **A view is one person's arrangement.** Which sessions sit in which of *my* windows,
  and which I have put away. Per-login, stored at the hub, invisible to other lenses
  except as a read-only label.

Nobody's action mutates another person's view. Collaboration is opt-in: any session can
be *opened into* your own window at any time; the underlying session is the same tmux,
so two people attaching still means one terminal, as today.

## Decisions

- **Views are per-login rows at the hub, keyed `(login, workspace)` → window name.**
  This supersedes the multihost decision "Groups are global" (`multihost-plan.md:43`):
  the *grouping* becomes personal; what stays global is the session itself. The hub is
  the natural owner — views are control-plane lens metadata like push subscriptions,
  a single machine is its own hub, and hub-down already means the control plane is down
  (`multihost-plan.md:61`). No per-host view storage, no view federation.
- **The wire contract does not change shape — it changes referent.** `group` on a
  workspace becomes *the caller's* view of it, stamped per request at the hub exactly
  like `Host` is stamped today (runtime-only, `aggregator.go:179`). `PUT
  /v1/workspaces/{id}/group` writes the caller's view row; empty string deletes it.
  Lenses keep their existing field and endpoint; the semantics do the work.
- **Every session gets an `owner` login, resolved at the hub.** v1 rule: the owner is
  the owner of the machine the session runs on (see the `owner` setting below), stamped
  runtime-only like `Host`. This is correct for the actual topology — one person per
  box — and avoids any host schema change or identity relay. Persisting a per-workspace
  owner for cross-host creates is a named follow-up, not v1.
- **Each host declares its human: a new host-local setting `owner`** (a tailnet login,
  e.g. `sandelin@gmail.com`), asked by the installer, editable in settings. It slots
  into the existing cascade as host-local (`multihost-plan.md:195`). It also becomes the
  identity fallback for unverified loopback callers (`identity.go:36`), which is what
  finally makes the Mac-on-127.0.0.1 case resolve to a real person instead of a
  self-declared `?user=` string — superseding the alias crutch for the one-user-per-
  machine case (`manager/identityalias.go:11`).
- **Close puts away a view; it archives only what is safely yours.** Closing a window
  always deletes your view rows. It archives a hosted session only when you are its
  owner *and* no other login holds a view row on it — which is exactly today's behavior
  in a single-user install, so nothing changes for a lone machine. The guard is
  enforced at the hub (409 on `/archive` when another login's row exists), not trusted
  to the lens. An explicit "Stop session" context action remains for the owner.
- **Revive binds to the window you clicked in.** The Mac claims the workspace into the
  clicking window and writes its view row *before* `POST /revive`, the same shape the
  create path already has (`WorkspaceWindowController.swift:74-81`). Today revive sends
  nothing about the window at all (`RemoteSessionService.swift:525-530`) and placement
  falls to a global name-match-or-window-0 heuristic (`WindowManager.swift:743`).
- **Other people's arrangements are visible, read-only, as labels.** A workspace gains
  runtime-only `owner` and `ownerGroup` (the owner's window name for it). Sessions with
  no view row for you render in an **Available** section, labeled `Patric · CHARTLABS`;
  opening one writes your view row. This is the "she sees my windows, closed for her,
  can open them" behavior.
- **The peers-bus group boundary keys on the owner's view — by NAME.** The hub already
  resolves a peer's group by joining pane → workspace → group against its aggregate
  (`multihost-plan.md:161`); that join now reads `views[owner]` (owner's row → legacy
  group until imported → "" once put away, whereupon the directory-name fallback
  applies, `peers/service.go:353-373`). Two Claudes are teammates when *the owner* put
  their sessions in one window — deterministic, no dependence on who else is watching.
  The bus deliberately stays name-based rather than `(login, window)`-scoped: group
  names flow through human-typed `to_group` addressing, error strings, and the
  directory-name fallback that lets a plain-terminal Claude join its project, and all
  of those match by bare name. Consequence, accepted: two users who name a window
  identically share one bus group — same fuzziness the folder fallback always had.
- **No `contract` bump.** Every change is additive fields, hub-local semantics, or
  lens-side. A member host on the old build still lists and attaches; the hub's view
  stamping and archive guard sit in front of it either way.

## Daemon

### 1. Host `owner` setting + identity fallback

- New settings key `owner` (host-local scope), prompted by `install.sh` / `ccmuxd
  install`, stored like `startup_command`. Empty means "unowned", which degrades to
  today's behavior everywhere below.
- `resolveIdentity` gains a middle tier: WhoIs-verified → as today; unverified but the
  host has an `owner` → `{Login: owner, Verified: false}` with the self-declared name
  kept as `Display`; else the `?user=`/anon path. Push keying, presence suppression,
  and driver attribution all inherit the fix for free, since they already flow through
  this one function (`identity.go:21`).
- **Tagged callers are machines, never people.** The ccmux daemon nodes themselves
  carry `tag:ccmux`, and a tagged Tailscale node has no user login — WhoIs answers
  with the synthetic `tagged-devices` profile. Both resolvers today would accept that
  as a *verified person* (`tailnet/whois.go:70`, `tailnet/tsnet.go:38` — any non-empty
  `LoginName` passes). Guard both: a response whose node carries tags (or whose login
  is `tagged-devices`) resolves `ok=false`, falling through to the `owner`/self-
  declared tiers like loopback does. Human identity therefore only ever comes from an
  untagged personal device (a laptop, a phone, a browser) — which is exactly what
  lenses connect from — while daemon↔daemon calls stay authorized by tag membership,
  as designed (`multihost-plan.md:87`).

### 2. Views at the hub

New hub-store table (same SQLite, hub role only):

```sql
CREATE TABLE IF NOT EXISTS views (
  login TEXT, ws_id TEXT, window TEXT NOT NULL,
  updated_at INTEGER, PRIMARY KEY (login, ws_id)
);
```

- `GET /v1/workspaces` (hub aggregation, `hub.go`): after `stampHost`, stamp per caller
  — `group` = caller's row (empty if none), `owner` = owning host's `owner` setting,
  `ownerGroup` = owner's row. All three runtime-only, like `Host` (`model.go:92`).
- `PUT /v1/workspaces/{id}/group` is handled *at the hub* (no longer proxied): upsert
  `(caller, id) → window`; empty body deletes the row. Broadcasts `workspace-status` so
  every lens refetches, as `SetGroup` does today (`manager.go:646`).
- `POST /v1/workspaces` / `POST /v1/hosts/{host}/workspaces`: the hub writes the
  creator's view row from the request's `group` before proxying the create onward. The
  host's own `ws_group` column keeps receiving the value for rollback safety but
  nothing reads it after migration.
- Row hygiene: on `workspace-removed` the hub deletes all rows for that id. Archive
  does *not* touch rows — a cold session stays in the windows of everyone who had it,
  which is what makes "my cold sessions under my window" work per person.

### 3. One-time import (migration)

On each aggregation pass, any workspace carrying a non-empty legacy `ws_group` with
*zero* view rows gets one row `(host-owner, ws, ws_group)`. Idempotent, self-healing
for hosts that upgrade late, and a no-op once rows exist. Requires the host's `owner`
to be set; until it is, the workspace simply shows as Available to everyone, which is
safe.

### 4. Archive guard

`POST /v1/workspaces/{id}/archive` at the hub: if any *other* login holds a view row,
respond `409 {holder: login}` instead of proxying. `?force=1` overrides (surfaced in
the lens as an explicit "Archive anyway"). Owner check: refuse with 409 likewise when
the caller is not the owner, same override. `DELETE /v1/workspaces/{id}` gets the same
guard — delete is archive plus permanence.

### 5. Peers

- `SetLocalPaneGroups` (`service.go:308`) gains the pushing caller's resolved login, so
  Mac-local driver panes group under their user, not just their host.
- Group resolution for hosted panes joins through `views[owner]` as decided above. The
  case-insensitive name compare (`service.go:373`) is unchanged but now scoped: two
  users may both have a window named `dasha` without their sessions becoming teammates,
  because the join key is `(owner's login, window)`, not the bare name.

## Lenses

### 6. Mac app

- **Adoption becomes per-user for free**: `groups[id]` already mirrors the wire `group`
  (`RemoteSessionService.swift:688`), which is now the caller's own view, so
  `adoptOrphanHostedWorkspaces` (`WindowManager.swift:702`) stops seeing other people's
  placements. A fresh Mac with an empty `state.json` adopts nothing and shows
  everything under Available — the exact fix for B's first launch.
- **Close** (`windowWillClose`, `WindowManager.swift:225`): per hosted workspace, send
  `PUT group ""`; archive only when `owner == me` and the archive call succeeds; on
  409, leave it running and drop it from the view silently (it reappears under
  Available). Local panes tear down as today. `ClosedWindow` restore recipes stay
  Mac-local in v1.
- **Revive**: claim into the clicking window + `PUT group` before `POST /revive`
  (parity with `beginHostedCreate`). Same for the Available section's "Open here".
- **Available section** in the sidebar: sessions with no view row, grouped by
  `owner · ownerGroup` labels, live and cold alike; replaces the current global COLD
  SESSIONS bucket's role for other people's work (`SidebarView.swift:48-52`).
- **Window-name unification**: one `displayName` helper ends the `"This Window"` vs
  `"Window N"` split (`SidebarView.swift:54-56` vs `WindowManager.swift:870-875`), which
  today makes an unnamed window unable to match its own cold sessions.

### 7. Web lens

- Buckets by `ws.group` exactly as today (`web/app.js:101-140`) — now automatically the
  caller's windows. The no-group bucket is retitled **Available** and rows carry the
  `owner · ownerGroup` label. Opening one sends the existing `PUT group` with a chosen
  window name. Create-into-group datalist (`app.js:833`) now lists only your windows.

## Verification

1. **Fresh second user.** B's first lens launch on a hub full of A's sessions: zero
   windows adopted, everything under Available with A's labels. Nothing of A's changes.
2. **Independent close.** B opens one of A's sessions into her window, then closes that
   window: her rows are gone, the session keeps running, A's sidebar never moves.
3. **Owner close unchanged.** A closes his own window while nobody else holds rows: the
   sessions archive exactly as today. With B holding a row: A's close leaves it
   running; "Archive anyway" forces it.
4. **Revive placement.** A revives a cold repo from window CHARTLABS while B has a
   window named `dasha`: it lands in CHARTLABS on every one of A's lenses and appears
   only in B's Available. The original bug, replayed.
5. **Name collisions.** A and B both name a window `shared`: their *sidebars* do not
   co-mingle (views are keyed by login). The peers bus, being name-based (see the
   decision above), does treat the two as one project group.
6. **Peers.** Two Claudes in one owner-window message each other across hosts as
   before; a session B opened into her window does not join her bus groups.
7. **Push and attribution.** Loopback Mac on an owned host keys pushes and co-author to
   the owner login with no alias configured.
7b. **Tagged caller.** A request arriving from a `tag:ccmux` node resolves as no-person
   (not as a verified `tagged-devices` user): presence, driver, and view writes all
   fall to the owner/self-declared tiers.
8. **Single machine, single user.** One daemon, one person, `owner` set: behavior is
   indistinguishable from today, including close-archives.

## Order of work (agent wall-clock)

1. `owner` setting + installer prompt + identity fallback tier (~30 min)
2. Hub `views` table + per-caller stamping (`group`/`owner`/`ownerGroup`) + hub-handled
   `PUT group` + legacy import (~75 min)
3. Archive guard + force path (~20 min)
4. Mac: close rewrite, claim-on-revive, Available section, window-name unification
   (~90 min)
5. Web: Available section + labels (~30 min)
6. Peers: owner-view group join + login on local-groups push (~45 min)
7. Two-user E2E on the two real boxes (~45 min)

≈ 5.5 h of execution. External waits: none beyond the two-box E2E being manual.

## Out of scope (named follow-ups)

- **Shared windows** — a window as a first-class hub entity with a member list, jointly
  edited. v1's "open into my own window" covers the collaboration actually needed now.
- **Persisted per-workspace owner + hub→host identity relay** — needed only when
  someone creates sessions on a machine that is not theirs.
- **Cross-device restore of closed windows** — `ClosedWindow` recipes moving to the hub.
- **Visibility ACLs** — everyone still sees everything; the tailnet remains the auth
  boundary, and views are arrangement, not authorization.

## Open questions (decide during build, not blocking)

- Whether Available shows other users' window *structure* (expandable groups) or stays
  flat labels. Start flat.
- Whether a 409-blocked archive should notify the row-holder ("A wants to stop
  CHARTLABS"). Start silent; the session simply stays in their Available/window.

## v2: shared windows (supersedes per-user views, 2026-08-21)

The model, in Patric's words: a repo is the source — one session, one set of panes,
Claude included — and every lens shows the same view of it. Windows are shared the
same way: ONE arrangement (window name + member repos, a repo in at most one window),
identical for every person on every lens. The only personal state is which windows
each login has OPEN right now. When the LAST person closes a window, its sessions
archive — exactly pre-multi-user behavior, generalized.

### Decisions

- **Windows are first-class shared entities at the hub.** Three tables replace the
  per-login `views` rows: `windows(id, name)`, `window_members(ws_id PK, window_id)`
  (a workspace lives in at most one window), `window_open(login, window_id)`. Names
  are unique case-insensitively; the id survives renames.
- **The wire keeps its shape.** `Workspace.Group` goes back to meaning the shared
  window name — for every caller. `PUT /v1/workspaces/{id}/group` assigns membership
  by name, creating the window if new; empty removes it. Old lenses keep working.
- **New surface:** `GET /v1/windows` → `[{id, name, open, openBy, workspaceIds}]`
  (open = for the caller); `POST /v1/windows/{id}/open` and `/close`;
  `PUT /v1/windows/{id}` renames. Close reports `{last: true, members: [...]}` when
  the caller was the final opener — the LENS then archives the members through the
  existing guarded archive route (keeps the daemon handler simple and reuses the
  Mac's existing close loop).
- **The archive guard keys on open state, not view rows:** archiving or deleting a
  session is refused (409, force=1 overrides) while its window is open by someone
  ELSE. The owner check and the fail-closed 503 stay.
- **The Mac app follows the daemon; it never blanket-syncs window names up.** The
  steady-state push (`syncHostedGroups`) dies — it is exactly the write-back fight
  that made v1's bugs. Membership writes happen only on explicit user actions: drag,
  create, revive-into-window, rename. Reconcile moves repos BETWEEN local windows
  when the shared membership changed elsewhere, and releases repos whose window is
  not open here.
- **Restore Window becomes shared.** The sidebar's closed-window list is
  `GET /v1/windows` where the caller has it closed; opening one opens the same
  window everyone else sees. Mac-local `ClosedWindow` records remain only for
  windows of purely local workspaces.
- **The peers bus reads the shared membership** — group = the window name, global
  again, matching multihost §3's original "groups are global" decision.
- **Migration:** one shot on first hub start after upgrade — distinct window names in
  `views` (case-insensitively merged) become `windows`; membership prefers the host
  owner's row, else any row; every login's rows become open flags. A wiped `views`
  table migrates to nothing, which is the recommended clean start.

### What v1 keeps contributing

Identity (verified / owner-at-the-keyboard / self-declared), the tagged-caller
guard, host owner attribution and labels, `?raw=1`, the fail-closed guard pattern,
and the events/cache plumbing all carry over unchanged.

### Verification (two boxes, replaces v1 list where it conflicts)

1. Patric arranges CHARTLABS; Dasha's lens shows the same window with the same
   repos, closed for her until she opens it.
2. Both open CHARTLABS: both see the same panes in repo XYZ, live.
3. Dasha closes CHARTLABS: nothing changes for Patric; sessions keep running.
4. Patric closes it too (last): sessions archive; the window sits in both
   Restore lists; either can reopen and revive.
5. Dasha moves repo XYZ to another window: it moves for Patric too, on every lens.
6. Archive/remove of a session whose window the other person has open: 409 with
   the person's name; force works; the Mac shows the same "anyway" flow as web.
