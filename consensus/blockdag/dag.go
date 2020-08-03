// Copyright (c) 2013-2017 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package blockdag

import (
	"fmt"
	"github.com/kaspanet/kaspad/consensus/blocklocator"
	"github.com/kaspanet/kaspad/consensus/blocknode"
	"github.com/kaspanet/kaspad/consensus/coinbase"
	"github.com/kaspanet/kaspad/consensus/common"
	"github.com/kaspanet/kaspad/consensus/delayedblocks"
	"github.com/kaspanet/kaspad/consensus/ghostdag"
	"github.com/kaspanet/kaspad/consensus/merkle"
	"github.com/kaspanet/kaspad/consensus/multiset"
	"github.com/kaspanet/kaspad/consensus/notifications"
	"github.com/kaspanet/kaspad/consensus/reachability"
	"github.com/kaspanet/kaspad/consensus/subnetworks"
	"github.com/kaspanet/kaspad/consensus/timesource"
	"github.com/kaspanet/kaspad/consensus/utxo"
	"github.com/kaspanet/kaspad/consensus/utxodiffstore"
	"github.com/kaspanet/kaspad/consensus/virtualblock"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/kaspanet/kaspad/util/mstime"

	"github.com/kaspanet/kaspad/dbaccess"

	"github.com/pkg/errors"

	"github.com/kaspanet/kaspad/util/subnetworkid"

	"github.com/kaspanet/go-secp256k1"
	"github.com/kaspanet/kaspad/consensus/txscript"
	"github.com/kaspanet/kaspad/dagconfig"
	"github.com/kaspanet/kaspad/util"
	"github.com/kaspanet/kaspad/util/daghash"
	"github.com/kaspanet/kaspad/wire"
)

const (
	// maxOrphanBlocks is the maximum number of orphan blocks that can be
	// queued.
	maxOrphanBlocks = 100

	// isDAGCurrentMaxDiff is the number of blocks from the network tips (estimated by timestamps) for the current
	// to be considered not synced
	isDAGCurrentMaxDiff = 40_000
)

// orphanBlock represents a block that we don't yet have the parent for. It
// is a normal block plus an expiration time to prevent caching the orphan
// forever.
type orphanBlock struct {
	block      *util.Block
	expiration mstime.Time
}

// BlockDAG provides functions for working with the kaspa block DAG.
// It includes functionality such as rejecting duplicate blocks, ensuring blocks
// follow all rules, and orphan handling.
type BlockDAG struct {
	// The following fields are set when the instance is created and can't
	// be changed afterwards, so there is no need to protect them with a
	// separate mutex.
	Params              *dagconfig.Params
	databaseContext     *dbaccess.DatabaseContext
	timeSource          timesource.TimeSource
	sigCache            *txscript.SigCache
	indexManager        IndexManager
	genesis             *blocknode.BlockNode
	notifier            *notifications.ConsensusNotifier
	coinbase            *coinbase.Coinbase
	ghostdag            *ghostdag.GHOSTDAG
	blockLocatorFactory *blocklocator.BlockLocatorFactory

	// powMaxBits defines the highest allowed proof of work value for a
	// block in compact form.
	powMaxBits uint32

	// dagLock protects concurrent access to the vast majority of the
	// fields in this struct below this point.
	dagLock sync.RWMutex

	utxoLock sync.RWMutex

	// index and virtual are related to the memory block index. They both
	// have their own locks, however they are often also protected by the
	// DAG lock to help prevent logic races when blocks are being processed.

	// index houses the entire block index in memory. The block index is
	// a tree-shaped structure.
	blockNodeStore *blocknode.BlockNodeStore

	// blockCount holds the number of blocks in the DAG
	blockCount uint64

	// virtual tracks the current tips.
	virtual *virtualblock.VirtualBlock

	// subnetworkID holds the subnetwork ID of the DAG
	subnetworkID *subnetworkid.SubnetworkID

	// These fields are related to handling of orphan blocks. They are
	// protected by a combination of the DAG lock and the orphan lock.
	orphanLock   sync.RWMutex
	orphans      map[daghash.Hash]*orphanBlock
	prevOrphans  map[daghash.Hash][]*orphanBlock
	newestOrphan *orphanBlock

	delayedBlocks *delayedblocks.DelayedBlocks

	// The following caches are used to efficiently keep track of the
	// current deployment threshold state of each rule change deployment.
	//
	// This information is stored in the database so it can be quickly
	// reconstructed on load.
	//
	// warningCaches caches the current deployment threshold state for blocks
	// in each of the **possible** deployments. This is used in order to
	// detect when new unrecognized rule changes are being voted on and/or
	// have been activated such as will be the case when older versions of
	// the software are being used
	//
	// deploymentCaches caches the current deployment threshold state for
	// blocks in each of the actively defined deployments.
	warningCaches    []thresholdStateCache
	deploymentCaches []thresholdStateCache

	// The following fields are used to determine if certain warnings have
	// already been shown.
	//
	// unknownRulesWarned refers to warnings due to unknown rules being
	// activated.
	//
	// unknownVersionsWarned refers to warnings due to unknown versions
	// being mined.
	unknownRulesWarned    bool
	unknownVersionsWarned bool

	lastFinalityPoint *blocknode.BlockNode

	utxoDiffStore *utxodiffstore.UtxoDiffStore
	multisetStore *multiset.MultisetStore

	reachabilityTree *reachability.ReachabilityTree

	recentBlockProcessingTimestamps []mstime.Time
	startTime                       mstime.Time
}

// New returns a BlockDAG instance using the provided configuration details.
func New(config *Config) (*BlockDAG, error) {
	// Enforce required config fields.
	if config.DAGParams == nil {
		return nil, errors.New("BlockDAG.New DAG parameters nil")
	}
	if config.TimeSource == nil {
		return nil, errors.New("BlockDAG.New timesource is nil")
	}
	if config.DatabaseContext == nil {
		return nil, errors.New("BlockDAG.DatabaseContext timesource is nil")
	}

	params := config.DAGParams

	blockNodeStore := blocknode.NewBlockNodeStore(params)
	dag := &BlockDAG{
		Params:           params,
		databaseContext:  config.DatabaseContext,
		timeSource:       config.TimeSource,
		sigCache:         config.SigCache,
		indexManager:     config.IndexManager,
		powMaxBits:       util.BigToCompact(params.PowMax),
		blockNodeStore:   blockNodeStore,
		orphans:          make(map[daghash.Hash]*orphanBlock),
		prevOrphans:      make(map[daghash.Hash][]*orphanBlock),
		delayedBlocks:    delayedblocks.New(),
		warningCaches:    newThresholdCaches(vbNumBits),
		deploymentCaches: newThresholdCaches(dagconfig.DefinedDeployments),
		blockCount:       0,
		subnetworkID:     config.SubnetworkID,
		startTime:        mstime.Now(),
		notifier:         notifications.New(),
		coinbase:         coinbase.New(config.DatabaseContext, params),
	}

	dag.multisetStore = multiset.NewMultisetStore()
	dag.reachabilityTree = reachability.NewReachabilityTree(blockNodeStore, params)
	dag.ghostdag = ghostdag.NewGHOSTDAG(dag.reachabilityTree, params, dag.timeSource)
	dag.virtual = virtualblock.NewVirtualBlock(dag.ghostdag, params, dag.blockNodeStore, nil)
	dag.blockLocatorFactory = blocklocator.NewBlockLocatorFactory(dag.blockNodeStore, params)
	dag.utxoDiffStore = utxodiffstore.NewUTXODiffStore(dag.databaseContext, blockNodeStore, dag.virtual)

	// Initialize the DAG state from the passed database. When the db
	// does not yet contain any DAG state, both it and the DAG state
	// will be initialized to contain only the genesis block.
	err := dag.initDAGState()
	if err != nil {
		return nil, err
	}

	// Initialize and catch up all of the currently active optional indexes
	// as needed.
	if config.IndexManager != nil {
		err = config.IndexManager.Init(dag, dag.databaseContext)
		if err != nil {
			return nil, err
		}
	}

	genesis, ok := blockNodeStore.LookupNode(params.GenesisHash)

	if !ok {
		genesisBlock := util.NewBlock(dag.Params.GenesisBlock)
		// To prevent the creation of a new err variable unintentionally so the
		// defered function above could read err - declare isOrphan and isDelayed explicitly.
		var isOrphan, isDelayed bool
		isOrphan, isDelayed, err = dag.ProcessBlock(genesisBlock, BFNone)
		if err != nil {
			return nil, err
		}
		if isDelayed {
			return nil, errors.New("genesis block shouldn't be in the future")
		}
		if isOrphan {
			return nil, errors.New("genesis block is unexpectedly orphan")
		}
		genesis, ok = blockNodeStore.LookupNode(params.GenesisHash)
		if !ok {
			return nil, errors.New("genesis is not found in the DAG after it was proccessed")
		}
	}

	// Save a reference to the genesis block.
	dag.genesis = genesis

	// Initialize rule change threshold state caches.
	err = dag.initThresholdCaches()
	if err != nil {
		return nil, err
	}

	selectedTip := dag.selectedTip()
	log.Infof("DAG state (blue score %d, hash %s)",
		selectedTip.BlueScore(), selectedTip.Hash())

	return dag, nil
}

// IsKnownBlock returns whether or not the DAG instance has the block represented
// by the passed hash. This includes checking the various places a block can
// be in, like part of the DAG or the orphan pool.
//
// This function is safe for concurrent access.
func (dag *BlockDAG) IsKnownBlock(hash *daghash.Hash) bool {
	return dag.IsInDAG(hash) || dag.IsKnownOrphan(hash) || dag.delayedBlocks.IsKnownDelayed(hash) || dag.IsKnownInvalid(hash)
}

