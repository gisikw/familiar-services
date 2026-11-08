package attention

// Card actions: one-click buttons on a card with effects from a small fixed set
// (spec §5). The card is the context: a fork effect hands its fork only the
// click line and the card ref, never hidden task text (Kev's safety rule).

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"time"
)

type cardEffect struct {
	Type   string `json:"type"`
	Lane   string `json:"lane,omitempty"`
	Queue  string `json:"queue,omitempty"`
	Field  string `json:"field,omitempty"`
	Value  any    `json:"value,omitempty"`
	Until  string `json:"until,omitempty"`
	For    string `json:"for,omitempty"`
	Option string `json:"option,omitempty"`
	Label  string `json:"label,omitempty"`
	Fresh  bool   `json:"fresh,omitempty"`
	Target string `json:"target,omitempty"`
}

type cardAction struct {
	Key     string       `json:"key"`
	Label   string       `json:"label"`
	Done    string       `json:"done,omitempty"`
	Primary bool         `json:"primary,omitempty"`
	Effects []cardEffect `json:"effects"`
}

var effectTypes = []string{"move", "approve", "set", "done", "snooze", "choose", "reply", "fork"}

// Reserved keys: the ask built-ins and the woken-card trio.
var reservedKeys = []string{"not-now", "talk", "split", "still-someday", "do-it", "archive"}

func validEffect(raw any) (cardEffect, error) {
	bad := func(m string) (cardEffect, error) {
		return cardEffect{}, code("invalid_request", "invalid effect: "+m)
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return bad("an effect is an object with a type")
	}
	b, _ := json.Marshal(m)
	var e cardEffect
	if json.Unmarshal(b, &e) != nil || !one(e.Type, effectTypes...) {
		return bad("type must be one of " + strings.Join(effectTypes, ", "))
	}
	switch e.Type {
	case "move":
		if !allowed(m, "type", "lane", "queue") || !one(e.Lane, v3Lanes...) && e.Lane != "archived" {
			return bad("move takes lane (todo, queue, running, done, archived) and queue?")
		}
		if e.Lane == "queue" && !one(e.Queue, queues...) {
			return bad("move to queue needs queue")
		}
	case "approve", "done", "reply":
		if !allowed(m, "type") {
			return bad(e.Type + " takes no arguments")
		}
	case "set":
		if !allowed(m, "type", "field", "value") || !one(e.Field, "due", "revisit", "confidence", "title", "summary") {
			return bad("set takes field (due, revisit, confidence, title, summary) and value")
		}
	case "snooze":
		if !allowed(m, "type", "until", "for") || (e.Until != "" && e.For != "") {
			return bad("snooze takes until or for")
		}
		if e.Until != "" && !validDate(e.Until) {
			return bad("until must be YYYY-MM-DD")
		}
		if e.For != "" {
			if _, ok := parseDur(e.For); !ok {
				return bad("for must be like 10d, 2w, 6mo")
			}
		}
	case "choose":
		if !allowed(m, "type", "option") || !validUnits(e.Option, 200, true) {
			return bad("choose takes option")
		}
	case "fork":
		if !allowed(m, "type", "label", "fresh", "target") {
			return bad("fork takes label?, fresh?, target? (no task: the card is the brief)")
		}
		if e.Label != "" && !validUnits(e.Label, 60, true) {
			return bad("label must be one line of at most 60 characters")
		}
		if e.Target != "" && !targetRE.MatchString(e.Target) {
			return bad("target must be instance:<session id>")
		}
	}
	return e, nil
}

