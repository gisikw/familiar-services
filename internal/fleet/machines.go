package fleet

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Machine profiles. The gateway's enrollment registry writes a manifest of
// enrolled nodes (herdr-machines.json); Herdr's `--machine` routing needs a
// saved profile per node in the client catalog (endpoints.json). Reconcile
// makes the catalog match the manifest for the profiles it owns (targets with
// the familiar-fleet- alias prefix) and leaves every other profile alone.
// Writing the catalog directly, not via `herdr machine add`, is deliberate:
// add probes the remote and refuses while a node is offline, and enrollment
// must not depend on the node being up at that moment.

const ownedPrefix = "familiar-fleet-"

type manifest struct {
	Machines []struct {
		Name     string `json:"name"`
		SSHAlias string `json:"ssh_alias"`
		Session  string `json:"session"`
	} `json:"machines"`
}

type catalog struct {
	Version int               `json:"version"`
	SSH     []json.RawMessage `json:"ssh"`
	rest    map[string]json.RawMessage
}

type profile struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Target  string `json:"target"`
	Session string `json:"session"`
	Enabled bool   `json:"enabled"`
}

// Reconcile returns whether it changed the catalog.
func Reconcile(manifestPath, catalogPath string) (bool, error) {
	b, err := os.ReadFile(manifestPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return false, err
	}
	want := map[string]profile{}
	for _, x := range m.Machines {
		if x.Name == "" || !strings.HasPrefix(x.SSHAlias, ownedPrefix) || x.Session == "" {
			continue
		}
		want[x.SSHAlias] = profile{Label: x.Name, Target: x.SSHAlias, Session: x.Session, Enabled: true}
	}

	raw := map[string]json.RawMessage{}
	if b, err := os.ReadFile(catalogPath); err == nil {
		if err := json.Unmarshal(b, &raw); err != nil {
			return false, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	var entries []json.RawMessage
	if raw["ssh"] != nil {
		if err := json.Unmarshal(raw["ssh"], &entries); err != nil {
			return false, err
		}
	}
	changed := false
	out := []json.RawMessage{}
	for _, e := range entries {
		var p profile
		if json.Unmarshal(e, &p) != nil || !strings.HasPrefix(p.Target, ownedPrefix) {
			out = append(out, e) // not ours: keep verbatim
			continue
		}
		w, ok := want[p.Target]
		if !ok {
			changed = true // revoked node: drop its profile
			continue
		}
		delete(want, p.Target)
		if p.Label != w.Label || p.Session != w.Session {
			p.Label, p.Session = w.Label, w.Session
			changed = true
			b, _ := json.Marshal(p)
			e = b
		}
		out = append(out, e)
	}
	for _, w := range want {
		var id [16]byte
		_, _ = rand.Read(id[:])
		w.ID = hex.EncodeToString(id[:])
		b, _ := json.Marshal(w)
		out = append(out, b)
		changed = true
	}
	if !changed {
		return false, nil
	}
	if raw["version"] == nil {
		raw["version"] = json.RawMessage("1")
	}
	ssh, _ := json.Marshal(out)
	raw["ssh"] = ssh
	b, err = json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(catalogPath), 0700); err != nil {
		return false, err
	}
	tmp := catalogPath + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0600); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, catalogPath)
}