// AreKnownBlocks returns whether or not the DAG instances has all blocks represented
// by the passed hashes. This includes checking the various places a block can
// be in, like part of the DAG or the orphan pool.
//
// This function is safe for concurrent access.
func (dag *BlockDAG) AreKnownBlocks(hashes []*daghash.Hash) bool {
	for _, hash := range hashes {
		haveBlock := dag.IsKnownBlock(hash)
		if !haveBlock {
			return false
		}
	}

	return true
}

// IsKnownOrphan returns whether the passed hash is currently a known orphan.
// Keep in mind that only a limited number of orphans are held onto for a
// limited amount of time, so this function must not be used as an absolute
// way to test if a block is an orphan block. A full block (as opposed to just
// its hash) must be passed to ProcessBlock for that purpose. However, calling
// ProcessBlock with an orphan that already exists results in an error, so this
// function provides a mechanism for a caller to intelligently detect *recent*
// duplicate orphans and react accordingly.
//
// This function is safe for concurrent access.
func (dag *BlockDAG) IsKnownOrphan(hash *daghash.Hash) bool {
	// Protect concurrent access. Using a read lock only so multiple
	// readers can query without blocking each other.
	dag.orphanLock.RLock()
	defer dag.orphanLock.RUnlock()
	_, exists := dag.orphans[*hash]

	return exists
}

// IsKnownInvalid returns whether the passed hash is known to be an invalid block.
// Note that if the block is not found this method will return false.
//
// This function is safe for concurrent access.
func (dag *BlockDAG) IsKnownInvalid(hash *daghash.Hash) bool {
	node, ok := dag.blockNodeStore.LookupNode(hash)
	if !ok {
		return false
	}
	return dag.blockNodeStore.NodeStatus(node).KnownInvalid()
}

// GetOrphanMissingAncestorHashes returns all of the missing parents in the orphan's sub-DAG
//
// This function is safe for concurrent access.
func (dag *BlockDAG) GetOrphanMissingAncestorHashes(orphanHash *daghash.Hash) []*daghash.Hash {
	// Protect concurrent access. Using a read lock only so multiple
	// readers can query without blocking each other.
	dag.orphanLock.RLock()
	defer dag.orphanLock.RUnlock()

	missingAncestorsHashes := make([]*daghash.Hash, 0)

	visited := make(map[daghash.Hash]bool)
	queue := []*daghash.Hash{orphanHash}
	for len(queue) > 0 {
		var current *daghash.Hash
		current, queue = queue[0], queue[1:]
		if !visited[*current] {
			visited[*current] = true
			orphan, orphanExists := dag.orphans[*current]
			if orphanExists {
				queue = append(queue, orphan.block.MsgBlock().Header.ParentHashes...)
			} else {
				if !dag.IsInDAG(current) && current != orphanHash {
					missingAncestorsHashes = append(missingAncestorsHashes, current)
				}
			}
		}
	}
	return missingAncestorsHashes
}

// removeOrphanBlock removes the passed orphan block from the orphan pool and
// previous orphan index.
func (dag *BlockDAG) removeOrphanBlock(orphan *orphanBlock) {
	// Protect concurrent access.
	dag.orphanLock.Lock()
	defer dag.orphanLock.Unlock()

	// Remove the orphan block from the orphan pool.
	orphanHash := orphan.block.Hash()
	delete(dag.orphans, *orphanHash)

	// Remove the reference from the previous orphan index too.
	for _, parentHash := range orphan.block.MsgBlock().Header.ParentHashes {
		// An indexing for loop is intentionally used over a range here as range
		// does not reevaluate the slice on each iteration nor does it adjust the
		// index for the modified slice.
		orphans := dag.prevOrphans[*parentHash]
		for i := 0; i < len(orphans); i++ {
			hash := orphans[i].block.Hash()
			if hash.IsEqual(orphanHash) {
				orphans = append(orphans[:i], orphans[i+1:]...)
				i--
			}
		}

		// Remove the map entry altogether if there are no longer any orphans
		// which depend on the parent hash.
		if len(orphans) == 0 {
			delete(dag.prevOrphans, *parentHash)
			continue
		}

		dag.prevOrphans[*parentHash] = orphans
	}
}

// addOrphanBlock adds the passed block (which is already determined to be
// an orphan prior calling this function) to the orphan pool. It lazily cleans
// up any expired blocks so a separate cleanup poller doesn't need to be run.
// It also imposes a maximum limit on the number of outstanding orphan
// blocks and will remove the oldest received orphan block if the limit is
// exceeded.
func (dag *BlockDAG) addOrphanBlock(block *util.Block) {
	// Remove expired orphan blocks.
	for _, oBlock := range dag.orphans {
		if mstime.Now().After(oBlock.expiration) {
			dag.removeOrphanBlock(oBlock)
			continue
		}

		// Update the newest orphan block pointer so it can be discarded
		// in case the orphan pool fills up.
		if dag.newestOrphan == nil || oBlock.block.Timestamp().After(dag.newestOrphan.block.Timestamp()) {
			dag.newestOrphan = oBlock
		}
	}

	// Limit orphan blocks to prevent memory exhaustion.
	if len(dag.orphans)+1 > maxOrphanBlocks {
		// If the new orphan is newer than the newest orphan on the orphan
		// pool, don't add it.
		if block.Timestamp().After(dag.newestOrphan.block.Timestamp()) {
			return
		}
		// Remove the newest orphan to make room for the added one.
		dag.removeOrphanBlock(dag.newestOrphan)
		dag.newestOrphan = nil
	}

	// Protect concurrent access. This is intentionally done here instead
	// of near the top since removeOrphanBlock does its own locking and
	// the range iterator is not invalidated by removing map entries.
	dag.orphanLock.Lock()
	defer dag.orphanLock.Unlock()

	// Insert the block into the orphan map with an expiration time
	// 1 hour from now.
	expiration := mstime.Now().Add(time.Hour)
	oBlock := &orphanBlock{
		block:      block,
		expiration: expiration,
	}
	dag.orphans[*block.Hash()] = oBlock

	// Add to parent hash lookup index for faster dependency lookups.
	for _, parentHash := range block.MsgBlock().Header.ParentHashes {
		dag.prevOrphans[*parentHash] = append(dag.prevOrphans[*parentHash], oBlock)
	}
}

// SequenceLock represents the converted relative lock-time in seconds, and
// absolute block-blue-score for a transaction input's relative lock-times.
// According to SequenceLock, after the referenced input has been confirmed
// within a block, a transaction spending that input can be included into a
// block either after 'seconds' (according to past median time), or once the
// 'BlockBlueScore' has been reached.
type SequenceLock struct {
	Milliseconds   int64
	BlockBlueScore int64
}

// CalcSequenceLock computes a relative lock-time SequenceLock for the passed
// transaction using the passed UTXOSet to obtain the past median time
// for blocks in which the referenced inputs of the transactions were included
// within. The generated SequenceLock lock can be used in conjunction with a
// block height, and adjusted median block time to determine if all the inputs
// referenced within a transaction have reached sufficient maturity allowing
// the candidate transaction to be included in a block.
//
// This function is safe for concurrent access.
func (dag *BlockDAG) CalcSequenceLock(tx *util.Tx, utxoSet utxo.UTXOSet, mempool bool) (*SequenceLock, error) {
	dag.dagLock.RLock()
	defer dag.dagLock.RUnlock()

	return dag.calcSequenceLock(dag.selectedTip(), utxoSet, tx, mempool)
}

// CalcSequenceLockNoLock is lock free version of CalcSequenceLockWithLock
// This function is unsafe for concurrent access.
func (dag *BlockDAG) CalcSequenceLockNoLock(tx *util.Tx, utxoSet utxo.UTXOSet, mempool bool) (*SequenceLock, error) {
	return dag.calcSequenceLock(dag.selectedTip(), utxoSet, tx, mempool)
}

// calcSequenceLock computes the relative lock-times for the passed
// transaction. See the exported version, CalcSequenceLock for further details.
//
// This function MUST be called with the DAG state lock held (for writes).
func (dag *BlockDAG) calcSequenceLock(node *blocknode.BlockNode, utxoSet utxo.UTXOSet, tx *util.Tx, mempool bool) (*SequenceLock, error) {
	// A value of -1 for each relative lock type represents a relative time
	// lock value that will allow a transaction to be included in a block
	// at any given height or time.
	sequenceLock := &SequenceLock{Milliseconds: -1, BlockBlueScore: -1}

	// Sequence locks don't apply to coinbase transactions Therefore, we
	// return sequence lock values of -1 indicating that this transaction
	// can be included within a block at any given height or time.
	if tx.IsCoinBase() {
		return sequenceLock, nil
	}

	mTx := tx.MsgTx()
	for txInIndex, txIn := range mTx.TxIn {
		entry, ok := utxoSet.Get(txIn.PreviousOutpoint)
		if !ok {
			str := fmt.Sprintf("output %s referenced from "+
				"transaction %s input %d either does not exist or "+
				"has already been spent", txIn.PreviousOutpoint,
				tx.ID(), txInIndex)
			return sequenceLock, common.NewRuleError(common.ErrMissingTxOut, str)
		}

		// If the input blue score is set to the mempool blue score, then we
		// assume the transaction makes it into the next block when
		// evaluating its sequence blocks.
		inputBlueScore := entry.BlockBlueScore()
		if entry.IsUnaccepted() {
			inputBlueScore = dag.virtual.BlueScore()
		}

		// Given a sequence number, we apply the relative time lock
		// mask in order to obtain the time lock delta required before
		// this input can be spent.
		sequenceNum := txIn.Sequence
		relativeLock := int64(sequenceNum & wire.SequenceLockTimeMask)

		switch {
		// Relative time locks are disabled for this input, so we can
		// skip any further calculation.
		case sequenceNum&wire.SequenceLockTimeDisabled == wire.SequenceLockTimeDisabled:
			continue
		case sequenceNum&wire.SequenceLockTimeIsSeconds == wire.SequenceLockTimeIsSeconds:
			// This input requires a relative time lock expressed
			// in seconds before it can be spent. Therefore, we
			// need to query for the block prior to the one in
			// which this input was accepted within so we can
			// compute the past median time for the block prior to
			// the one which accepted this referenced output.
			blockNode := node
			for blockNode.SelectedParent().BlueScore() > inputBlueScore {
				blockNode = blockNode.SelectedParent()
			}
			medianTime := dag.PastMedianTime(blockNode)

			// Time based relative time-locks have a time granularity of
			// wire.SequenceLockTimeGranularity, so we shift left by this
			// amount to convert to the proper relative time-lock. We also
			// subtract one from the relative lock to maintain the original
			// lockTime semantics.
			timeLockMilliseconds := (relativeLock << wire.SequenceLockTimeGranularity) - 1
			timeLock := medianTime.UnixMilliseconds() + timeLockMilliseconds
			if timeLock > sequenceLock.Milliseconds {
				sequenceLock.Milliseconds = timeLock
			}
		default:
			// The relative lock-time for this input is expressed
			// in blocks so we calculate the relative offset from
			// the input's blue score as its converted absolute
			// lock-time. We subtract one from the relative lock in
			// order to maintain the original lockTime semantics.
			blockBlueScore := int64(inputBlueScore) + relativeLock - 1
			if blockBlueScore > sequenceLock.BlockBlueScore {
				sequenceLock.BlockBlueScore = blockBlueScore
			}
		}
	}

	return sequenceLock, nil
}

