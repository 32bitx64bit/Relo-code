package orchestrator

import "reflect"

// IntegrationUpdate is one change's new projection and derived wire state. A nil Integration leaves the stored
// projection unchanged.
type IntegrationUpdate struct {
	ID           string
	Integration  *Integration // nil means leave the current projection in place
	Conflict     string
	SupersededBy string
}

// UpdateIntegration applies a batch of projections under one lock and emits changes.updated once, and only when a
// visible field changed. Projection loops that find nothing new therefore cannot re-arm the UI or auto-apply.
func (c *Coord) UpdateIntegration(updates []IntegrationUpdate) bool {
	if len(updates) == 0 {
		return false
	}
	want := make(map[string]IntegrationUpdate, len(updates))
	for _, u := range updates {
		want[u.ID] = u
	}
	c.mu.Lock()
	if c.sess == nil {
		c.mu.Unlock()
		return false
	}
	changed := false
	for i := range c.sess.Changes {
		u, ok := want[c.sess.Changes[i].ID]
		if !ok {
			continue
		}
		ch := &c.sess.Changes[i]
		if u.Integration != nil && !reflect.DeepEqual(ch.Integration, u.Integration) {
			ch.Integration = cloneIntegration(u.Integration)
			changed = true
		}
		if ch.Conflict != u.Conflict {
			ch.Conflict = u.Conflict
			changed = true
		}
		if u.SupersededBy != "" && ch.SupersededBy != u.SupersededBy {
			ch.SupersededBy = u.SupersededBy
			changed = true
		}
	}
	snapshot := append([]Change{}, c.sess.Changes...)
	c.mu.Unlock()
	if changed {
		c.emit("changes.updated", snapshot)
	}
	return changed
}

func cloneIntegration(in *Integration) *Integration {
	if in == nil {
		return nil
	}
	out := *in
	out.Hunks = append([]Hunk(nil), in.Hunks...)
	out.Since = append([]string(nil), in.Since...)
	return &out
}

// FindChange returns the change with the given id from the current session.
func (c *Coord) FindChange(id string) (Change, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sess != nil {
		for _, ch := range c.sess.Changes {
			if ch.ID == id {
				return ch, true
			}
		}
	}
	return Change{}, false
}

// Changes returns a copy of the current session's changes.
func (c *Coord) Changes() []Change {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sess == nil {
		return []Change{}
	}
	return append([]Change{}, c.sess.Changes...)
}

// setLineage records the dependencies applied to an agent's base. It is written once at spawn and kept across
// status changes and recuts.
func (c *Coord) setLineage(agentID string, deps []Dep) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lineage == nil {
		c.lineage = map[string][]Dep{}
	}
	c.lineage[agentID] = append([]Dep(nil), deps...)
}

// lineageOf returns the dependencies applied to an agent's base.
func (c *Coord) lineageOf(agentID string) []Dep {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Dep(nil), c.lineage[agentID]...)
}

// UpdateChange edits one change in place under the Coord lock, so a worker recording new changes at the same time
// cannot be overwritten by a stale copy of the session, then tells the UI.
func (c *Coord) UpdateChange(id string, fn func(*Change)) (Change, bool) {
	c.mu.Lock()
	if c.sess == nil {
		c.mu.Unlock()
		return Change{}, false
	}
	var out Change
	found := false
	for i := range c.sess.Changes {
		if c.sess.Changes[i].ID == id {
			fn(&c.sess.Changes[i])
			out, found = c.sess.Changes[i], true
			break
		}
	}
	snapshot := append([]Change{}, c.sess.Changes...)
	c.mu.Unlock()
	if found {
		c.emit("changes.updated", snapshot)
	}
	return out, found
}

// UpdateChanges edits every change whose id is in ids under one lock and tells the UI once.
func (c *Coord) UpdateChanges(ids []string, fn func(*Change)) {
	if len(ids) == 0 {
		return
	}
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	c.mu.Lock()
	if c.sess == nil {
		c.mu.Unlock()
		return
	}
	for i := range c.sess.Changes {
		if want[c.sess.Changes[i].ID] {
			fn(&c.sess.Changes[i])
		}
	}
	snapshot := append([]Change{}, c.sess.Changes...)
	c.mu.Unlock()
	c.emit("changes.updated", snapshot)
}

// OpenChanges counts the changes of one agent that still wait for a decision.
func (c *Coord) OpenChanges(agentID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	if c.sess != nil {
		for _, ch := range c.sess.Changes {
			if ch.AgentID == agentID && ch.Status == "proposed" {
				n++
			}
		}
	}
	return n
}
