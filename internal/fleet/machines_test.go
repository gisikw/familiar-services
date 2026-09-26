package fleet

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestReconcileOwnsOnlyFleetProfiles(t *testing.T) {
	dir := t.TempDir()
	man, cat := filepath.Join(dir, "herdr-machines.json"), filepath.Join(dir, "client", "endpoints.json")
	os.MkdirAll(filepath.Dir(cat), 0700)
	os.WriteFile(cat, []byte(`{"version":1,"ssh":[
		{"id":"keep","label":"workbox","target":"workbox","session":"default","enabled":true},
		{"id":"b1","label":"bandit","target":"familiar-fleet-bandit","session":"familiar-fleet","enabled":true},
		{"id":"old","label":"kev-macbook","target":"familiar-fleet-kev-macbook","session":"familiar-fleet","enabled":true}]}`), 0600)
	os.WriteFile(man, []byte(`{"machines":[
		{"name":"bandit","ssh_alias":"familiar-fleet-bandit","session":"familiar-fleet"},
		{"name":"worklaptop","ssh_alias":"familiar-fleet-worklaptop","session":"familiar-fleet"}]}`), 0600)
	changed, err := Reconcile(man, cat)
	if err != nil || !changed {
		t.Fatalf("reconcile: %v %v", changed, err)
	}
	var got struct{ SSH []profile }
	b, _ := os.ReadFile(cat)
	json.Unmarshal(b, &got)
	byTarget := map[string]profile{}
	for _, p := range got.SSH {
		byTarget[p.Target] = p
	}
	if byTarget["workbox"].ID != "keep" || byTarget["familiar-fleet-bandit"].ID != "b1" {
		t.Fatalf("existing profiles disturbed: %#v", got.SSH)
	}
	if _, ok := byTarget["familiar-fleet-kev-macbook"]; ok {
		t.Fatal("revoked node kept")
	}
	if w := byTarget["familiar-fleet-worklaptop"]; w.Label != "worklaptop" || w.Session != "familiar-fleet" || !w.Enabled || len(w.ID) != 32 {
		t.Fatalf("new profile: %#v", w)
	}
	if changed, _ := Reconcile(man, cat); changed {
		t.Fatal("second reconcile changed the catalog")
	}
	if changed, err := Reconcile(filepath.Join(dir, "missing.json"), cat); changed || err != nil {
		t.Fatalf("missing manifest: %v %v", changed, err)
	}
}