// LockTimeToSequence converts the passed relative locktime to a sequence
// number.
func LockTimeToSequence(isMilliseconds bool, locktime uint64) uint64 {
	// If we're expressing the relative lock time in blocks, then the
	// corresponding sequence number is simply the desired input age.
	if !isMilliseconds {
		return locktime
	}

	// Set the 22nd bit which indicates the lock time is in milliseconds, then
	// shift the locktime over by 19 since the time granularity is in
	// 524288-millisecond intervals (2^19). This results in a max lock-time of
	// 34,359,214,080 seconds, or 1.1 years.
	return wire.SequenceLockTimeIsSeconds |
		locktime>>wire.SequenceLockTimeGranularity
}

// addBlock handles adding the passed block to the DAG.
//
// The flags modify the behavior of this function as follows:
//  - BFFastAdd: Avoids several expensive transaction validation operations.
//
// This function MUST be called with the DAG state lock held (for writes).
func (dag *BlockDAG) addBlock(node *blocknode.BlockNode,
	block *util.Block, selectedParentAnticone []*blocknode.BlockNode, flags BehaviorFlags) (*common.ChainUpdates, error) {
	// Skip checks if node has already been fully validated.
	fastAdd := flags&BFFastAdd == BFFastAdd || dag.blockNodeStore.NodeStatus(node).KnownValid()

	// Connect the block to the DAG.
	chainUpdates, err := dag.connectBlock(node, block, selectedParentAnticone, fastAdd)
	if err != nil {
		if errors.As(err, &common.RuleError{}) {
			dag.blockNodeStore.SetStatusFlags(node, blocknode.StatusValidateFailed)

			dbTx, err := dag.databaseContext.NewTx()
			if err != nil {
				return nil, err
			}
			defer dbTx.RollbackUnlessClosed()
			err = dag.blockNodeStore.FlushToDB(dbTx)
			if err != nil {
				return nil, err
			}
			err = dbTx.Commit()
			if err != nil {
				return nil, err
			}
		}
		return nil, err
	}
	dag.blockCount++
	return chainUpdates, nil
}

func calculateAcceptedIDMerkleRoot(multiBlockTxsAcceptanceData common.MultiBlockTxsAcceptanceData) *daghash.Hash {
	var acceptedTxs []*util.Tx
	for _, blockTxsAcceptanceData := range multiBlockTxsAcceptanceData {
		for _, txAcceptance := range blockTxsAcceptanceData.TxAcceptanceData {
			if !txAcceptance.IsAccepted {
				continue
			}
			acceptedTxs = append(acceptedTxs, txAcceptance.Tx)
		}
	}
	sort.Slice(acceptedTxs, func(i, j int) bool {
		return daghash.LessTxID(acceptedTxs[i].ID(), acceptedTxs[j].ID())
	})

	acceptedIDMerkleTree := merkle.BuildIDMerkleTreeStore(acceptedTxs)
	return acceptedIDMerkleTree.Root()
}

func (dag *BlockDAG) validateAcceptedIDMerkleRoot(node *blocknode.BlockNode, txsAcceptanceData common.MultiBlockTxsAcceptanceData) error {
	if node.IsGenesis() {
		return nil
	}

	calculatedAccepetedIDMerkleRoot := calculateAcceptedIDMerkleRoot(txsAcceptanceData)
	header := node.Header()
	if !header.AcceptedIDMerkleRoot.IsEqual(calculatedAccepetedIDMerkleRoot) {
		str := fmt.Sprintf("block accepted ID merkle root is invalid - block "+
			"header indicates %s, but calculated value is %s",
			header.AcceptedIDMerkleRoot, calculatedAccepetedIDMerkleRoot)
		return common.NewRuleError(common.ErrBadMerkleRoot, str)
	}
	return nil
}

// connectBlock handles connecting the passed node/block to the DAG.
//
// This function MUST be called with the DAG state lock held (for writes).
func (dag *BlockDAG) connectBlock(node *blocknode.BlockNode,
	block *util.Block, selectedParentAnticone []*blocknode.BlockNode, fastAdd bool) (*common.ChainUpdates, error) {
	// No warnings about unknown rules or versions until the DAG is
	// synced.
	if dag.isSynced() {
		// Warn if any unknown new rules are either about to activate or
		// have already been activated.
		if err := dag.warnUnknownRuleActivations(node); err != nil {
			return nil, err
		}

		// Warn if a high enough percentage of the last blocks have
		// unexpected versions.
		if err := dag.warnUnknownVersions(node); err != nil {
			return nil, err
		}
	}

	if err := dag.checkFinalityViolation(node); err != nil {
		return nil, err
	}

	if err := dag.validateGasLimit(block); err != nil {
		return nil, err
	}

	newBlockPastUTXO, txsAcceptanceData, newBlockFeeData, newBlockMultiSet, err :=
		dag.verifyAndBuildUTXO(node, block.Transactions(), fastAdd)
	if err != nil {
		return nil, errors.Wrapf(err, "error verifying UTXO for %s", node)
	}

	err = dag.coinbase.ValidateCoinbaseTransaction(node, block, txsAcceptanceData)
	if err != nil {
		return nil, err
	}

	// Apply all changes to the DAG.
	virtualUTXODiff, chainUpdates, err :=
		dag.applyDAGChanges(node, newBlockPastUTXO, newBlockMultiSet, selectedParentAnticone)
	if err != nil {
		// Since all validation logic has already ran, if applyDAGChanges errors out,
		// this means we have a problem in the internal structure of the DAG - a problem which is
		// irrecoverable, and it would be a bad idea to attempt adding any more blocks to the DAG.
		// Therefore - in such cases we panic.
		panic(err)
	}

	err = dag.saveChangesFromBlock(block, virtualUTXODiff, txsAcceptanceData, newBlockFeeData)
	if err != nil {
		return nil, err
	}

	return chainUpdates, nil
}

// calcMultiset returns the multiset of the past UTXO of the given block.
func (dag *BlockDAG) calcMultiset(node *blocknode.BlockNode, acceptanceData common.MultiBlockTxsAcceptanceData,
	selectedParentPastUTXO utxo.UTXOSet) (*secp256k1.MultiSet, error) {

	return dag.pastUTXOMultiSet(node, acceptanceData, selectedParentPastUTXO)
}

func (dag *BlockDAG) pastUTXOMultiSet(node *blocknode.BlockNode, acceptanceData common.MultiBlockTxsAcceptanceData,
	selectedParentPastUTXO utxo.UTXOSet) (*secp256k1.MultiSet, error) {

	ms, err := dag.selectedParentMultiset(node)
	if err != nil {
		return nil, err
	}

	for _, blockAcceptanceData := range acceptanceData {
		for _, txAcceptanceData := range blockAcceptanceData.TxAcceptanceData {
			if !txAcceptanceData.IsAccepted {
				continue
			}

			tx := txAcceptanceData.Tx.MsgTx()

			var err error
			ms, err = addTxToMultiset(ms, tx, selectedParentPastUTXO, node.BlueScore())
			if err != nil {
				return nil, err
			}
		}
	}
	return ms, nil
}

// selectedParentMultiset returns the multiset of the node's selected
// parent. If the node is the genesis BlockNode then it does not have
// a selected parent, in which case return a new, empty multiset.
func (dag *BlockDAG) selectedParentMultiset(node *blocknode.BlockNode) (*secp256k1.MultiSet, error) {
	if node.IsGenesis() {
		return secp256k1.NewMultiset(), nil
	}

	ms, err := dag.multisetStore.MultisetByBlockHash(node.SelectedParent().Hash())
	if err != nil {
		return nil, err
	}

	return ms, nil
}

func addTxToMultiset(ms *secp256k1.MultiSet, tx *wire.MsgTx, pastUTXO utxo.UTXOSet, blockBlueScore uint64) (*secp256k1.MultiSet, error) {
	for _, txIn := range tx.TxIn {
		entry, ok := pastUTXO.Get(txIn.PreviousOutpoint)
		if !ok {
			return nil, errors.Errorf("Couldn't find entry for outpoint %s", txIn.PreviousOutpoint)
		}

		var err error
		ms, err = utxo.RemoveUTXOFromMultiset(ms, entry, &txIn.PreviousOutpoint)
		if err != nil {
			return nil, err
		}
	}

	isCoinbase := tx.IsCoinBase()
	for i, txOut := range tx.TxOut {
		outpoint := *wire.NewOutpoint(tx.TxID(), uint32(i))
		entry := utxo.NewUTXOEntry(txOut, isCoinbase, blockBlueScore)

		var err error
		ms, err = utxo.AddUTXOToMultiset(ms, entry, &outpoint)
		if err != nil {
			return nil, err
		}
	}
	return ms, nil
}

