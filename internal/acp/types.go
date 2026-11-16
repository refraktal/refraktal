package acp

import (
	"encoding/json"
	"math"
	"time"
)

// Agent is the agent's answer to initialize.
type Agent struct {
	ProtocolVersion int `json:"protocolVersion"`
	Info            struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"agentInfo"`
	Capabilities json.RawMessage `json:"agentCapabilities"`
}

// Session is the agent's answer to session/new.
type Session struct {
	ID    string `json:"sessionId"`
	Modes *struct {
		Current   string `json:"currentModeId"`
		Available []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"availableModes"`
	} `json:"modes,omitempty"`
	ConfigOptions []ConfigOption `json:"configOptions,omitempty"`
}

// ConfigOption is one of a session's settings, such as its model.
type ConfigOption struct {
	ID           string          `json:"id"`
	Category     string          `json:"category"`
	CurrentValue json.RawMessage `json:"currentValue"`
}

// modelOf returns the current model among config options, or "".
func modelOf(opts []ConfigOption) string {
	for _, o := range opts {
		if o.ID == "model" || o.Category == "model" {
			var v string
			if json.Unmarshal(o.CurrentValue, &v) == nil {
				return v
			}
		}
	}
	return ""
}

// Model is the session's model as session/new reported it, or "".
func (s Session) Model() string { return modelOf(s.ConfigOptions) }

// Cost is a cost the harness reports. Claude's is cumulative for the session.
type Cost struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
}

// Usage is the token count on a prompt response, for that turn only. It is
// unstable in ACP v1, so any field may be missing.
type Usage struct {
	TotalTokens       int64  `json:"totalTokens"`
	InputTokens       int64  `json:"inputTokens"`
	OutputTokens      int64  `json:"outputTokens"`
	ThoughtTokens     *int64 `json:"thoughtTokens,omitempty"`
	CachedReadTokens  *int64 `json:"cachedReadTokens,omitempty"`
	CachedWriteTokens *int64 `json:"cachedWriteTokens,omitempty"`
}

// Update is one session/update notification.
type Update struct {
	SessionID string
	Kind      string          // the sessionUpdate field, such as agent_message_chunk
	Raw       json.RawMessage // the whole update object
	Used      *int64          // usage_update: context fill, not a billing figure
	Size      *int64          // usage_update: context size
	Cost      *Cost           // usage_update: set only when the harness reports a cost
}

func parseUpdate(params json.RawMessage) Update {
	var p struct {
		SessionID string          `json:"sessionId"`
		Update    json.RawMessage `json:"update"`
	}
	if json.Unmarshal(params, &p) != nil {
		return Update{}
	}
	var k struct {
		Kind string `json:"sessionUpdate"`
	}
	if json.Unmarshal(p.Update, &k) != nil {
		return Update{}
	}
	out := Update{SessionID: p.SessionID, Kind: k.Kind, Raw: p.Update}
	// Accounting is all or nothing: if any of used, size or cost is malformed
	// or out of range, none of them is applied. The update itself still
	// reaches the callbacks and the log, so a harness's odd number is visible.
	var a struct {
		Used *int64 `json:"used"`
		Size *int64 `json:"size"`
		Cost *struct {
			Amount   *float64 `json:"amount"`
			Currency string   `json:"currency"`
		} `json:"cost"`
	}
	if json.Unmarshal(p.Update, &a) != nil ||
		(a.Used != nil && *a.Used < 0) || (a.Size != nil && *a.Size < 0) ||
		(a.Cost != nil && (a.Cost.Amount == nil || *a.Cost.Amount < 0 || math.IsNaN(*a.Cost.Amount) || math.IsInf(*a.Cost.Amount, 0))) {
		return out
	}
	out.Used, out.Size = a.Used, a.Size
	if a.Cost != nil {
		out.Cost = &Cost{Amount: *a.Cost.Amount, Currency: a.Cost.Currency}
	}
	return out
}

// PermissionOption is one choice in a permission request.
type PermissionOption struct {
	ID   string `json:"optionId"`
	Name string `json:"name"`
	Kind string `json:"kind"` // allow_once, allow_always, reject_once, reject_always
}

// PermissionRequest is a session/request_permission from the agent.
type PermissionRequest struct {
	SessionID string `json:"sessionId"`
	ToolCall  struct {
		ID       string          `json:"toolCallId"`
		Title    string          `json:"title"`
		Kind     string          `json:"kind"`
		RawInput json.RawMessage `json:"rawInput,omitempty"`
	} `json:"toolCall"`
	Options []PermissionOption `json:"options"`
	Raw     json.RawMessage    `json:"-"`
}

// Option returns the ID of the first option of the given kind, or "".
func (r PermissionRequest) Option(kind string) string {
	for _, o := range r.Options {
		if o.Kind == kind {
			return o.ID
		}
	}
	return ""
}

func (r PermissionRequest) reject() string {
	if id := r.Option("reject_once"); id != "" {
		return id
	}
	return r.Option("reject_always")
}

// Turn is the result of one prompt.
type Turn struct {
	StopReason  string        // end_turn, cancelled, max_tokens, refusal and so on
	Cancelled   bool          // session/cancel was sent
	Usage       *Usage        // from the prompt response; nil if the harness sent none
	LastUsed    *int64        // last usage_update "used" during the turn: context fill, never bill from it
	Cost        *Cost         // last cost reported during the turn
	Model       string        // the session's model when the turn ended, if the harness reports it
	Permissions int           // permission requests received during the turn
	Wall        time.Duration // prompt sent to response
	Child       *ChildReport  // a child Refraktal's report in the response's _meta, if any
}

// ChildReport is what a child Refraktal adds to its prompt response, under
// _meta.refraktal, so the parent can count its prompts across the tree.
type ChildReport struct {
	Turns   int    `json:"turns"`   // prompts the child sent to its own harnesses
	Outcome string `json:"outcome"` // the child run's outcome
	Exit    int    `json:"exit"`    // the child run's exit code
	Run     string `json:"run"`     // the child's run folder
	// Retained is US dollars the child's cancelled children keep reserved,
	// beyond what they spent: the parent holds it against --usd without
	// reporting it as spent.
	Retained float64 `json:"retained_usd,omitempty"`
}
