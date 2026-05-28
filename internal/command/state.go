package command

import (
	"context"

	"github.com/schmidt/agentgram/internal/router"
)

// StateAcceptor tries to handle the message as part of a pending state.
// handled=true means: "this message was captured, don't send it further".
type StateAcceptor interface {
	AcceptText(ctx context.Context, msg router.Message, r Replier) (handled bool, err error)
}

// StateChain — sequence of StateAcceptors. Implements
// router.StateAcceptor: tries each in turn.
//
// Commands that have their own pending state (e.g. SettingsCommand
// waiting for user_id to add), reset it themselves in callback handlers —
// a common ResetAll mechanism isn't needed for now.
type StateChain struct {
	acceptors []StateAcceptor
	replier   Replier
}

func NewStateChain(r Replier) *StateChain {
	return &StateChain{replier: r}
}

func (sc *StateChain) Add(a StateAcceptor) {
	sc.acceptors = append(sc.acceptors, a)
}

// TryHandle implements router.StateAcceptor.
func (sc *StateChain) TryHandle(ctx context.Context, msg router.Message) (bool, error) {
	for _, a := range sc.acceptors {
		if handled, err := a.AcceptText(ctx, msg, sc.replier); handled {
			return true, err
		}
	}
	return false, nil
}
