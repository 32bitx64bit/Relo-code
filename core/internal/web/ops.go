package web

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// operation is retained after completion so Wait can collect its result. An
// agent's completed operation is replaced by its next operation.
type operation struct {
	id      string
	agentID string
	action  Action
	done    chan struct{}
	result  Result
	err     error
}

var errNoOperation = errors.New("web: no current operation")

func (e *Engine) startOperation(agent *agentState, settings Settings, req Request) (Result, error) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return Result{}, errEngineClosed
	}
	if agent.forgotten {
		e.mu.Unlock()
		return Result{}, context.Canceled
	}
	if agent.current != "" {
		if current := e.ops[agent.current]; current != nil {
			select {
			case <-current.done:
				delete(e.ops, current.id)
			default:
				e.mu.Unlock()
				return Result{}, fmt.Errorf("web: agent %q already has running operation %s", agent.id, current.id)
			}
		}
	}
	e.opSeq++
	op := &operation{
		id:      fmt.Sprintf("web-%d", e.opSeq),
		agentID: agent.id,
		action:  req.Action,
		done:    make(chan struct{}),
	}
	e.ops[op.id] = op
	agent.current = op.id
	e.mu.Unlock()

	go e.runOperation(agent, settings, req, op)
	return Result{State: StateRunning, Op: op.id}, nil
}

func (e *Engine) runOperation(agent *agentState, settings Settings, req Request, op *operation) {
	result, err := e.execute(agent.ctx, agent, settings, req)
	result.Op = string(req.Action)
	if err == nil {
		if result.State != StateNeedsUser {
			result.State = StateDone
			result.Outcome = OutcomeDone
		}
	} else {
		result.State = StateDone
		result.Outcome = OutcomeDone
		switch {
		case errors.Is(err, context.Canceled):
			result.Outcome = OutcomeCancelled
		case errors.Is(err, context.DeadlineExceeded):
			result.Outcome = OutcomeTimeout
		}
	}

	e.mu.Lock()
	op.result = result
	op.err = err
	close(op.done)
	e.mu.Unlock()
}

// waitOperation has a separate timer from ctx: reaching d is a normal running
// result, while cancellation of the caller is an error.
func boundedWait(d time.Duration) time.Duration {
	if d <= 0 || d > MaxWait {
		return MaxWait
	}
	return d
}

func (e *Engine) waitOperation(ctx context.Context, op *operation, d time.Duration) (Result, error) {
	d = boundedWait(d)
	select {
	case <-op.done:
		return op.result, op.err
	default:
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-op.done:
		return op.result, op.err
	case <-t.C:
		return Result{State: StateRunning, Op: op.id}, nil
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}
