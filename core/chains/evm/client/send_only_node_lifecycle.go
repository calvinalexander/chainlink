package client

import (
	"context"
	"fmt"
	"time"

	"github.com/smartcontractkit/chainlink/core/utils"
)

// verifyLoop may only be triggered once, on Start, if initial chain ID check
// fails.
//
// It will continue checking until success and then exit permanently.
func (s *sendOnlyNode) verifyLoop() {
	defer s.wg.Done()

	backoff := utils.NewRedialBackoff()
	for {
		select {
		case <-time.After(backoff.Duration()):
			chainID, err := s.sender.ChainID(context.Background())
			if err != nil {
				// TODO: prom metric?
				s.log.Errorw(fmt.Sprintf("Verify failed: %v", err), "err", err)
				continue
			} else if chainID.Cmp(s.chainID) != 0 {
				// TODO: prom metric?
				ok := s.IfStarted(func() {
					s.setState(SendOnlyNodeStateInvalidChainID)
				})
				if !ok {
					return
				}
				s.log.Errorf(
					"sendonly rpc ChainID doesn't match local chain ID: RPC ID=%s, local ID=%s, node name=%s",
					chainID.String(),
					s.chainID.String(),
					s.name,
				)

				continue
			} else {
				ok := s.IfStarted(func() {
					s.setState(SendOnlyNodeStateAlive)
				})
				if !ok {
					return
				}
				s.log.Infow("Sendonly RPC Node is online", "nodeState", s.state)
				return
			}
		case <-s.chStop:
			return
		}
	}
}