func validCardActions(raw any) ([]cardAction, error) {
	if raw == nil {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok || len(list) > 5 {
		return nil, code("invalid_request", "actions is a list of at most 5")
	}
	out := []cardAction{}
	seen := map[string]bool{}
	for _, it := range list {
		m, ok := it.(map[string]any)
		if !ok || !allowed(m, "key", "label", "done", "primary", "effects") {
			return nil, code("invalid_request", "an action is {key, label, done?, primary?, effects}")
		}
		key, _ := str(m, "key")
		label, _ := str(m, "label")
		if !validKey(key) || one(key, reservedKeys...) || seen[key] {
			return nil, code("invalid_request", "invalid or reserved action key: "+key)
		}
		seen[key] = true
		if !validUnits(label, 60, true) {
			return nil, code("invalid_request", "action label must be one line of at most 60 characters")
		}
		a := cardAction{Key: key, Label: label}
		if d, ok := str(m, "done"); ok {
			a.Done = d
		}
		a.Primary, _ = boolean(m, "primary", false)
		effs, ok := m["effects"].([]any)
		if !ok || len(effs) == 0 || len(effs) > 4 {
			return nil, code("invalid_request", "an action has 1 to 4 effects")
		}
		forks := 0
		for _, x := range effs {
			e, err := validEffect(x)
			if err != nil {
				return nil, err
			}
			if e.Type == "fork" {
				forks++
			}
			a.Effects = append(a.Effects, e)
		}
		if forks > 1 {
			return nil, code("invalid_request", "at most one fork per action")
		}
		out = append(out, a)
	}
	return out, nil
}

func cardActionsWire(raw sql.NullString) any {
	if !raw.Valid {
		return []any{}
	}
	var v []any
	if json.Unmarshal([]byte(raw.String), &v) != nil {
		return []any{}
	}
	return v
}

// ForkClickLine is everything a card-action fork is told. Anything else it
// needs must already be on the card, where Kev could read it before clicking.
func ForkClickLine(label, id, title string) string {
	return fmt.Sprintf("Kevin clicked %q on card %s (%s). Read the card: imp attn card get %s", label, id, title, id[:8])
}

// woken returns the built-in trio for a card woken back to Kev from Icebox.
func wokenActions() []cardAction {
	return []cardAction{
		{Key: "still-someday", Label: "Still someday", Done: "Snoozed again", Effects: []cardEffect{{Type: "snooze"}}},
		{Key: "do-it", Label: "Do it", Done: "Back in Todo", Primary: true, Effects: []cardEffect{{Type: "set", Field: "revisit", Value: nil}}},
		{Key: "archive", Label: "Archive", Done: "Archived", Effects: []cardEffect{{Type: "done"}}},
	}
}

// cardAct runs one action on a card: {id, key, text?}. Effects apply in one
// transaction; a fork starts last, inside it, so a refused spawn rolls back.
func (s *Store) cardAct(actor string, a map[string]any) (any, error) {
	ref, _ := str(a, "id")
	id, e := s.resolveCardID(ref)
	if e != nil {
		return nil, e
	}
	key, ok := str(a, "key")
	if !ok || !validKey(key) {
		return nil, code("invalid_request", "key required")
	}
	r, e := s.card(id)
	if e != nil {
		return nil, code("not_found", "card not found")
	}
	var acts []cardAction
	if r.Actions.Valid {
		_ = json.Unmarshal([]byte(r.Actions.String), &acts)
	}
	if r.WokeAt.Valid {
		acts = append(acts, wokenActions()...)
	}
	var act *cardAction
	for i := range acts {
		if acts[i].Key == key {
			act = &acts[i]
		}
	}
	if act == nil {
		return nil, code("not_found", "no such action on this card")
	}
	talk := false
	_, e = s.write(func(tx *sql.Tx, at string) (any, error) {
		if er := event(tx, id, actor, "acted", at, map[string]any{"key": act.Key, "label": act.Label}); er != nil {
			return nil, er
		}
		var fork *cardEffect
		for i, ef := range act.Effects {
			var er error
			switch ef.Type {
			case "move":
				args := map[string]any{"lane": ef.Lane}
				if ef.Queue != "" {
					args["queue"] = ef.Queue
				}
				_, er = s.apply(tx, at, actor, "card.move", id, args)
			case "approve":
				_, er = s.apply(tx, at, actor, "card.set", id, map[string]any{"approved": true})
			case "set":
				_, er = s.apply(tx, at, actor, "card.set", id, map[string]any{ef.Field: ef.Value})
			case "done":
				er = s.doneTx(tx, at, actor, id)
			case "snooze":
				args := map[string]any{}
				if ef.Until != "" {
					args["until"] = ef.Until
				}
				if ef.For != "" {
					args["for"] = ef.For
				}
				er = s.snoozeTx(tx, at, actor, id, args)
			case "choose":
				_, er = tx.Exec("insert into notes(card_id,at,by,text,detail) values(?,?,?,?,null)", id, at, actor, clipUnits("Kev chose: "+ef.Option, 300))
			case "reply":
				talk = true
				text, _ := str(a, "text")
				if strings.TrimSpace(text) != "" {
					_, er = tx.Exec("insert into notes(card_id,at,by,text,detail) values(?,?,?,?,null)", id, at, actor, clipUnits(text, 300))
				}
			case "fork":
				fork = &act.Effects[i]
			}
			if er != nil {
				return nil, er
			}
		}
		if key == "do-it" || key == "still-someday" || key == "archive" {
			if _, er := tx.Exec("update cards set woke_at=null where id=?", id); er != nil {
				return nil, er
			}
		}
		if fork != nil {
			if s.SpawnFork == nil {
				return nil, code("unavailable", "starting a fork is unavailable here")
			}
			var n int
			_ = tx.QueryRow("select count(*) from events where card_id=? and kind='forked' and json_extract(data,'$.key')=?", id, act.Key).Scan(&n)
			if n > 0 {
				return nil, code("conflict", "this action already started a fork")
			}
			label := fork.Label
			if label == "" {
				label = act.Label
			}
			target := fork.Target
			if target == "" {
				target = s.SplitTarget
			}
			spawn := ForkSpawn{ID: fmt.Sprintf("card-%s-%s-%d", id[:8], act.Key, s.now().Unix()), Target: target, Task: ForkClickLine(act.Label, id, r.Title), Label: label, Fresh: fork.Fresh, Choice: act.Key}
			if er := s.SpawnFork(spawn); er != nil {
				return nil, er
			}
			if er := event(tx, id, actor, "forked", at, map[string]any{"key": act.Key, "label": label}); er != nil {
				return nil, er
			}
		}
		_, er := tx.Exec("update cards set updated_at=? where id=?", at, id)
		return nil, er
	})
	if e != nil {
		return nil, e
	}
	card, e := s.full(id)
	if e != nil {
		return nil, e
	}
	return map[string]any{"card": card, "talk": talk}, nil
}

// doneTx is the `done` effect: a jot is ticked; any other card is archived.
// Still only by Kev's click (Kes never archives; guidance and this check).
func (s *Store) doneTx(tx *sql.Tx, at, actor, id string) error {
	var project, lane string
	if e := tx.QueryRow("select project,lane from cards where id=?", id).Scan(&project, &lane); e != nil {
		return code("not_found", "card not found")
	}
	if project == "_today" {
		_, e := s.apply(tx, at, actor, "card.done", id, map[string]any{"done": true})
		return e
	}
	if actor != "kevin" {
		return code("invalid_request", "only Kev archives a card; offer him a done action instead")
	}
	if lane == "archived" {
		return nil
	}
	if _, e := tx.Exec("update cards set lane='archived',snoozed=0,woke_at=null,moved_at=?,updated_at=? where id=?", at, at, id); e != nil {
		return e
	}
	return event(tx, id, actor, "moved", at, map[string]any{"from": lane, "to": "archived"})
}

func clipUnits(x string, max int) string {
	r := []rune(x)
	if len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return x
}

var _ = rand.Intn
var _ = time.Now
