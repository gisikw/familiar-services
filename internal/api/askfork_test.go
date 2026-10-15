package api

import (
	"encoding/json"
	"testing"

	"github.com/gisikw/familiar-services/internal/attention"
)

func TestAskForkEventMatchesScheduledFork(t *testing.T) {
	e := AskForkEvent(attention.ForkSpawn{ID: "ask-7-20261014", Target: "instance:01abc", Task: "Do it.", Label: "do it", Fresh: true})
	if e.Type != "fork" || e.Target != "instance:01abc" || e.ID != "ask-7-20261014" || e.Source != "attn.ask" || e.Summary == "" {
		t.Fatalf("event %#v", e)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(e.Body), &body); err != nil || body["task"] != "Do it." || body["label"] != "do it" || body["fresh"] != true {
		t.Fatalf("body %s %v", e.Body, err)
	}
	body = nil
	if e2 := AskForkEvent(attention.ForkSpawn{ID: "a", Target: "instance:x", Task: "t", Label: "l"}); json.Unmarshal([]byte(e2.Body), &body) != nil || body["fresh"] != nil {
		t.Fatalf("fresh should be omitted: %s", e2.Body)
	}
}
