package mcptransport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

var (
	ErrBadParam  = errors.New("mcptransport: bad parameter")
	ErrDuplicate = errors.New("mcptransport: duplicate tool name")
	ErrUnknown   = errors.New("mcptransport: unknown tool")
)

// Tool is a tool definition as returned by tools/list.
type Tool struct {
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	InputSchema Schema `json:"inputSchema"`
}

// Call is a tools/call invocation handed to a Handler.
type Call struct {
	Name      string
	Arguments json.RawMessage
}

// Handler processes one tool call. Return a non-nil error for protocol-level
// failures (bad arguments, internal error); use (*ToolResult).IsError for
// tool-execution errors the model can recover from.
type Handler func(ctx context.Context, call *Call) (*ToolResult, error)

// Registry maps tool names to handlers and lists tools for tools/list.
type Registry struct {
	prefix   string
	tools    []*Tool
	handlers map[string]Handler
}

// NewRegistry returns a Registry that prefixes every registered tool name
// with prefix (empty prefix = names unchanged).
func NewRegistry(prefix string) *Registry {
	return &Registry{
		prefix:   prefix,
		tools:    make([]*Tool, 0, 16),
		handlers: make(map[string]Handler),
	}
}

// Register adds a tool and its handler. The tool name is prefixed in place.
func (r *Registry) Register(t *Tool, h Handler) error {
	if t == nil || t.Name == "" || h == nil {
		return ErrBadParam
	}
	name := r.prefix + t.Name
	if _, ok := r.handlers[name]; ok {
		return fmt.Errorf("%w: %s", ErrDuplicate, name)
	}
	t.Name = name
	r.tools = append(r.tools, t)
	r.handlers[name] = h
	return nil
}

// Dispatch runs the handler for call.Name.
func (r *Registry) Dispatch(ctx context.Context, call *Call) (*ToolResult, error) {
	h, ok := r.handlers[call.Name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknown, call.Name)
	}
	return h(ctx, call)
}

// List returns the registered tools in registration order (deterministic).
func (r *Registry) List() []*Tool {
	out := make([]*Tool, len(r.tools))
	copy(out, r.tools)
	return out
}
