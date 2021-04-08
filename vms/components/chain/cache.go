// (c) 2019-2020, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package chain

import (
	"errors"
	"fmt"

	"github.com/ava-labs/avalanchego/cache"
	"github.com/ava-labs/avalanchego/cache/metercacher"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/choices"
	"github.com/ava-labs/avalanchego/snow/consensus/snowman"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	// ErrBlockNotFound indicates that the VM was not able to retrieve a block. If this error is returned
	// from getBlock then the miss will be considered cacheable. Any other error will not be considered a
	// cacheable miss.
	ErrBlockNotFound = errors.New("block not found")
)

// State implements an efficient caching layer used to wrap a VM
// implementation.
type Cache struct {
	// getBlock retrieves a block from the VM's storage. If getBlock returns
	// a nil error, then the returned block must not have the status Unknown
	getBlock func(ids.ID) (snowman.Block, error)
	// unmarshals [b] into a block
	unmarshalBlock func([]byte) (snowman.Block, error)
	// buildBlock attempts to build a block on top of the currently preferred block
	// buildBlock should always return a block with status Processing since it should never
	// create an unknown block, and building on top of the preferred block should never yield
	// a block that has already been decided.
	buildBlock func() (snowman.Block, error)

	// getStatus returns the status of the block
	getStatus func(snowman.Block) (choices.Status, error)

	// verifiedBlocks is a map of blocks that have been verified and are
	// therefore currently in consensus.
	verifiedBlocks map[ids.ID]*BlockWrapper
	// decidedBlocks is an LRU cache of decided blocks.
	// Every value in [decidedBlocks] is a (*BlockWrapper)
	decidedBlocks cache.Cacher
	// unverifiedBlocks is an LRU cache of blocks with status processing
	// that have not yet been verified.
	// Every value in [unverifiedBlocks] is a (*BlockWrapper)
	unverifiedBlocks cache.Cacher
	// missingBlocks is an LRU cache of missing blocks
	// Every value in [missingBlocks] is an empty struct.
	missingBlocks     cache.Cacher
	lastAcceptedBlock *BlockWrapper
}

// Config defines all of the parameters necessary to initialize State
type Config struct {
	// Cache configuration:
	DecidedCacheSize, MissingCacheSize, UnverifiedCacheSize int

	LastAcceptedBlock  snowman.Block
	GetBlock           func(ids.ID) (snowman.Block, error)
	UnmarshalBlock     func([]byte) (snowman.Block, error)
	BuildBlock         func() (snowman.Block, error)
	GetBlockIDAtHeight func(uint64) (ids.ID, error)
}

// Block is an interface wrapping the normal snowman.Block interface to be used in
// association with passing in a non-nil function to GetBlockIDAtHeight
type Block interface {
	snowman.Block

	SetStatus(choices.Status)
}

// produceGetStatus creates a getStatus function that infers the status of a block by using a function
// passed in from the VM that gets the block ID at a specific height. It is assumed that for any height
// less than or equal to the last accepted block, getBlockIDAtHeight returns the accepted blockID at
// the requested height.
func produceGetStatus(c *Cache, getBlockIDAtHeight func(uint64) (ids.ID, error)) func(snowman.Block) (choices.Status, error) {
	return func(blk snowman.Block) (choices.Status, error) {
		internalBlk, ok := blk.(Block)
		if !ok {
			return choices.Unknown, fmt.Errorf("expected block to match chain Block interface but found block of type %T", blk)
		}
		lastAcceptedHeight := c.lastAcceptedBlock.Height()
		blkHeight := internalBlk.Height()
		if blkHeight > lastAcceptedHeight {
			internalBlk.SetStatus(choices.Processing)
			return choices.Processing, nil
		}

		acceptedID, err := getBlockIDAtHeight(blkHeight)
		if err != nil {
			return choices.Unknown, fmt.Errorf("failed to get accepted blkID at height %d", blkHeight)
		}
		if acceptedID == blk.ID() {
			internalBlk.SetStatus(choices.Accepted)
			return choices.Accepted, nil
		}

		internalBlk.SetStatus(choices.Rejected)
		return choices.Rejected, nil
	}
}

