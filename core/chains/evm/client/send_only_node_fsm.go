package client

import "fmt"

// SendOnlyNodeState represents the current state of the node
// Node is a FSM (finite state machine)
type SendOnlyNodeState int

func (s SendOnlyNodeState) String() string {
	switch s {
	case SendOnlyNodeStateUndialed:
		return "Undialed"
	case SendOnlyNodeStateDialed:
		return "Dialed"
	case SendOnlyNodeStateInvalidChainID:
		return "InvalidChainID"
	case SendOnlyNodeStateAlive:
		return "Alive"
	case SendOnlyNodeStateUnusable:
		return "Invalid"
	case SendOnlyNodeStateClosed:
		return "Closed"
	default:
		return fmt.Sprintf("SendOnlyNodeState(%d)", s)
	}
}

const (
	// SendOnlyNodeStateUndialed is the first state of a virgin sendonly node
	SendOnlyNodeStateUndialed = SendOnlyNodeState(iota)
	// SendOnlyNodeStateDialed is after a sendonly node has successfully dialed but before it has verified the correct chain ID
	SendOnlyNodeStateDialed
	// SendOnlyNodeStateInvalidChainID is after chain ID verification failed
	SendOnlyNodeStateInvalidChainID
	// SendOnlyNodeStateAlive is a healthy sendonly node after chain ID verification succeeded
	SendOnlyNodeStateAlive
	// SendOnlyNodeStateUnusable is a sendonly sendonly node that has an invalid URL that can never be reached
	SendOnlyNodeStateUnusable
	// SendOnlyNodeStateClosed is after the connection has been closed and the node is at the end of its lifecycle
	SendOnlyNodeStateClosed
)

// GoString prints a prettier state
func (s SendOnlyNodeState) GoString() string {
	return fmt.Sprintf("SendOnlyNodeState%s(%d)", s.String(), s)
}

func (s *sendOnlyNode) setState(state SendOnlyNodeState) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.state = state
}
