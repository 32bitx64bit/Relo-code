package ctxeng

import "time"

func (e *Engine) unhealthyLocked(st *state, threshold float64) (bool, string) {
	if threshold <= 0 {
		threshold = 0.85
	}
	if st.Metrics.Utilization >= threshold && st.Metrics.Generations > 0 {
		return true, "high context utilization"
	}
	repeats := 0
	for _, n := range st.Metrics.Repeats {
		if n >= 3 {
			repeats++
		}
	}
	if repeats >= 2 {
		return true, "repeated identical actions"
	}
	if st.Metrics.FailedEdits >= 3 {
		return true, "repeated failed edits"
	}
	if st.Metrics.RepeatedActions >= 4 {
		return true, "tool-call loop"
	}
	if !st.Metrics.LastProgress.IsZero() && e.now().Sub(st.Metrics.LastProgress) > 20*time.Minute && st.Metrics.Generations > 0 {
		return true, "stalled progress"
	}
	if len(st.Generations) > 0 {
		hist := 0
		for _, it := range st.Generations[len(st.Generations)-1].Items {
			if it.Class == ClassHistory || it.Class == ClassEphemeral {
				hist += it.Tokens
			}
		}
		if st.Budget > 0 && float64(hist) > 0.5*float64(st.Budget) {
			return true, "growing irrelevant transcript volume"
		}
	}
	return false, ""
}

func (e *Engine) MaybeRefresh(agentID string, threshold float64, auto bool) (*Generation, string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.ensure(agentID)
	ok, reason := e.unhealthyLocked(st, threshold)
	if !ok || !auto {
		return nil, reason, ok
	}
	st.Metrics.Refreshes++
	st.Metrics.RefreshReasons = append(st.Metrics.RefreshReasons, reason)
	e.gcPool(st)
	gen, err := e.buildLocked(BuildRequest{
		AgentID: agentID,
		TaskID:  st.TaskID,
		Role:    st.Role,
		Phase:   st.Phase,
		Budget:  st.Budget,
		Reason:  reason,
	}, true)
	if err != nil {
		return nil, reason, true
	}
	return gen, reason, true
}
