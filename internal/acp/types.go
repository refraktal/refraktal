package acp

import (
	"encoding/json"
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
}

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
	json.Unmarshal(params, &p)
	var u struct {
		Kind string `json:"sessionUpdate"`
		Used *int64 `json:"used"`
		Size *int64 `json:"size"`
		Cost *Cost  `json:"cost"`
	}
	json.Unmarshal(p.Update, &u)
	return Update{SessionID: p.SessionID, Kind: u.Kind, Raw: p.Update, Used: u.Used, Size: u.Size, Cost: u.Cost}
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
	Permissions int           // permission requests received during the turn
	Wall        time.Duration // prompt sent to response
}