func NewCache(config *Config) *Cache {
	c := &Cache{
		verifiedBlocks:   make(map[ids.ID]*BlockWrapper),
		decidedBlocks:    &cache.LRU{Size: config.DecidedCacheSize},
		missingBlocks:    &cache.LRU{Size: config.MissingCacheSize},
		unverifiedBlocks: &cache.LRU{Size: config.UnverifiedCacheSize},
		getBlock:         config.GetBlock,
		unmarshalBlock:   config.UnmarshalBlock,
		buildBlock:       config.BuildBlock,
		getStatus:        func(blk snowman.Block) (choices.Status, error) { return blk.Status(), nil },
	}
	if config.GetBlockIDAtHeight != nil {
		c.getStatus = produceGetStatus(c, config.GetBlockIDAtHeight)
	}
	c.lastAcceptedBlock = &BlockWrapper{
		Block: config.LastAcceptedBlock,
		cache: c,
	}
	c.decidedBlocks.Put(config.LastAcceptedBlock.ID(), c.lastAcceptedBlock)
	return c
}

func NewMeteredCache(
	registerer prometheus.Registerer,
	namespace string,
	config *Config,
) (*Cache, error) {
	decidedCache, err := metercacher.New(
		fmt.Sprintf("%s_decided_cache", namespace),
		registerer,
		&cache.LRU{Size: config.DecidedCacheSize},
	)
	if err != nil {
		return nil, err
	}
	missingCache, err := metercacher.New(
		fmt.Sprintf("%s_missing_cache", namespace),
		registerer,
		&cache.LRU{Size: config.MissingCacheSize},
	)
	if err != nil {
		return nil, err
	}
	unverifiedCache, err := metercacher.New(
		fmt.Sprintf("%s_unverified_cache", namespace),
		registerer,
		&cache.LRU{Size: config.UnverifiedCacheSize},
	)
	if err != nil {
		return nil, err
	}
	c := &Cache{
		verifiedBlocks:   make(map[ids.ID]*BlockWrapper),
		decidedBlocks:    decidedCache,
		missingBlocks:    missingCache,
		unverifiedBlocks: unverifiedCache,
		getBlock:         config.GetBlock,
		unmarshalBlock:   config.UnmarshalBlock,
		buildBlock:       config.BuildBlock,
	}
	if config.GetBlockIDAtHeight != nil {
		c.getStatus = produceGetStatus(c, config.GetBlockIDAtHeight)
	}
	c.lastAcceptedBlock = &BlockWrapper{
		Block: config.LastAcceptedBlock,
		cache: c,
	}
	c.decidedBlocks.Put(config.LastAcceptedBlock.ID(), c.lastAcceptedBlock)
	return c, nil
}

// FlushCaches flushes each block cache completely.
func (c *Cache) FlushCaches() {
	c.decidedBlocks.Flush()
	c.missingBlocks.Flush()
	c.unverifiedBlocks.Flush()
}

// GetBlock returns the BlockWrapper as snowman.Block corresponding to [blkID]
func (c *Cache) GetBlock(blkID ids.ID) (snowman.Block, error) {
	if blk, ok := c.getCachedBlock(blkID); ok {
		return blk, nil
	}

	if _, ok := c.missingBlocks.Get(blkID); ok {
		return nil, ErrBlockNotFound
	}

	blk, err := c.getBlock(blkID)
	if err == ErrBlockNotFound {
		c.missingBlocks.Put(blkID, struct{}{})
		return nil, err
	} else if err != nil {
		return nil, err
	}

	// Since this block is not in consensus, addBlockOutsideConsensus
	// is called to add [blk] to the correct cache.
	return c.addBlockOutsideConsensus(blk)
}