func (dag *BlockDAG) saveChangesFromBlock(block *util.Block, virtualUTXODiff *utxo.UTXODiff,
	txsAcceptanceData common.MultiBlockTxsAcceptanceData, feeData coinbase.CompactFeeData) error {

	dbTx, err := dag.databaseContext.NewTx()
	if err != nil {
		return err
	}
	defer dbTx.RollbackUnlessClosed()

	err = dag.blockNodeStore.FlushToDB(dbTx)
	if err != nil {
		return err
	}

	err = dag.utxoDiffStore.FlushToDB(dbTx)
	if err != nil {
		return err
	}

	err = dag.reachabilityTree.StoreState(dbTx)
	if err != nil {
		return err
	}

	err = dag.multisetStore.FlushToDB(dbTx)
	if err != nil {
		return err
	}

	// Update DAG state.
	state := &dagState{
		TipHashes:         dag.TipHashes(),
		LastFinalityPoint: dag.lastFinalityPoint.Hash(),
		LocalSubnetworkID: dag.subnetworkID,
	}
	err = saveDAGState(dbTx, state)
	if err != nil {
		return err
	}

	// Update the UTXO set using the diffSet that was melded into the
	// full UTXO set.
	err = utxo.UpdateUTXOSet(dbTx, virtualUTXODiff)
	if err != nil {
		return err
	}

	// Scan all accepted transactions and register any subnetwork registry
	// transaction. If any subnetwork registry transaction is not well-formed,
	// fail the entire block.
	err = subnetworks.RegisterSubnetworks(dbTx, block.Transactions())
	if err != nil {
		return err
	}

	// Allow the index manager to call each of the currently active
	// optional indexes with the block being connected so they can
	// update themselves accordingly.
	if dag.indexManager != nil {
		err := dag.indexManager.ConnectBlock(dbTx, block.Hash(), txsAcceptanceData)
		if err != nil {
			return err
		}
	}

	// Apply the fee data into the database
	err = dbaccess.StoreFeeData(dbTx, block.Hash(), feeData)
	if err != nil {
		return err
	}

	err = dbTx.Commit()
	if err != nil {
		return err
	}

	dag.blockNodeStore.ClearDirtyEntries()
	dag.utxoDiffStore.ClearDirtyEntries()
	dag.utxoDiffStore.ClearOldEntries()
	dag.reachabilityTree.ClearDirtyEntries()
	dag.multisetStore.ClearNewEntries()

	return nil
}

func (dag *BlockDAG) validateGasLimit(block *util.Block) error {
	var currentSubnetworkID *subnetworkid.SubnetworkID
	var currentSubnetworkGasLimit uint64
	var currentGasUsage uint64
	var err error

	// We assume here that transactions are ordered by subnetworkID,
	// since it was already validated in checkTransactionSanity
	for _, tx := range block.Transactions() {
		msgTx := tx.MsgTx()

		// In native and Built-In subnetworks all txs must have Gas = 0, and that was already validated in checkTransactionSanity
		// Therefore - no need to check them here.
		if msgTx.SubnetworkID.IsEqual(subnetworkid.SubnetworkIDNative) || msgTx.SubnetworkID.IsBuiltIn() {
			continue
		}

		if !msgTx.SubnetworkID.IsEqual(currentSubnetworkID) {
			currentSubnetworkID = &msgTx.SubnetworkID
			currentGasUsage = 0
			currentSubnetworkGasLimit, err = subnetworks.GasLimit(dag.databaseContext, currentSubnetworkID)
			if err != nil {
				return errors.Errorf("Error getting gas limit for subnetworkID '%s': %s", currentSubnetworkID, err)
			}
		}

		newGasUsage := currentGasUsage + msgTx.Gas
		if newGasUsage < currentGasUsage { // check for overflow
			str := fmt.Sprintf("Block gas usage in subnetwork with ID %s has overflown", currentSubnetworkID)
			return common.NewRuleError(common.ErrInvalidGas, str)
		}
		if newGasUsage > currentSubnetworkGasLimit {
			str := fmt.Sprintf("Block wastes too much gas in subnetwork with ID %s", currentSubnetworkID)
			return common.NewRuleError(common.ErrInvalidGas, str)
		}

		currentGasUsage = newGasUsage
	}

	return nil
}

// LastFinalityPointHash returns the hash of the last finality point
func (dag *BlockDAG) LastFinalityPointHash() *daghash.Hash {
	if dag.lastFinalityPoint == nil {
		return nil
	}
	return dag.lastFinalityPoint.Hash()
}

// isInSelectedParentChainOf returns whether `node` is in the selected parent chain of `other`.
func (dag *BlockDAG) isInSelectedParentChainOf(node *blocknode.BlockNode, other *blocknode.BlockNode) (bool, error) {
	// By definition, a node is not in the selected parent chain of itself.
	if node == other {
		return false, nil
	}

	return dag.reachabilityTree.IsReachabilityTreeAncestorOf(node, other)
}

// FinalityInterval is the interval that determines the finality window of the DAG.
func (dag *BlockDAG) FinalityInterval() uint64 {
	return uint64(dag.Params.FinalityDuration / dag.Params.TargetTimePerBlock)
}

// checkFinalityViolation checks the new block does not violate the finality rules
// specifically - the new block selectedParent chain should contain the old finality point.
func (dag *BlockDAG) checkFinalityViolation(newNode *blocknode.BlockNode) error {
	// the genesis block can not violate finality rules
	if newNode.IsGenesis() {
		return nil
	}

	// Because newNode doesn't have reachability data we
	// need to check if the last finality point is in the
	// selected parent chain of newNode.selectedParent, so
	// we explicitly check if newNode.selectedParent is
	// the finality point.
	if dag.lastFinalityPoint == newNode.SelectedParent() {
		return nil
	}

	isInSelectedChain, err := dag.isInSelectedParentChainOf(dag.lastFinalityPoint, newNode.SelectedParent())
	if err != nil {
		return err
	}

	if !isInSelectedChain {
		return common.NewRuleError(common.ErrFinality, "the last finality point is not in the selected parent chain of this block")
	}
	return nil
}

// updateFinalityPoint updates the dag's last finality point if necessary.
func (dag *BlockDAG) updateFinalityPoint() {
	selectedTip := dag.selectedTip()
	// if the selected tip is the genesis block - it should be the new finality point
	if selectedTip.IsGenesis() {
		dag.lastFinalityPoint = selectedTip
		return
	}
	// We are looking for a new finality point only if the new block's finality score is higher
	// by 2 than the existing finality point's
	if dag.FinalityScore(selectedTip) < dag.FinalityScore(dag.lastFinalityPoint)+2 {
		return
	}

	var currentNode *blocknode.BlockNode
	for currentNode = selectedTip.SelectedParent(); ; currentNode = currentNode.SelectedParent() {
		// We look for the first node in the selected parent chain that has a higher finality score than the last finality point.
		if dag.FinalityScore(currentNode.SelectedParent()) == dag.FinalityScore(dag.lastFinalityPoint) {
			break
		}
	}
	dag.lastFinalityPoint = currentNode
	spawn("dag.finalizeNodesBelowFinalityPoint", func() {
		dag.finalizeNodesBelowFinalityPoint(true)
	})
}

func (dag *BlockDAG) finalizeNodesBelowFinalityPoint(deleteDiffData bool) {
	queue := make([]*blocknode.BlockNode, 0, len(dag.lastFinalityPoint.Parents()))
	for parent := range dag.lastFinalityPoint.Parents() {
		queue = append(queue, parent)
	}
	var nodesToDelete []*blocknode.BlockNode
	if deleteDiffData {
		nodesToDelete = make([]*blocknode.BlockNode, 0, dag.FinalityInterval())
	}
	for len(queue) > 0 {
		var current *blocknode.BlockNode
		current, queue = queue[0], queue[1:]
		if !current.IsFinalized() {
			current.SetFinalized(true)
			if deleteDiffData {
				nodesToDelete = append(nodesToDelete, current)
			}
			for parent := range current.Parents() {
				queue = append(queue, parent)
			}
		}
	}
	if deleteDiffData {
		err := dag.utxoDiffStore.RemoveBlocksDiffData(dag.databaseContext, nodesToDelete)
		if err != nil {
			panic(fmt.Sprintf("Error removing diff data from utxoDiffStore: %s", err))
		}
	}
}

// IsKnownFinalizedBlock returns whether the block is below the finality point.
// IsKnownFinalizedBlock might be false-negative because node finality status is
// updated in a separate goroutine. To get a definite answer if a block
// is finalized or not, use dag.checkFinalityViolation.
func (dag *BlockDAG) IsKnownFinalizedBlock(blockHash *daghash.Hash) bool {
	node, ok := dag.blockNodeStore.LookupNode(blockHash)
	return ok && node.IsFinalized()
}

// NextBlockCoinbaseTransaction prepares the coinbase transaction for the next mined block
//
// This function CAN'T be called with the DAG lock held.
func (dag *BlockDAG) NextBlockCoinbaseTransaction(scriptPubKey []byte, extraData []byte) (*util.Tx, error) {
	dag.dagLock.RLock()
	defer dag.dagLock.RUnlock()

	return dag.NextBlockCoinbaseTransactionNoLock(scriptPubKey, extraData)
}

// NextBlockCoinbaseTransactionNoLock prepares the coinbase transaction for the next mined block
//
// This function MUST be called with the DAG read-lock held
func (dag *BlockDAG) NextBlockCoinbaseTransactionNoLock(scriptPubKey []byte, extraData []byte) (*util.Tx, error) {
	txsAcceptanceData, err := dag.TxsAcceptedByVirtual()
	if err != nil {
		return nil, err
	}
	return dag.coinbase.ExpectedCoinbaseTransaction(&dag.virtual.BlockNode, txsAcceptanceData, scriptPubKey, extraData)
}

