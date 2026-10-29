package metrics

import (
	"sort"
	"sync"
)

const (
	DefaultCacheWriteWeight = 1.25
	DefaultCacheReadWeight  = 0.1
)

type Record struct {
	Session    string `json:"sessionId"`
	Task       string `json:"taskId"`
	Agent      string `json:"agentId"`
	Model      string `json:"model"`
	Strategy   string `json:"strategy"`
	Generation int    `json:"generation"`
	TokensIn   int    `json:"inputTokens"`
	TokensOut  int    `json:"outputTokens"`
	CacheRead  int    `json:"cacheRead,omitempty"`
	CacheWrite int    `json:"cacheWrite,omitempty"`
	ContextTok int    `json:"contextTokens"`
	Refreshes  int    `json:"refreshes"`
	Reason     string `json:"refreshReason,omitempty"`
	Success    bool   `json:"success"`
	Retries    int    `json:"retries"`
	ElapsedMS  int64  `json:"elapsedMs"`
	Verified   bool   `json:"verificationSuccess"`
	Repeats    int    `json:"repeatedActions"`
}

type Usage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	Reasoning  int `json:"reasoning"`
	CacheRead  int `json:"cacheRead"`
	CacheWrite int `json:"cacheWrite"`
}

type CacheRow struct {
	AgentID       string  `json:"agentId,omitempty"`
	Generation    int     `json:"generation,omitempty"`
	Model         string  `json:"model,omitempty"`
	Role          string  `json:"role"`
	Profile       string  `json:"profile"`
	Usage         Usage   `json:"usage"`
	HitRate       float64 `json:"hitRate"`
	WeightedInput float64 `json:"weightedInput"`
}

type CacheSnapshot struct {
	Agents  []CacheRow `json:"agents"`
	Session CacheRow   `json:"session"`
}

type CacheStats struct {
	mu       sync.RWMutex
	rows     map[string]CacheRow
	all      Usage
	weighted float64
}

func (s *CacheStats) Add(agentID string, generation int, model string, u Usage) {
	s.AddLabeled(agentID, generation, model, "", "", u)
}

func (s *CacheStats) AddLabeled(agentID string, generation int, model, role, profile string, u Usage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rows == nil {
		s.rows = map[string]CacheRow{}
	}
	r := s.rows[agentID]
	r.AgentID, r.Generation, r.Model, r.Role, r.Profile = agentID, generation, model, role, profile
	r.Usage = addUsage(r.Usage, u)
	r.HitRate = hitRate(r.Usage)
	r.WeightedInput = WeightedInput(r.Usage, DefaultCacheWriteWeight, DefaultCacheReadWeight)
	s.rows[agentID] = r
	s.all = addUsage(s.all, u)
	s.weighted += WeightedInput(u, DefaultCacheWriteWeight, DefaultCacheReadWeight)
}

func (s *CacheStats) HitRate(agentID string) float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var u Usage
	for _, r := range s.rows {
		if r.AgentID == agentID {
			u = addUsage(u, r.Usage)
		}
	}
	return hitRate(u)
}

func (s *CacheStats) SessionHitRate() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return hitRate(s.all)
}

func (s *CacheStats) Snapshot() CacheSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := CacheSnapshot{Session: CacheRow{Usage: s.all, HitRate: hitRate(s.all), WeightedInput: s.weighted}}
	for _, r := range s.rows {
		out.Agents = append(out.Agents, r)
	}
	sort.Slice(out.Agents, func(i, j int) bool { return out.Agents[i].AgentID < out.Agents[j].AgentID })
	return out
}

func WeightedInput(u Usage, wWrite, wRead float64) float64 {
	return float64(u.Input) + float64(u.CacheWrite)*wWrite + float64(u.CacheRead)*wRead
}

func addUsage(a, b Usage) Usage {
	return Usage{Input: a.Input + b.Input, Output: a.Output + b.Output, Reasoning: a.Reasoning + b.Reasoning, CacheRead: a.CacheRead + b.CacheRead, CacheWrite: a.CacheWrite + b.CacheWrite}
}

// UsedTokens is what a call spends: uncached input, output and reasoning. CacheRead and CacheWrite are
// reported separately and never counted here.
func (u Usage) UsedTokens() int { return u.Input + u.Output + u.Reasoning }

// Payload is the cache.updated and cache.get shape: session totals plus one row per agent.
func (s CacheSnapshot) Payload() map[string]any {
	agents := make([]map[string]any, 0, len(s.Agents))
	for _, r := range s.Agents {
		row := usagePayload(r)
		row["agentId"], row["generation"], row["model"], row["role"], row["profile"] = r.AgentID, r.Generation, r.Model, r.Role, r.Profile
		agents = append(agents, row)
	}
	return map[string]any{"session": usagePayload(s.Session), "agents": agents}
}

func usagePayload(r CacheRow) map[string]any {
	return map[string]any{"hitRate": r.HitRate, "input": r.Usage.Input, "output": r.Usage.Output, "reasoning": r.Usage.Reasoning, "cacheRead": r.Usage.CacheRead, "cacheWrite": r.Usage.CacheWrite}
}

// Hit rate = CacheRead / (Input + CacheRead + CacheWrite); zero denominator yields zero.
func hitRate(u Usage) float64 {
	total := u.Input + u.CacheRead + u.CacheWrite
	if total == 0 {
		return 0
	}
	return float64(u.CacheRead) / float64(total)
}
