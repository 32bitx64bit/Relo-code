package orchestrator

import (
	"context"
	"fmt"

	"github.com/relocode/relo/internal/integration"
)

func (c *Coord) Integrate(ctx context.Context) (*integration.Result, error) {
	wsManager := c.workspace()
	if wsManager == nil {
		return &integration.Result{}, nil
	}
	sess := c.Current()
	if sess == nil {
		return nil, fmt.Errorf("no session")
	}
	by := map[string][]string{}
	agents := c.Agents.List()
	for _, a := range agents {
		if a.Worktree == "" || a.Worktree == sess.Workspace {
			continue
		}
		files, err := wsManager.Changed(a.ID)
		if err != nil {
			c.logWarn("list worktree changes", err)
			return nil, err
		}
		by[a.ID] = files
	}
	conflicts := integration.Detect(by)
	if len(conflicts) > 0 {
		return &integration.Result{Conflicts: conflicts}, nil
	}
	var last integration.Result
	for _, a := range agents {
		if a.Worktree == "" || a.Worktree == sess.Workspace {
			continue
		}
		patch, err := wsManager.Patch(a.ID)
		if err != nil {
			c.logWarn("read worktree patch", err)
			return &last, err
		}
		if patch == "" {
			continue
		}
		base := wsManager.Base()
		res, err := integration.Apply(sess.Workspace, patch, append(base.Dirty, base.Untracked...))
		if err != nil {
			return &res, err
		}
		last = res
	}
	return &last, nil
}
