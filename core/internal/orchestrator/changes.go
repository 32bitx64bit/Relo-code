package orchestrator

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