// getCachedBlock checks the caches for [blkID] by priority. Returning
// true if [blkID] is found in one of the caches.
func (c *Cache) getCachedBlock(blkID ids.ID) (snowman.Block, bool) {
	if blk, ok := c.verifiedBlocks[blkID]; ok {
		return blk, true
	}

	if blk, ok := c.decidedBlocks.Get(blkID); ok {
		return blk.(snowman.Block), true
	}

	if blk, ok := c.unverifiedBlocks.Get(blkID); ok {
		return blk.(snowman.Block), true
	}

	return nil, false
}

// GetBlockInternal returns the internal representation of [blkID]
func (c *Cache) GetBlockInternal(blkID ids.ID) (snowman.Block, error) {
	wrappedBlk, err := c.GetBlock(blkID)
	if err != nil {
		return nil, err
	}

	return wrappedBlk.(*BlockWrapper).Block, nil
}

// ParseBlock attempts to parse [b] into an internal Block and adds it to the appropriate
// caching layer if successful.
func (c *Cache) ParseBlock(b []byte) (snowman.Block, error) {
	blk, err := c.unmarshalBlock(b)
	if err != nil {
		return nil, err
	}

	blkID := blk.ID()
	// Check for an existing block, so we can return a unique block
	// if processing or simply allow this block to be immediately
	// garbage collected if it is already cached.
	if cachedBlk, ok := c.getCachedBlock(blkID); ok {
		return cachedBlk, nil
	}

	c.missingBlocks.Evict(blkID)

	// Since this block is not in consensus, addBlockOutsideConsensus
	// is called to add [blk] to the correct cache.
	return c.addBlockOutsideConsensus(blk)
}

// BuildBlock attempts to build a new internal Block, wraps it, and adds it
// to the appropriate caching layer if successful.
func (c *Cache) BuildBlock() (snowman.Block, error) {
	blk, err := c.buildBlock()
	if err != nil {
		return nil, err
	}

	blkID := blk.ID()
	// Defensive: buildBlock should not return a block that has already been verified.
	// If it does, make sure to return the existing reference to the block.
	if existingBlk, ok := c.getCachedBlock(blkID); ok {
		return existingBlk, nil
	}
	// Evict the produced block from missing blocks in case it was previously
	// marked as missing.
	c.missingBlocks.Evict(blkID)

	// wrap the returned block and add it to the correct cache
	return c.addBlockOutsideConsensus(blk)
}

// addBlockOutsideConsensus adds [blk] to the correct cache and returns
// a wrapped version of [blk]
// assumes [blk] is a known, non-wrapped block that is not currently
// in consensus. [blk] could be either decided or a block that has not yet
// been verified and added to consensus.
func (c *Cache) addBlockOutsideConsensus(blk snowman.Block) (snowman.Block, error) {
	wrappedBlk := &BlockWrapper{
		Block: blk,
		cache: c,
	}

	blkID := blk.ID()
	status, err := c.getStatus(blk)
	if err != nil {
		return nil, fmt.Errorf("could not get block status for %s due to %w", blkID, err)
	}
	switch status {
	case choices.Accepted, choices.Rejected:
		c.decidedBlocks.Put(blkID, wrappedBlk)
	case choices.Processing:
		c.unverifiedBlocks.Put(blkID, wrappedBlk)
	default:
		return nil, fmt.Errorf("found unexpected status for blk %s: %s", blkID, status)
	}

	return wrappedBlk, nil
}

// LastAccepted ...
func (c *Cache) LastAccepted() (ids.ID, error) {
	return c.lastAcceptedBlock.ID(), nil
}

// LastAcceptedBlock returns the last accepted wrapped block
func (c *Cache) LastAcceptedBlock() *BlockWrapper {
	return c.lastAcceptedBlock
}

// LastAcceptedBlockInternal returns the internal snowman.Block that was last accepted
func (c *Cache) LastAcceptedBlockInternal() snowman.Block {
	return c.LastAcceptedBlock().Block
}