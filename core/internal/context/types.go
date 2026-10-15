package ctxeng

import "time"

type Class string

const (
	ClassEphemeral Class = "ephemeral"
	ClassTask      Class = "task_state"
	ClassFinding   Class = "finding"
	ClassMemory    Class = "memory"
	ClassHistory   Class = "history"
	ClassCode      Class = "code"
	ClassReport    Class = "report"
)

// Valid reports whether c is one of the classes the engine knows.
func (c Class) Valid() bool {
	switch c {
	case ClassEphemeral, ClassTask, ClassFinding, ClassMemory, ClassHistory, ClassCode, ClassReport:
		return true
	}
	return false
}

type Source string

const (
	SourceUser       Source = "user_requirement"
	SourceVerified   Source = "verified_fact"
	SourceWorker     Source = "worker_finding"
	SourceHypothesis Source = "unverified_hypothesis"
	SourceSummary    Source = "generated_summary"
	SourceTest       Source = "test_result"
	SourceRepo       Source = "repository_source"
	SourceParent     Source = "parent_report"
	SourceSystem     Source = "system"
)

type Phase string

const (
	PhaseExplore   Phase = "explore"
	PhaseDesign    Phase = "design"
	PhaseImplement Phase = "implement"
	PhaseDebug     Phase = "debug"
	PhaseVerify    Phase = "verify"
	PhaseReview    Phase = "review"
	PhaseIntegrate Phase = "integrate"
)

type Item struct {
	ID         string    `json:"id"`
	Class      Class     `json:"class"`
	Source     Source    `json:"source"`
	Content    string    `json:"content"`
	AgentID    string    `json:"agentId"`
	TaskID     string    `json:"taskId"`
	Created    time.Time `json:"created"`
	Confidence float64   `json:"confidence"`
	Relevance  float64   `json:"relevance"`
	Durability float64   `json:"durability"`
	Verified   bool      `json:"verified"`
	Pinned     bool      `json:"pinned"`
	Tokens     int       `json:"tokens"`
	Files      []string  `json:"files,omitempty"`
	Symbols    []string  `json:"symbols,omitempty"`
	Phase      Phase     `json:"phase,omitempty"`
	Kind       string    `json:"kind,omitempty"`
	Title      string    `json:"title,omitempty"`
	FindingID  string    `json:"findingId,omitempty"`
}

type Dropped struct {
	Item   Item   `json:"item"`
	Reason string `json:"reason"`
}

type Compressed struct {
	From    string `json:"from"`
	Content string `json:"content"`
	Tokens  int    `json:"tokens"`
}

type Generation struct {
	Number         int          `json:"number"`
	AgentID        string       `json:"agentId"`
	TaskID         string       `json:"taskId"`
	Phase          Phase        `json:"phase"`
	Reason         string       `json:"reason"`
	Strategy       string       `json:"strategy"`
	Items          []Item       `json:"items"`
	Dropped        []Dropped    `json:"dropped"`
	Compressed     []Compressed `json:"compressed"`
	BuiltAt        time.Time    `json:"builtAt"`
	Tokens         int          `json:"tokens"`
	Budget         int          `json:"budget"`
	OverflowTokens int          `json:"overflowTokens,omitempty"`
	// Deferred lists stable-class item IDs first selected by a non-boundary build. They
	// render in the dynamic section so the Static block stays byte-identical until the
	// next boundary build (refresh/replace/limit refresh) clears this list.
	Deferred []string `json:"deferred,omitempty"`
}

func (g *Generation) isDeferred(id string) bool {
	if g == nil {
		return false
	}
	for _, d := range g.Deferred {
		if d == id {
			return true
		}
	}
	return false
}

type Metrics struct {
	Generations     int            `json:"generations"`
	Refreshes       int            `json:"refreshes"`
	InputTokens     int            `json:"inputTokens"`
	OutputTokens    int            `json:"outputTokens"`
	Utilization     float64        `json:"utilization"`
	Repeats         map[string]int `json:"repeats,omitempty"`
	FailedEdits     int            `json:"failedEdits"`
	LastProgress    time.Time      `json:"lastProgress"`
	RefreshReasons  []string       `json:"refreshReasons,omitempty"`
	RepeatedActions int            `json:"repeatedActions"`
}

type Query struct {
	Task     string
	Files    []string
	Symbols  []string
	Keywords []string
	Role     string
	Phase    Phase
	Failures bool
}

type BuildRequest struct {
	AgentID           string
	TaskID            string
	Role              string
	Phase             Phase
	Budget            int
	Seed              []Item
	Query             Query
	Reason            string
	Strategy          string
	Scope             *Scope
	DependencyTaskIDs []string
}

type Scope struct {
	MaxItemTokens         int
	Classes               []Class
	Memory                bool
	DependencyReportsOnly bool
	ReadOnly              bool
}

type Inspection struct {
	AgentID     string         `json:"agentId"`
	Generation  int            `json:"generation"`
	Used        int            `json:"usedTokens"`
	Budget      int            `json:"budget"`
	Phase       Phase          `json:"phase"`
	Included    []Item         `json:"included"`
	Excluded    []Item         `json:"excluded"`
	Composition map[string]int `json:"composition"`
	Reason      string         `json:"refreshReason,omitempty"`
	Metrics     Metrics        `json:"metrics"`
}

type Finding struct {
	ID         string    `json:"id"`
	TaskID     string    `json:"taskId"`
	AgentID    string    `json:"agentId"`
	Kind       string    `json:"kind"`
	Summary    string    `json:"summary"`
	Evidence   []string  `json:"evidence"`
	RootCause  string    `json:"rootCause,omitempty"`
	Detail     string    `json:"detail,omitempty"`
	Files      []string  `json:"files,omitempty"`
	Symbols    []string  `json:"symbols,omitempty"`
	Confidence float64   `json:"confidence"`
	Verified   bool      `json:"verified"`
	Created    time.Time `json:"created"`
}

const (
	KindArchitecture = "architecture"
	KindBug          = "bug"
	KindDependency   = "dependency"
	KindConstraint   = "constraint"
	KindTest         = "test"
	KindPerformance  = "performance"
	KindSecurity     = "security"
	KindAPI          = "api"
	KindSymbol       = "symbol"
)

func Priority(it Item) int {
	if it.Pinned {
		return 0
	}
	if it.Source == SourceUser || it.Kind == KindConstraint {
		return 1
	}
	if it.Class == ClassTask {
		return 1
	}
	if it.Class == ClassCode || (it.Class == ClassFinding && it.Verified) || it.Source == SourceTest {
		return 2
	}
	if it.Class == ClassMemory || it.Class == ClassReport || it.Source == SourceParent {
		return 3
	}
	return 4
}

func Durable(it Item) bool {
	if it.Pinned || it.Verified {
		return true
	}
	switch it.Class {
	case ClassTask, ClassFinding, ClassMemory, ClassReport:
		return true
	}
	if it.Source == SourceUser || it.Source == SourceVerified || it.Source == SourceTest {
		return true
	}
	return it.Durability >= 0.7
}
