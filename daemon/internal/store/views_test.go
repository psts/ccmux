package store

import "testing"

// Views are per-login arrangement rows and must survive a store reopen (the
// daemon-restart path), and an empty window must delete the row — "put away"
// is a deletion, not a value.
func TestViewsPersistAcrossReopen(t *testing.T) {
	path := t.TempDir() + "/reg.db"
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.SetView("patric@x.com", "w1", "CHARTLABS"))
	must(s.SetView("dasha@x.com", "w1", "dasha"))
	must(s.SetView("patric@x.com", "w2", "HQ"))
	must(s.SetView("patric@x.com", "w2", "HQ2")) // upsert replaces
	must(s.SetView("dasha@x.com", "w2", "x"))
	must(s.SetView("dasha@x.com", "w2", "")) // put away deletes
	must(s.MarkViewImported("w1"))
	must(s.Close())

	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	views, err := s.AllViews()
	if err != nil {
		t.Fatalf("all views: %v", err)
	}
	if views["w1"]["patric@x.com"] != "CHARTLABS" || views["w1"]["dasha@x.com"] != "dasha" {
		t.Fatalf("w1 rows = %v", views["w1"])
	}
	if views["w2"]["patric@x.com"] != "HQ2" {
		t.Fatalf("upsert lost: %v", views["w2"])
	}
	if _, ok := views["w2"]["dasha@x.com"]; ok {
		t.Fatalf("put-away row survived: %v", views["w2"])
	}
	imported, err := s.ViewImports()
	if err != nil || !imported["w1"] || imported["w2"] {
		t.Fatalf("imports = %v (%v)", imported, err)
	}

	// Deleting a workspace's views clears rows AND the import marker.
	must(s.DeleteWorkspaceViews("w1"))
	views, _ = s.AllViews()
	imported, _ = s.ViewImports()
	if len(views["w1"]) != 0 || imported["w1"] {
		t.Fatalf("delete left rows %v imported %v", views["w1"], imported["w1"])
	}
}
