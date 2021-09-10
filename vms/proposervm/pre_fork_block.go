// (c) 2021, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package proposervm

import (
	"errors"
	"time"

	"github.com/ava-labs/avalanchego/database"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/choices"
	"github.com/ava-labs/avalanchego/snow/consensus/snowman"
	"github.com/ava-labs/avalanchego/vms/proposervm/block"
)

var (
	errPChainHeightTooLow = errors.New("block P-chain height is too low")

	_ Block = &preForkBlock{}
)

// preForkBlock implements proposervm.Block
type preForkBlock struct {
	snowman.Block
	vm *VM
}

func (b *preForkBlock) Parent() ids.ID {
	return b.Block.Parent()
}

func (b *preForkBlock) Verify() error {
	parent, err := b.vm.getBlock(b.Block.Parent())
	if err != nil {
		return err
	}
	return parent.verifyPreForkChild(b)
}

func (b *preForkBlock) Options() ([2]snowman.Block, error) {
	oracleBlk, ok := b.Block.(snowman.OracleBlock)
	if !ok {
		return [2]snowman.Block{}, snowman.ErrNotOracle
	}

	options, err := oracleBlk.Options()
	if err != nil {
		return [2]snowman.Block{}, err
	}
	// A pre-fork block's child options are always pre-fork blocks
	return [2]snowman.Block{
		&preForkBlock{
			Block: options[0],
			vm:    b.vm,
		},
		&preForkBlock{
			Block: options[1],
			vm:    b.vm,
		},
	}, nil
}

func (b *preForkBlock) getInnerBlk() snowman.Block {
	return b.Block
}

func (b *preForkBlock) verifyPreForkChild(child *preForkBlock) error {
	parentTimestamp := b.Timestamp()
	if !parentTimestamp.Before(b.vm.activationTime) {
		if err := verifyIsOracleBlock(b.Block); err != nil {
			return err
		}

		if err := b.verifyIsPreForkBlock(); err != nil {
			return err
		}

		b.vm.ctx.Log.Debug("allowing pre-fork block %s after the fork time because the parent is an oracle block",
			b.ID())
	}

	return child.Block.Verify()
}

// This method only returns nil once (during the transition)
func (b *preForkBlock) verifyPostForkChild(child *postForkBlock) error {
	if err := verifyIsNotOracleBlock(b.Block); err != nil {
		return err
	}

	if err := b.verifyIsPreForkBlock(); err != nil {
		return err
	}

	childID := child.ID()
	childPChainHeight := child.PChainHeight()
	currentPChainHeight, err := b.vm.ctx.ValidatorState.GetCurrentHeight()
	if err != nil {
		b.vm.ctx.Log.Error("couldn't retrieve current P-Chain height while verifying %s: %s", childID, err)
		return err
	}
	if childPChainHeight > currentPChainHeight {
		return errPChainHeightNotReached
	}
	if childPChainHeight < b.vm.minimumPChainHeight {
		return errPChainHeightTooLow
	}

	// Make sure [b] is the parent of [child]'s inner block
	expectedInnerParentID := b.ID()
	innerParentID := child.innerBlk.Parent()
	if innerParentID != expectedInnerParentID {
		return errInnerParentMismatch
	}

	// A *preForkBlock can only have a *postForkBlock child
	// if the *preForkBlock is the last *preForkBlock before activation takes effect
	// (its timestamp is at or after the activation time)
	parentTimestamp := b.Timestamp()
	if parentTimestamp.Before(b.vm.activationTime) {
		return errProposersNotActivated
	}

	// Child's timestamp must be at or after its parent's timestamp
	childTimestamp := child.Timestamp()
	if childTimestamp.Before(parentTimestamp) {
		return errTimeNotMonotonic
	}

	// Child timestamp can't be too far in the future
	maxTimestamp := b.vm.Time().Add(maxSkew)
	if childTimestamp.After(maxTimestamp) {
		return errTimeTooAdvanced
	}

	// Verify the lack of signature on the node
	if err := child.SignedBlock.Verify(false, b.vm.ctx.ChainID); err != nil {
		return err
	}

	// If inner block's Verify returned true, don't call it again.
	// Note that if [child.innerBlk.Verify] returns nil,
	// this method returns nil. This must always remain the case to
	// maintain the inner block's invariant that if it's Verify()
	// returns nil, it is eventually accepted/rejected.
	if !b.vm.Tree.Contains(child.innerBlk) {
		if err := child.innerBlk.Verify(); err != nil {
			return err
		}
		b.vm.Tree.Add(child.innerBlk)
	}

	b.vm.verifiedBlocks[childID] = child
	return nil
}

func (b *preForkBlock) verifyPostForkOption(child *postForkOption) error {
	return errUnexpectedBlockType
}

func (b *preForkBlock) buildChild(innerBlock snowman.Block) (Block, error) {
	parentTimestamp := b.Timestamp()
	if parentTimestamp.Before(b.vm.activationTime) {
		// The chain hasn't forked yet
		res := &preForkBlock{
			Block: innerBlock,
			vm:    b.vm,
		}

		b.vm.ctx.Log.Info("built block %s - parent timestamp %v",
			res.ID(), parentTimestamp)
		return res, nil
	}

	// The chain is currently forking

	parentID := b.ID()
	newTimestamp := b.vm.Time().Truncate(time.Second)
	if newTimestamp.Before(parentTimestamp) {
		newTimestamp = parentTimestamp
	}

	pChainHeight, err := b.vm.ctx.ValidatorState.GetCurrentHeight()
	if err != nil {
		return nil, err
	}

	statelessBlock, err := block.BuildUnsigned(
		parentID,
		newTimestamp,
		pChainHeight,
		innerBlock.Bytes(),
	)
	if err != nil {
		return nil, err
	}

	blk := &postForkBlock{
		SignedBlock: statelessBlock,
		postForkCommonComponents: postForkCommonComponents{
			vm:       b.vm,
			innerBlk: innerBlock,
			status:   choices.Processing,
		},
	}

	b.vm.ctx.Log.Info("built block %s - parent timestamp %v, block timestamp %v",
		blk.ID(), parentTimestamp, newTimestamp)
	return blk, b.vm.storePostForkBlock(blk)
}

func (b *preForkBlock) pChainHeight() (uint64, error) {
	return 0, nil
}

func (b *preForkBlock) verifyIsPreForkBlock() error {
	if status := b.Status(); status == choices.Accepted {
		_, err := b.vm.GetLastAccepted()
		if err == nil {
			// If this block is accepted and it was a preForkBlock, then there
			// shouldn't have been an accepted postForkBlock yet. If there was
			// an accepted postForkBlock, then this block wasn't a preForkBlock.
			return errUnexpectedBlockType
		}
		if err != database.ErrNotFound {
			// If an unexpected error was returned - propagate that that
			// error.
			return err
		}
	} else if b.vm.Tree.Contains(b.Block) {
		// If this block is a preForkBlock, then it's inner block shouldn't have
		// been registered into the inner block tree. If this block was
		// registered into the inner block tree, then it wasn't a preForkBlock.
		return errUnexpectedBlockType
	}
	return nil
}