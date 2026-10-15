package runtime

import (
	"context"
	"testing"
	"time"
)

func TestMockUsageCachesRepeatedSystemPrompt(t *testing.T) {
	m := NewMock()
	session, err := m.StartSession(context.Background(), SessionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	events, err := m.Events(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	task := AgentTask{System: "stable system prefix shared by both turns", Prompt: "Reply"}
	cacheReads := make([]int, 0, 2)
	for i := 0; i < 2; i++ {
		if err := m.SendTask(context.Background(), session, task); err != nil {
			t.Fatal(err)
		}
		deadline := time.After(time.Second)
		for {
			select {
			case event := <-events:
				if event.Done {
					if event.Usage == nil {
						t.Fatal("terminal usage missing")
					}
					cacheReads = append(cacheReads, event.Usage.CacheRead)
					goto nextTurn
				}
			case <-deadline:
				t.Fatal("mock terminal event missing")
			}
		}
	nextTurn:
	}
	if cacheReads[0] != 0 || cacheReads[1] <= cacheReads[0] {
		t.Fatalf("cache reads = %v", cacheReads)
	}
}

func TestMockUsageCachesSystemPromptAcrossSessions(t *testing.T) {
	m := NewMock()
	task := AgentTask{System: "shared prefix across sessions", Prompt: "Reply"}
	var usages []Usage
	for i := 0; i < 2; i++ {
		session, err := m.StartSession(context.Background(), SessionConfig{})
		if err != nil {
			t.Fatal(err)
		}
		events, err := m.Events(context.Background(), session)
		if err != nil {
			t.Fatal(err)
		}
		if err := m.SendTask(context.Background(), session, task); err != nil {
			t.Fatal(err)
		}
		deadline := time.After(time.Second)
		for {
			select {
			case event := <-events:
				if event.Done {
					if event.Usage == nil {
						t.Fatal("mock terminal usage missing")
					}
					usages = append(usages, *event.Usage)
					goto turnDone
				}
			case <-deadline:
				t.Fatal("mock terminal event missing")
			}
		}
	turnDone:
		if err := m.CloseSession(context.Background(), session); err != nil {
			t.Fatal(err)
		}
	}
	if len(usages) != 2 || usages[0].CacheRead != 0 || usages[1].CacheRead <= usages[0].CacheRead {
		t.Fatalf("cross-session usage = %+v", usages)
	}
}