// NextAcceptedIDMerkleRootNoLock prepares the acceptedIDMerkleRoot for the next mined block
//
// This function MUST be called with the DAG read-lock held
func (dag *BlockDAG) NextAcceptedIDMerkleRootNoLock() (*daghash.Hash, error) {
	txsAcceptanceData, err := dag.TxsAcceptedByVirtual()
	if err != nil {
		return nil, err
	}

	return calculateAcceptedIDMerkleRoot(txsAcceptanceData), nil
}

// TxsAcceptedByVirtual retrieves transactions accepted by the current virtual block
//
// This function MUST be called with the DAG read-lock held
func (dag *BlockDAG) TxsAcceptedByVirtual() (common.MultiBlockTxsAcceptanceData, error) {
	_, _, txsAcceptanceData, err := dag.pastUTXO(&dag.virtual.BlockNode)
	return txsAcceptanceData, err
}

// TxsAcceptedByBlockHash retrieves transactions accepted by the given block
//
// This function MUST be called with the DAG read-lock held
func (dag *BlockDAG) TxsAcceptedByBlockHash(blockHash *daghash.Hash) (common.MultiBlockTxsAcceptanceData, error) {
	node, ok := dag.blockNodeStore.LookupNode(blockHash)
	if !ok {
		return nil, errors.Errorf("Couldn't find block %s", blockHash)
	}
	_, _, txsAcceptanceData, err := dag.pastUTXO(node)
	return txsAcceptanceData, err
}

// applyDAGChanges does the following:
// 1. Connects each of the new block's parents to the block.
// 2. Adds the new block to the DAG's tips.
// 3. Updates the DAG's full UTXO set.
// 4. Updates each of the tips' utxoDiff.
// 5. Applies the new virtual's blue score to all the unaccepted UTXOs
// 6. Adds the block to the reachability structures
// 7. Adds the multiset of the block to the multiset store.
// 8. Updates the finality point of the DAG (if required).
//
// It returns the diff in the virtual block's UTXO set.
//
// This function MUST be called with the DAG state lock held (for writes).
func (dag *BlockDAG) applyDAGChanges(node *blocknode.BlockNode, newBlockPastUTXO utxo.UTXOSet,
	newBlockMultiset *secp256k1.MultiSet, selectedParentAnticone []*blocknode.BlockNode) (
	virtualUTXODiff *utxo.UTXODiff, chainUpdates *common.ChainUpdates, err error) {

	// Add the block to the reachability tree
	err = dag.reachabilityTree.AddBlock(node, selectedParentAnticone, dag.SelectedTipBlueScore())
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed adding block to the reachability tree")
	}

	dag.multisetStore.SetMultiset(node.Hash(), newBlockMultiset)

	if err = dag.updateParents(node, newBlockPastUTXO); err != nil {
		return nil, nil, errors.Wrapf(err, "failed updating parents of %s", node)
	}

	// Update the virtual block's parents (the DAG tips) to include the new block.
	chainUpdates = dag.virtual.AddTip(node)

	// Build a UTXO set for the new virtual block
	newVirtualUTXO, _, _, err := dag.pastUTXO(&dag.virtual.BlockNode)
	if err != nil {
		return nil, nil, errors.Wrap(err, "could not restore past UTXO for virtual")
	}

	// Apply new utxoDiffs to all the tips
	err = updateTipsUTXO(dag, newVirtualUTXO)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed updating the tips' UTXO")
	}

	// It is now safe to meld the UTXO set to base.
	diffSet := newVirtualUTXO.(*utxo.DiffUTXOSet)
	virtualUTXODiff = diffSet.UTXODiff
	err = dag.meldVirtualUTXO(diffSet)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed melding the virtual UTXO")
	}

	dag.blockNodeStore.SetStatusFlags(node, blocknode.StatusValid)

	// And now we can update the finality point of the DAG (if required)
	dag.updateFinalityPoint()

	return virtualUTXODiff, chainUpdates, nil
}

func (dag *BlockDAG) meldVirtualUTXO(newVirtualUTXODiffSet *utxo.DiffUTXOSet) error {
	dag.utxoLock.Lock()
	defer dag.utxoLock.Unlock()
	return newVirtualUTXODiffSet.MeldToBase()
}

// checkDoubleSpendsWithBlockPast checks that each block transaction
// has a corresponding UTXO in the block pastUTXO.
func checkDoubleSpendsWithBlockPast(pastUTXO utxo.UTXOSet, blockTransactions []*util.Tx) error {
	for _, tx := range blockTransactions {
		if tx.IsCoinBase() {
			continue
		}

		for _, txIn := range tx.MsgTx().TxIn {
			if _, ok := pastUTXO.Get(txIn.PreviousOutpoint); !ok {
				return common.NewRuleError(common.ErrMissingTxOut, fmt.Sprintf("missing transaction "+
					"output %s in the utxo set", txIn.PreviousOutpoint))
			}
		}
	}

	return nil
}

// verifyAndBuildUTXO verifies all transactions in the given block and builds its UTXO
// to save extra traversals it returns the transactions acceptance data, the compactFeeData
// for the new block and its multiset.
func (dag *BlockDAG) verifyAndBuildUTXO(node *blocknode.BlockNode, transactions []*util.Tx, fastAdd bool) (
	newBlockUTXO utxo.UTXOSet, txsAcceptanceData common.MultiBlockTxsAcceptanceData, newBlockFeeData coinbase.CompactFeeData, multiset *secp256k1.MultiSet, err error) {

	pastUTXO, selectedParentPastUTXO, txsAcceptanceData, err := dag.pastUTXO(node)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	err = dag.validateAcceptedIDMerkleRoot(node, txsAcceptanceData)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	feeData, err := dag.checkConnectToPastUTXO(node, pastUTXO, transactions, fastAdd)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	multiset, err = dag.calcMultiset(node, txsAcceptanceData, selectedParentPastUTXO)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	calculatedMultisetHash := daghash.Hash(*multiset.Finalize())
	if !calculatedMultisetHash.IsEqual(node.UTXOCommitment()) {
		str := fmt.Sprintf("block %s UTXO commitment is invalid - block "+
			"header indicates %s, but calculated value is %s", node.Hash(),
			node.UTXOCommitment(), calculatedMultisetHash)
		return nil, nil, nil, nil, common.NewRuleError(common.ErrBadUTXOCommitment, str)
	}

	return pastUTXO, txsAcceptanceData, feeData, multiset, nil
}

func genesisPastUTXO(virtual *virtualblock.VirtualBlock) (utxo.UTXOSet, error) {
	// The genesis has no past UTXO, so we create an empty UTXO
	// set by creating a diff UTXO set with the virtual UTXO
	// set, and adding all of its entries in toRemove
	diff := utxo.NewUTXODiff()
	for outpoint, entry := range virtual.UTXOSet().UTXOCollection {
		err := diff.RemoveEntry(outpoint, entry)
		if err != nil {
			return nil, err
		}
	}
	genesisPastUTXO := utxo.UTXOSet(utxo.NewDiffUTXOSet(virtual.UTXOSet(), diff))
	return genesisPastUTXO, nil
}

func (dag *BlockDAG) fetchBlueBlocks(node *blocknode.BlockNode) ([]*util.Block, error) {
	blueBlocks := make([]*util.Block, len(node.Blues()))
	for i, blueBlockNode := range node.Blues() {
		blueBlock, err := dag.fetchBlockByHash(blueBlockNode.Hash())
		if err != nil {
			return nil, err
		}

		blueBlocks[i] = blueBlock
	}
	return blueBlocks, nil
}

// applyBlueBlocks adds all transactions in the blue blocks to the selectedParent's past UTXO set
// Purposefully ignoring failures - these are just unaccepted transactions
// Writing down which transactions were accepted or not in txsAcceptanceData
func (dag *BlockDAG) applyBlueBlocks(node *blocknode.BlockNode, selectedParentPastUTXO utxo.UTXOSet, blueBlocks []*util.Block) (
	pastUTXO utxo.UTXOSet, multiBlockTxsAcceptanceData common.MultiBlockTxsAcceptanceData, err error) {

	pastUTXO = selectedParentPastUTXO.(*utxo.DiffUTXOSet).CloneWithoutBase()
	multiBlockTxsAcceptanceData = make(common.MultiBlockTxsAcceptanceData, len(blueBlocks))

	// Add blueBlocks to multiBlockTxsAcceptanceData in topological order. This
	// is so that anyone who iterates over it would process blocks (and transactions)
	// in their order of appearance in the DAG.
	for i := 0; i < len(blueBlocks); i++ {
		blueBlock := blueBlocks[i]
		transactions := blueBlock.Transactions()
		blockTxsAcceptanceData := common.BlockTxsAcceptanceData{
			BlockHash:        *blueBlock.Hash(),
			TxAcceptanceData: make([]common.TxAcceptanceData, len(transactions)),
		}
		isSelectedParent := i == 0

		for j, tx := range blueBlock.Transactions() {
			var isAccepted bool

			// Coinbase transaction outputs are added to the UTXO
			// only if they are in the selected parent chain.
			if !isSelectedParent && tx.IsCoinBase() {
				isAccepted = false
			} else {
				isAccepted, err = pastUTXO.AddTx(tx.MsgTx(), node.BlueScore())
				if err != nil {
					return nil, nil, err
				}
			}
			blockTxsAcceptanceData.TxAcceptanceData[j] = common.TxAcceptanceData{Tx: tx, IsAccepted: isAccepted}
		}
		multiBlockTxsAcceptanceData[i] = blockTxsAcceptanceData
	}

	return pastUTXO, multiBlockTxsAcceptanceData, nil
}

// updateParents adds this block to the children sets of its parents
// and updates the diff of any parent whose DiffChild is this block
func (dag *BlockDAG) updateParents(node *blocknode.BlockNode, newBlockUTXO utxo.UTXOSet) error {
	node.UpdateParentsChildren()
	return dag.updateParentsDiffs(node, newBlockUTXO)
}

