package api

import (
	"encoding/json"

	"github.com/gisikw/familiar-services/internal/attention"
	"github.com/gisikw/familiar-services/internal/scheduler"
)

// AskForkEvent turns a one-click ask fork into the same scheduler event
// `imp schedule --fork` writes, so the primary's scheduler extension spawns it
// with `imp fork` exactly like a scheduled fork. The id is stable per ask: a
// retried resolve can't start a second fork.
func AskForkEvent(f attention.ForkSpawn) scheduler.Enqueue {
	req := map[string]any{"task": f.Task, "label": f.Label}
	if f.Fresh {
		req["fresh"] = true
	}
	body, _ := json.Marshal(req)
	return scheduler.Enqueue{
		ID:      f.ID,
		Target:  f.Target,
		Type:    "fork",
		Source:  "attn.ask",
		Summary: "Kev started: " + f.Label,
		Body:    string(body),
	}
}

// AskForkSpawner wires Attention's one-click forks to the scheduler.
func AskForkSpawner(sched *scheduler.Store) func(attention.ForkSpawn) error {
	return func(f attention.ForkSpawn) error {
		_, _, err := sched.Enqueue(AskForkEvent(f))
		return err
	}
}
