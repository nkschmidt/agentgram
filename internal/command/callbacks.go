package command

import (
	"context"
	"log"
	"strings"

	"github.com/schmidt/agentgram/internal/router"
)

// CallbackPrefixHandler — handler for callback queries with the given prefix
// in callback_data (e.g. "settings"). Full data like "settings:open"
// is routed here, parsing the suffix is the handler's concern.
type CallbackPrefixHandler interface {
	Prefix() string
	HandleCallback(ctx context.Context, cb router.Callback, r Replier) error
}

// CallbackRouter routes callbacks by the first segment of data (before ":").
// Implements router.CallbackHandler.
type CallbackRouter struct {
	handlers map[string]CallbackPrefixHandler
	replier  Replier
}

func NewCallbackRouter(r Replier) *CallbackRouter {
	return &CallbackRouter{handlers: map[string]CallbackPrefixHandler{}, replier: r}
}

func (cr *CallbackRouter) Register(h CallbackPrefixHandler) {
	cr.handlers[h.Prefix()] = h
}

// Handle parses the prefix and delegates. Unknown prefix — log + ack,
// so the user doesn't see a spinner stuck on the button.
func (cr *CallbackRouter) Handle(ctx context.Context, cb router.Callback) error {
	prefix := cb.Data
	if i := strings.Index(cb.Data, ":"); i >= 0 {
		prefix = cb.Data[:i]
	}
	h, ok := cr.handlers[prefix]
	if !ok {
		log.Printf("callback: unknown prefix %q (data=%q)", prefix, cb.Data)
		return cr.replier.Answer(ctx, cb.ID, "")
	}
	return h.HandleCallback(ctx, cb, cr.replier)
}