// updateParentsDiffs updates the diff of any parent whose DiffChild is this block
func (dag *BlockDAG) updateParentsDiffs(node *blocknode.BlockNode, newBlockUTXO utxo.UTXOSet) error {
	virtualDiffFromNewBlock, err := dag.virtual.UTXOSet().DiffFrom(newBlockUTXO)
	if err != nil {
		return err
	}

	err = dag.utxoDiffStore.SetBlockDiff(node, virtualDiffFromNewBlock)
	if err != nil {
		return err
	}

	for parent := range node.Parents() {
		diffChild, err := dag.utxoDiffStore.DiffChildByNode(parent)
		if err != nil {
			return err
		}
		if diffChild == nil {
			parentPastUTXO, err := dag.restorePastUTXO(parent)
			if err != nil {
				return err
			}
			err = dag.utxoDiffStore.SetBlockDiffChild(parent, node)
			if err != nil {
				return err
			}
			diff, err := newBlockUTXO.DiffFrom(parentPastUTXO)
			if err != nil {
				return err
			}
			err = dag.utxoDiffStore.SetBlockDiff(parent, diff)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// pastUTXO returns the UTXO of a given block's past
// To save traversals over the blue blocks, it also returns the transaction acceptance data for
// all blue blocks
func (dag *BlockDAG) pastUTXO(node *blocknode.BlockNode) (
	pastUTXO, selectedParentPastUTXO utxo.UTXOSet, bluesTxsAcceptanceData common.MultiBlockTxsAcceptanceData, err error) {

	if node.IsGenesis() {
		genesisPastUTXO, err := genesisPastUTXO(dag.virtual)
		if err != nil {
			return nil, nil, nil, err
		}
		return genesisPastUTXO, nil, common.MultiBlockTxsAcceptanceData{}, nil
	}

	selectedParentPastUTXO, err = dag.restorePastUTXO(node.SelectedParent())
	if err != nil {
		return nil, nil, nil, err
	}

	blueBlocks, err := dag.fetchBlueBlocks(node)
	if err != nil {
		return nil, nil, nil, err
	}

	pastUTXO, bluesTxsAcceptanceData, err = dag.applyBlueBlocks(node, selectedParentPastUTXO, blueBlocks)
	if err != nil {
		return nil, nil, nil, err
	}

	return pastUTXO, selectedParentPastUTXO, bluesTxsAcceptanceData, nil
}

// restorePastUTXO restores the UTXO of a given block from its diff
func (dag *BlockDAG) restorePastUTXO(node *blocknode.BlockNode) (utxo.UTXOSet, error) {
	stack := []*blocknode.BlockNode{}

	// Iterate over the chain of diff-childs from node till virtual and add them
	// all into a stack
	for current := node; current != nil; {
		stack = append(stack, current)
		var err error
		current, err = dag.utxoDiffStore.DiffChildByNode(current)
		if err != nil {
			return nil, err
		}
	}

	// Start with the top item in the stack, going over it top-to-bottom,
	// applying the UTXO-diff one-by-one.
	topNode, stack := stack[len(stack)-1], stack[:len(stack)-1] // pop the top item in the stack
	topNodeDiff, err := dag.utxoDiffStore.DiffByNode(topNode)
	if err != nil {
		return nil, err
	}
	accumulatedDiff := topNodeDiff.Clone()

	for i := len(stack) - 1; i >= 0; i-- {
		diff, err := dag.utxoDiffStore.DiffByNode(stack[i])
		if err != nil {
			return nil, err
		}
		// Use withDiffInPlace, otherwise copying the diffs again and again create a polynomial overhead
		err = accumulatedDiff.WithDiffInPlace(diff)
		if err != nil {
			return nil, err
		}
	}

	return utxo.NewDiffUTXOSet(dag.virtual.UTXOSet(), accumulatedDiff), nil
}

// updateTipsUTXO builds and applies new diff UTXOs for all the DAG's tips
func updateTipsUTXO(dag *BlockDAG, virtualUTXO utxo.UTXOSet) error {
	for tip := range dag.virtual.Parents() {
		tipPastUTXO, err := dag.restorePastUTXO(tip)
		if err != nil {
			return err
		}
		diff, err := virtualUTXO.DiffFrom(tipPastUTXO)
		if err != nil {
			return err
		}
		err = dag.utxoDiffStore.SetBlockDiff(tip, diff)
		if err != nil {
			return err
		}
	}

	return nil
}

// isSynced returns whether or not the DAG believes it is synced. Several
// factors are used to guess, but the key factors that allow the DAG to
// believe it is synced are:
//  - Latest block has a timestamp newer than 24 hours ago
//
// This function MUST be called with the DAG state lock held (for reads).
func (dag *BlockDAG) isSynced() bool {
	// Not synced if the virtual's selected parent has a timestamp
	// before 24 hours ago. If the DAG is empty, we take the genesis
	// block timestamp.
	//
	// The DAG appears to be syncned if none of the checks reported
	// otherwise.
	var dagTimestamp int64
	selectedTip := dag.selectedTip()
	if selectedTip == nil {
		dagTimestamp = dag.Params.GenesisBlock.Header.Timestamp.UnixMilliseconds()
	} else {
		dagTimestamp = selectedTip.Timestamp()
	}
	dagTime := mstime.UnixMilliseconds(dagTimestamp)
	return dag.Now().Sub(dagTime) <= isDAGCurrentMaxDiff*dag.Params.TargetTimePerBlock
}

// Now returns the adjusted time according to
// dag.timeSource. See TimeSource.Now for
// more details.
func (dag *BlockDAG) Now() mstime.Time {
	return dag.timeSource.Now()
}

// IsSynced returns whether or not the DAG believes it is synced. Several
// factors are used to guess, but the key factors that allow the DAG to
// believe it is synced are:
//  - Latest block has a timestamp newer than 24 hours ago
//
// This function is safe for concurrent access.
func (dag *BlockDAG) IsSynced() bool {
	dag.dagLock.RLock()
	defer dag.dagLock.RUnlock()

	return dag.isSynced()
}

// selectedTip returns the current selected tip for the DAG.
// It will return nil if there is no tip.
func (dag *BlockDAG) selectedTip() *blocknode.BlockNode {
	return dag.virtual.SelectedParent()
}

// SelectedTipHeader returns the header of the current selected tip for the DAG.
// It will return nil if there is no tip.
//
// This function is safe for concurrent access.
func (dag *BlockDAG) SelectedTipHeader() *wire.BlockHeader {
	selectedTip := dag.selectedTip()
	if selectedTip == nil {
		return nil
	}

	return selectedTip.Header()
}

// SelectedTipHash returns the hash of the current selected tip for the DAG.
// It will return nil if there is no tip.
//
// This function is safe for concurrent access.
func (dag *BlockDAG) SelectedTipHash() *daghash.Hash {
	selectedTip := dag.selectedTip()
	if selectedTip == nil {
		return nil
	}

	return selectedTip.Hash()
}

// UTXOSet returns the DAG's UTXO set
func (dag *BlockDAG) UTXOSet() *utxo.FullUTXOSet {
	return dag.virtual.UTXOSet()
}

// CalcPastMedianTime returns the past median time of the DAG.
func (dag *BlockDAG) CalcPastMedianTime() mstime.Time {
	return dag.PastMedianTime(dag.virtual.Tips().Bluest())
}

// GetUTXOEntry returns the requested unspent transaction output. The returned
// instance must be treated as immutable since it is shared by all callers.
//
// This function is safe for concurrent access. However, the returned entry (if
// any) is NOT.
func (dag *BlockDAG) GetUTXOEntry(outpoint wire.Outpoint) (*utxo.UTXOEntry, bool) {
	return dag.virtual.UTXOSet().Get(outpoint)
}

// BlueScoreByBlockHash returns the blue score of a block with the given hash.
func (dag *BlockDAG) BlueScoreByBlockHash(hash *daghash.Hash) (uint64, error) {
	node, ok := dag.blockNodeStore.LookupNode(hash)
	if !ok {
		return 0, errors.Errorf("block %s is unknown", hash)
	}

	return node.BlueScore(), nil
}

// BluesByBlockHash returns the blues of the block for the given hash.
func (dag *BlockDAG) BluesByBlockHash(hash *daghash.Hash) ([]*daghash.Hash, error) {
	node, ok := dag.blockNodeStore.LookupNode(hash)
	if !ok {
		return nil, errors.Errorf("block %s is unknown", hash)
	}

	hashes := make([]*daghash.Hash, len(node.Blues()))
	for i, blue := range node.Blues() {
		hashes[i] = blue.Hash()
	}

	return hashes, nil
}

// BlockConfirmationsByHash returns the confirmations number for a block with the
// given hash. See blockConfirmations for further details.
//
// This function is safe for concurrent access
func (dag *BlockDAG) BlockConfirmationsByHash(hash *daghash.Hash) (uint64, error) {
	dag.dagLock.RLock()
	defer dag.dagLock.RUnlock()

	return dag.BlockConfirmationsByHashNoLock(hash)
}

// BlockConfirmationsByHashNoLock is lock free version of BlockConfirmationsByHash
//
// This function is unsafe for concurrent access.
func (dag *BlockDAG) BlockConfirmationsByHashNoLock(hash *daghash.Hash) (uint64, error) {
	if hash.IsEqual(&daghash.ZeroHash) {
		return 0, nil
	}

	node, ok := dag.blockNodeStore.LookupNode(hash)
	if !ok {
		return 0, errors.Errorf("block %s is unknown", hash)
	}

	return dag.blockConfirmations(node)
}

// UTXOConfirmations returns the confirmations for the given outpoint, if it exists
// in the DAG's UTXO set.
//
// This function is safe for concurrent access.
func (dag *BlockDAG) UTXOConfirmations(outpoint *wire.Outpoint) (uint64, bool) {
	dag.dagLock.RLock()
	defer dag.dagLock.RUnlock()

	utxoEntry, ok := dag.GetUTXOEntry(*outpoint)
	if !ok {
		return 0, false
	}
	confirmations := dag.SelectedTipBlueScore() - utxoEntry.BlockBlueScore() + 1

	return confirmations, true
}

// blockConfirmations returns the current confirmations number of the given node
// The confirmations number is defined as follows:
// * If the node is in the selected tip red set	-> 0
// * If the node is the selected tip			-> 1
// * Otherwise									-> selectedTip.blueScore - acceptingBlock.blueScore + 2
func (dag *BlockDAG) blockConfirmations(node *blocknode.BlockNode) (uint64, error) {
	acceptingBlock, err := dag.acceptingBlock(node)
	if err != nil {
		return 0, err
	}

	// if acceptingBlock is nil, the node is red
	if acceptingBlock == nil {
		return 0, nil
	}

	return dag.selectedTip().BlueScore() - acceptingBlock.BlueScore() + 1, nil
}

// acceptingBlock finds the node in the selected-parent chain that had accepted
// the given node
func (dag *BlockDAG) acceptingBlock(node *blocknode.BlockNode) (*blocknode.BlockNode, error) {
	// Return an error if the node is the virtual block
	if node == &dag.virtual.BlockNode {
		return nil, errors.New("cannot get acceptingBlock for virtual")
	}

	// If the node is a chain-block itself, the accepting block is its chain-child
	isNodeInSelectedParentChain, err := dag.virtual.IsInSelectedParentChain(node.Hash())
	if err != nil {
		return nil, err
	}
	if isNodeInSelectedParentChain {
		if len(node.Children()) == 0 {
			// If the node is the selected tip, it doesn't have an accepting block
			return nil, nil
		}
		for child := range node.Children() {
			isChildInSelectedParentChain, err := dag.virtual.IsInSelectedParentChain(child.Hash())
			if err != nil {
				return nil, err
			}
			if isChildInSelectedParentChain {
				return child, nil
			}
		}
		return nil, errors.Errorf("chain block %s does not have a chain child", node.Hash())
	}

	// Find the only chain block that may contain the node in its blues
	candidateAcceptingBlock := dag.virtual.OldestChainBlockWithBlueScoreGreaterThan(node.BlueScore())

	// if no candidate is found, it means that the node has same or more
	// blue score than the selected tip and is found in its anticone, so
	// it doesn't have an accepting block
	if candidateAcceptingBlock == nil {
		return nil, nil
	}

	// candidateAcceptingBlock is the accepting block only if it actually contains
	// the node in its blues
	for _, blue := range candidateAcceptingBlock.Blues() {
		if blue == node {
			return candidateAcceptingBlock, nil
		}
	}

	// Otherwise, the node is red or in the selected tip anticone, and
	// doesn't have an accepting block
	return nil, nil
}

// SelectedTipBlueScore returns the blue score of the selected tip. Returns zero
// if we hadn't accepted the genesis block yet.
func (dag *BlockDAG) SelectedTipBlueScore() uint64 {
	selectedTip := dag.selectedTip()
	if selectedTip == nil {
		return 0
	}
	return selectedTip.BlueScore()
}

// VirtualBlueScore returns the blue score of the current virtual block
func (dag *BlockDAG) VirtualBlueScore() uint64 {
	return dag.virtual.BlueScore()
}

// BlockCount returns the number of blocks in the DAG
func (dag *BlockDAG) BlockCount() uint64 {
	return dag.blockCount
}

// TipHashes returns the hashes of the DAG's tips
func (dag *BlockDAG) TipHashes() []*daghash.Hash {
	return dag.virtual.Tips().Hashes()
}

// CurrentBits returns the bits of the tip with the lowest bits, which also means it has highest difficulty.
func (dag *BlockDAG) CurrentBits() uint32 {
	tips := dag.virtual.Tips()
	minBits := uint32(math.MaxUint32)
	for tip := range tips {
		if minBits > tip.Header().Bits {
			minBits = tip.Header().Bits
		}
	}
	return minBits
}

// HeaderByHash returns the block header identified by the given hash or an
// error if it doesn't exist.
func (dag *BlockDAG) HeaderByHash(hash *daghash.Hash) (*wire.BlockHeader, error) {
	node, ok := dag.blockNodeStore.LookupNode(hash)
	if !ok {
		err := errors.Errorf("block %s is not known", hash)
		return &wire.BlockHeader{}, err
	}

	return node.Header(), nil
}

// ChildHashesByHash returns the child hashes of the block with the given hash in the
// DAG.
//
// This function is safe for concurrent access.
func (dag *BlockDAG) ChildHashesByHash(hash *daghash.Hash) ([]*daghash.Hash, error) {
	node, ok := dag.blockNodeStore.LookupNode(hash)
	if !ok {
		str := fmt.Sprintf("block %s is not in the DAG", hash)
		return nil, common.ErrNotInDAG(str)

	}

	return node.Children().Hashes(), nil
}

// SelectedParentHash returns the selected parent hash of the block with the given hash in the
// DAG.
//
// This function is safe for concurrent access.
func (dag *BlockDAG) SelectedParentHash(blockHash *daghash.Hash) (*daghash.Hash, error) {
	node, ok := dag.blockNodeStore.LookupNode(blockHash)
	if !ok {
		str := fmt.Sprintf("block %s is not in the DAG", blockHash)
		return nil, common.ErrNotInDAG(str)

	}

	if node.SelectedParent() == nil {
		return nil, nil
	}
	return node.SelectedParent().Hash(), nil
}

// antiPastHashesBetween returns the hashes of the blocks between the
// lowHash's antiPast and highHash's antiPast, or up to the provided
// max number of block hashes.
//
// This function MUST be called with the DAG state lock held (for reads).
func (dag *BlockDAG) antiPastHashesBetween(lowHash, highHash *daghash.Hash, maxHashes uint64) ([]*daghash.Hash, error) {
	nodes, err := dag.antiPastBetween(lowHash, highHash, maxHashes)
	if err != nil {
		return nil, err
	}
	hashes := make([]*daghash.Hash, len(nodes))
	for i, node := range nodes {
		hashes[i] = node.Hash()
	}
	return hashes, nil
}

// antiPastBetween returns the blockNodes between the lowHash's antiPast
// and highHash's antiPast, or up to the provided max number of blocks.
//
// This function MUST be called with the DAG state lock held (for reads).
func (dag *BlockDAG) antiPastBetween(lowHash, highHash *daghash.Hash, maxEntries uint64) ([]*blocknode.BlockNode, error) {
	lowNode, ok := dag.blockNodeStore.LookupNode(lowHash)
	if !ok {
		return nil, errors.Errorf("Couldn't find low hash %s", lowHash)
	}
	highNode, ok := dag.blockNodeStore.LookupNode(highHash)
	if !ok {
		return nil, errors.Errorf("Couldn't find high hash %s", highHash)
	}
	if lowNode.BlueScore() >= highNode.BlueScore() {
		return nil, errors.Errorf("Low hash blueScore >= high hash blueScore (%d >= %d)",
			lowNode.BlueScore(), highNode.BlueScore())
	}

	// In order to get no more then maxEntries blocks from the
	// future of the lowNode (including itself), we iterate the
	// selected parent chain of the highNode and stop once we reach
	// highNode.blueScore-lowNode.blueScore+1 <= maxEntries. That
	// stop point becomes the new highNode.
	// Using blueScore as an approximation is considered to be
	// fairly accurate because we presume that most DAG blocks are
	// blue.
	for highNode.BlueScore()-lowNode.BlueScore()+1 > maxEntries {
		highNode = highNode.SelectedParent()
	}

	// Collect every node in highNode's past (including itself) but
	// NOT in the lowNode's past (excluding itself) into an up-heap
	// (a heap sorted by blueScore from lowest to greatest).
	visited := blocknode.NewBlockNodeSet()
	candidateNodes := blocknode.NewUpHeap()
	queue := blocknode.NewDownHeap()
	queue.Push(highNode)
	for queue.Len() > 0 {
		current := queue.Pop()
		if visited.Contains(current) {
			continue
		}
		visited.Add(current)
		isCurrentAncestorOfLowNode, err := dag.isInPast(current, lowNode)
		if err != nil {
			return nil, err
		}
		if isCurrentAncestorOfLowNode {
			continue
		}
		candidateNodes.Push(current)
		for parent := range current.Parents() {
			queue.Push(parent)
		}
	}

	// Pop candidateNodes into a slice. Since candidateNodes is
	// an up-heap, it's guaranteed to be ordered from low to high
	nodesLen := int(maxEntries)
	if candidateNodes.Len() < nodesLen {
		nodesLen = candidateNodes.Len()
	}
	nodes := make([]*blocknode.BlockNode, nodesLen)
	for i := 0; i < nodesLen; i++ {
		nodes[i] = candidateNodes.Pop()
	}
	return nodes, nil
}

func (dag *BlockDAG) isInPast(this *blocknode.BlockNode, other *blocknode.BlockNode) (bool, error) {
	return dag.reachabilityTree.IsInPast(this, other)
}

// AntiPastHashesBetween returns the hashes of the blocks between the
// lowHash's antiPast and highHash's antiPast, or up to the provided
// max number of block hashes.
//
// This function is safe for concurrent access.
func (dag *BlockDAG) AntiPastHashesBetween(lowHash, highHash *daghash.Hash, maxHashes uint64) ([]*daghash.Hash, error) {
	dag.dagLock.RLock()
	defer dag.dagLock.RUnlock()
	hashes, err := dag.antiPastHashesBetween(lowHash, highHash, maxHashes)
	if err != nil {
		return nil, err
	}
	return hashes, nil
}

// antiPastHeadersBetween returns the headers of the blocks between the
// lowHash's antiPast and highHash's antiPast, or up to the provided
// max number of block headers.
//
// This function MUST be called with the DAG state lock held (for reads).
func (dag *BlockDAG) antiPastHeadersBetween(lowHash, highHash *daghash.Hash, maxHeaders uint64) ([]*wire.BlockHeader, error) {
	nodes, err := dag.antiPastBetween(lowHash, highHash, maxHeaders)
	if err != nil {
		return nil, err
	}
	headers := make([]*wire.BlockHeader, len(nodes))
	for i, node := range nodes {
		headers[i] = node.Header()
	}
	return headers, nil
}

// GetTopHeaders returns the top wire.MaxBlockHeadersPerMsg block headers ordered by blue score.
func (dag *BlockDAG) GetTopHeaders(highHash *daghash.Hash, maxHeaders uint64) ([]*wire.BlockHeader, error) {
	highNode := &dag.virtual.BlockNode
	if highHash != nil {
		var ok bool
		highNode, ok = dag.blockNodeStore.LookupNode(highHash)
		if !ok {
			return nil, errors.Errorf("Couldn't find the high hash %s in the dag", highHash)
		}
	}
	headers := make([]*wire.BlockHeader, 0, highNode.BlueScore())
	queue := blocknode.NewDownHeap()
	queue.PushSet(highNode.Parents())

	visited := blocknode.NewBlockNodeSet()
	for i := uint32(0); queue.Len() > 0 && uint64(len(headers)) < maxHeaders; i++ {
		current := queue.Pop()
		if !visited.Contains(current) {
			visited.Add(current)
			headers = append(headers, current.Header())
			queue.PushSet(current.Parents())
		}
	}
	return headers, nil
}

// Lock locks the DAG's UTXO set for writing.
func (dag *BlockDAG) Lock() {
	dag.dagLock.Lock()
}

// Unlock unlocks the DAG's UTXO set for writing.
func (dag *BlockDAG) Unlock() {
	dag.dagLock.Unlock()
}

// RLock locks the DAG's UTXO set for reading.
func (dag *BlockDAG) RLock() {
	dag.dagLock.RLock()
}

// RUnlock unlocks the DAG's UTXO set for reading.
func (dag *BlockDAG) RUnlock() {
	dag.dagLock.RUnlock()
}

// AntiPastHeadersBetween returns the headers of the blocks between the
// lowHash's antiPast and highHash's antiPast, or up to
// wire.MaxBlockHeadersPerMsg block headers.
//
// This function is safe for concurrent access.
func (dag *BlockDAG) AntiPastHeadersBetween(lowHash, highHash *daghash.Hash, maxHeaders uint64) ([]*wire.BlockHeader, error) {
	dag.dagLock.RLock()
	defer dag.dagLock.RUnlock()
	headers, err := dag.antiPastHeadersBetween(lowHash, highHash, maxHeaders)
	if err != nil {
		return nil, err
	}
	return headers, nil
}

// SubnetworkID returns the node's subnetwork ID
func (dag *BlockDAG) SubnetworkID() *subnetworkid.SubnetworkID {
	return dag.subnetworkID
}

// ForEachHash runs the given fn on every hash that's currently known to
// the DAG.
//
// This function is NOT safe for concurrent access. It is meant to be
// used either on initialization or when the dag lock is held for reads.
func (dag *BlockDAG) ForEachHash(fn func(hash daghash.Hash) error) error {
	return dag.blockNodeStore.ForEachHash(fn)
}

func (dag *BlockDAG) addDelayedBlock(block *util.Block, delay time.Duration) error {
	processTime := dag.Now().Add(delay)
	log.Debugf("Adding block to delayed blocks queue (block hash: %s, process time: %s)", block.Hash().String(), processTime)

	dag.delayedBlocks.Add(block, processTime)

	return dag.processDelayedBlocks()
}

// processDelayedBlocks loops over all delayed blocks and processes blocks which are due.
// This method is invoked after processing a block (ProcessBlock method).
func (dag *BlockDAG) processDelayedBlocks() error {
	// Check if the delayed block with the earliest process time should be processed
	for dag.delayedBlocks.Len() > 0 {
		earliestDelayedBlockProcessTime := dag.delayedBlocks.Peek().ProcessTime()
		if earliestDelayedBlockProcessTime.After(dag.Now()) {
			break
		}
		delayedBlock := dag.delayedBlocks.Pop()
		_, _, err := dag.processBlockNoLock(delayedBlock.Block(), BFAfterDelay)
		if err != nil {
			log.Errorf("Error while processing delayed block (block %s)", delayedBlock.Block().Hash().String())
			// Rule errors should not be propagated as they refer only to the delayed block,
			// while this function runs in the context of another block
			if !errors.As(err, &common.RuleError{}) {
				return err
			}
		}
		log.Debugf("Processed delayed block (block %s)", delayedBlock.Block().Hash().String())
	}

	return nil
}

// IndexManager provides a generic interface that is called when blocks are
// connected to the DAG for the purpose of supporting optional indexes.
type IndexManager interface {
	// Init is invoked during DAG initialize in order to allow the index
	// manager to initialize itself and any indexes it is managing.
	Init(*BlockDAG, *dbaccess.DatabaseContext) error

	// ConnectBlock is invoked when a new block has been connected to the
	// DAG.
	ConnectBlock(dbContext *dbaccess.TxContext, blockHash *daghash.Hash, acceptedTxsData common.MultiBlockTxsAcceptanceData) error
}

// Config is a descriptor which specifies the blockDAG instance configuration.
type Config struct {
	// Interrupt specifies a channel the caller can close to signal that
	// long running operations, such as catching up indexes or performing
	// database migrations, should be interrupted.
	//
	// This field can be nil if the caller does not desire the behavior.
	Interrupt <-chan struct{}

	// DAGParams identifies which DAG parameters the DAG is associated
	// with.
	//
	// This field is required.
	DAGParams *dagconfig.Params

	// TimeSource defines the time source to use for things such as
	// block processing and determining whether or not the DAG is current.
	TimeSource timesource.TimeSource

	// SigCache defines a signature cache to use when when validating
	// signatures. This is typically most useful when individual
	// transactions are already being validated prior to their inclusion in
	// a block such as what is usually done via a transaction memory pool.
	//
	// This field can be nil if the caller is not interested in using a
	// signature cache.
	SigCache *txscript.SigCache

	// IndexManager defines an index manager to use when initializing the
	// DAG and connecting blocks.
	//
	// This field can be nil if the caller does not wish to make use of an
	// index manager.
	IndexManager IndexManager

	// SubnetworkID identifies which subnetwork the DAG is associated
	// with.
	//
	// This field is required.
	SubnetworkID *subnetworkid.SubnetworkID

	// DatabaseContext is the context in which all database queries related to
	// this DAG are going to run.
	DatabaseContext *dbaccess.DatabaseContext
}

// initBlockNode returns a new block node for the given block header and parents, and the
// anticone of its selected parent (parent with highest blue score).
// selectedParentAnticone is used to update reachability data we store for future reachability queries.
// This function is NOT safe for concurrent access.
func (dag *BlockDAG) initBlockNode(blockHeader *wire.BlockHeader, parents blocknode.BlockNodeSet) (node *blocknode.BlockNode, selectedParentAnticone []*blocknode.BlockNode) {
	return dag.ghostdag.InitBlockNode(blockHeader, parents)
}

func (dag *BlockDAG) Notifier() *notifications.ConsensusNotifier {
	return dag.notifier
}

func (dag *BlockDAG) FinalityScore(node *blocknode.BlockNode) uint64 {
	return node.BlueScore() / dag.FinalityInterval()
}

// CalcPastMedianTime returns the median time of the previous few blocks
// prior to, and including, the block node.
//
// This function is safe for concurrent access.
func (dag *BlockDAG) PastMedianTime(node *blocknode.BlockNode) mstime.Time {
	window := blueBlockWindow(node, 2*dag.Params.TimestampDeviationTolerance-1)
	medianTimestamp, err := window.medianTimestamp()
	if err != nil {
		panic(fmt.Sprintf("blueBlockWindow: %s", err))
	}
	return mstime.UnixMilliseconds(medianTimestamp)
}

// GasLimit returns the gas limit of a registered subnetwork. If the subnetwork does not
// exist this method returns an error.
func (dag *BlockDAG) GasLimit(subnetworkID *subnetworkid.SubnetworkID) (uint64, error) {
	return subnetworks.GasLimit(dag.databaseContext, subnetworkID)
}

// IsInSelectedParentChain returns whether or not a block hash is found in the selected
// parent chain. Note that this method returns an error if the given blockHash does not
// exist within the block node store.
func (dag *BlockDAG) IsInSelectedParentChain(blockHash *daghash.Hash) (bool, error) {
	return dag.virtual.IsInSelectedParentChain(blockHash)
}

// SelectedParentChain returns the selected parent chain starting from blockHash (exclusive)
// up to the virtual (exclusive). If blockHash is nil then the genesis block is used. If
// blockHash is not within the select parent chain, go down its own selected parent chain,
// while collecting each block hash in removedChainHashes, until reaching a block within
// the main selected parent chain.
func (dag *BlockDAG) SelectedParentChain(blockHash *daghash.Hash) ([]*daghash.Hash, []*daghash.Hash, error) {
	return dag.virtual.SelectedParentChain(blockHash)
}

// BlockLocatorFromHashes returns a block locator from high and low hash.
// See BlockLocator for details on the algorithm used to create a block locator.
func (dag *BlockDAG) BlockLocatorFromHashes(highHash, lowHash *daghash.Hash) (blocklocator.BlockLocator, error) {
	return dag.blockLocatorFactory.BlockLocatorFromHashes(highHash, lowHash)
}

// FindNextLocatorBoundaries returns the lowest unknown block locator, hash
// and the highest known block locator hash. This is used to create the
// next block locator to find the highest shared known chain block with the
// sync peer.
func (dag *BlockDAG) FindNextLocatorBoundaries(locator blocklocator.BlockLocator) (highHash, lowHash *daghash.Hash) {
	return dag.blockLocatorFactory.FindNextLocatorBoundaries(locator)
}
