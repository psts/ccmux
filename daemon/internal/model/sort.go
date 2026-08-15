package model

import (
	"sort"
	"strings"
)

// SortByName orders workspaces alphabetically by name (case-insensitive), ID as
// the tiebreak. Every workspace list a lens receives goes through this: the
// manager's map iteration and the hub's concurrent member fetches both produce
// a different order on every call, and the sidebar rendered that order
// verbatim — so cold rows shuffled on each 4s poll.
func SortByName(wss []*Workspace) {
	sort.Slice(wss, func(i, j int) bool {
		a, b := strings.ToLower(wss[i].Name), strings.ToLower(wss[j].Name)
		if a != b {
			return a < b
		}
		return wss[i].ID < wss[j].ID
	})
}
