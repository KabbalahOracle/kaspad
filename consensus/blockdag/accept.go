// Copyright (c) 2013-2017 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package blockdag

import (
	"fmt"
	"github.com/kaspanet/kaspad/consensus/blocknode"
	"github.com/kaspanet/kaspad/consensus/common"
	"github.com/kaspanet/kaspad/consensus/notifications"
	"github.com/kaspanet/kaspad/consensus/validation/blockvalidation"

	"github.com/kaspanet/kaspad/dbaccess"
	"github.com/kaspanet/kaspad/util"
	"github.com/pkg/errors"
)

func (dag *BlockDAG) addNodeToIndexWithInvalidAncestor(block *util.Block) error {
	blockHeader := &block.MsgBlock().Header
	newNode, _ := dag.initBlockNode(blockHeader, blocknode.NewBlockNodeSet())
	newNode.SetStatus(blocknode.StatusInvalidAncestor)
	dag.blockNodeStore.AddNode(newNode)

	dbTx, err := dag.databaseContext.NewTx()
	if err != nil {
		return err
	}
	defer dbTx.RollbackUnlessClosed()
	err = dag.blockNodeStore.FlushToDB(dbTx)
	if err != nil {
		return err
	}
	return dbTx.Commit()
}

// maybeAcceptBlock potentially accepts a block into the block DAG. It
// performs several validation checks which depend on its position within
// the block DAG before adding it. The block is expected to have already
// gone through ProcessBlock before calling this function with it.
//
// The flags are also passed to checkBlockContext and connectToDAG. See
// their documentation for how the flags modify their behavior.
//
// This function MUST be called with the dagLock held (for writes).
func (dag *BlockDAG) maybeAcceptBlock(block *util.Block, flags common.BehaviorFlags) error {
	parents, err := lookupParentNodes(block, dag)
	if err != nil {
		var ruleErr common.RuleError
		if ok := errors.As(err, &ruleErr); ok && ruleErr.ErrorCode == common.ErrInvalidAncestorBlock {
			err := dag.addNodeToIndexWithInvalidAncestor(block)
			if err != nil {
				return err
			}
		}
		return err
	}

	// The block must pass all of the validation rules which depend on the
	// position of the block within the block DAG.
	err = blockvalidation.CheckBlockContext(dag.difficulty, dag.pastMedianTimeFactory, dag.reachabilityTree, block, parents, flags)
	if err != nil {
		return err
	}

	// Create a new block node for the block and add it to the node index.
	newNode, selectedParentAnticone := dag.initBlockNode(&block.MsgBlock().Header, parents)
	newNode.SetStatus(blocknode.StatusDataStored)
	dag.blockNodeStore.AddNode(newNode)

	// Insert the block into the database if it's not already there. Even
	// though it is possible the block will ultimately fail to connect, it
	// has already passed all proof-of-work and validity tests which means
	// it would be prohibitively expensive for an attacker to fill up the
	// disk with a bunch of blocks that fail to connect. This is necessary
	// since it allows block download to be decoupled from the much more
	// expensive connection logic. It also has some other nice properties
	// such as making blocks that never become part of the DAG or
	// blocks that fail to connect available for further analysis.
	dbTx, err := dag.databaseContext.NewTx()
	if err != nil {
		return err
	}
	defer dbTx.RollbackUnlessClosed()
	blockExists, err := dbaccess.HasBlock(dbTx, block.Hash())
	if err != nil {
		return err
	}
	if !blockExists {
		err := storeBlock(dbTx, block)
		if err != nil {
			return err
		}
	}
	err = dag.blockNodeStore.FlushToDB(dbTx)
	if err != nil {
		return err
	}
	err = dbTx.Commit()
	if err != nil {
		return err
	}

	// Make sure that all the block's transactions are finalized
	fastAdd := flags&common.BFFastAdd == common.BFFastAdd
	bluestParent := parents.Bluest()
	if !fastAdd {
		if err := blockvalidation.ValidateAllTxsFinalized(block, newNode, bluestParent, dag.pastMedianTimeFactory); err != nil {
			return err
		}
	}

	// Connect the passed block to the DAG. This also handles validation of the
	// transaction scripts.
	chainUpdates, err := dag.addBlock(newNode, block, selectedParentAnticone, flags)
	if err != nil {
		return err
	}

	// Notify the caller that the new block was accepted into the block
	// DAG. The caller would typically want to react by relaying the
	// inventory to other peers.
	dag.dagLock.Unlock()
	dag.notifier.SendNotification(notifications.NTBlockAdded, &notifications.BlockAddedNotificationData{
		Block:         block,
		WasUnorphaned: flags&common.BFWasUnorphaned != 0,
	})
	if len(chainUpdates.AddedChainBlockHashes) > 0 {
		dag.notifier.SendNotification(notifications.NTChainChanged, &notifications.ChainChangedNotificationData{
			RemovedChainBlockHashes: chainUpdates.RemovedChainBlockHashes,
			AddedChainBlockHashes:   chainUpdates.AddedChainBlockHashes,
		})
	}
	dag.dagLock.Lock()

	return nil
}

func lookupParentNodes(block *util.Block, dag *BlockDAG) (blocknode.BlockNodeSet, error) {
	header := block.MsgBlock().Header
	parentHashes := header.ParentHashes

	nodes := blocknode.NewBlockNodeSet()
	for _, parentHash := range parentHashes {
		node, ok := dag.blockNodeStore.LookupNode(parentHash)
		if !ok {
			str := fmt.Sprintf("parent block %s is unknown", parentHash)
			return nil, common.NewRuleError(common.ErrParentBlockUnknown, str)
		} else if dag.blockNodeStore.NodeStatus(node).KnownInvalid() {
			str := fmt.Sprintf("parent block %s is known to be invalid", parentHash)
			return nil, common.NewRuleError(common.ErrInvalidAncestorBlock, str)
		}

		nodes.Add(node)
	}

	return nodes, nil
}
