// (c) 2019-2020, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package evm

import (
	"fmt"

	"github.com/ava-labs/coreth/core/types"
	"github.com/ava-labs/coreth/params"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/choices"
	"github.com/ava-labs/avalanchego/snow/consensus/snowman"
	"github.com/ava-labs/avalanchego/vms/components/missing"
)

// Block implements the snowman.Block interface
type Block struct {
	id       ids.ID
	ethBlock *types.Block
	vm       *VM
}

// ID implements the snowman.Block interface
func (b *Block) ID() ids.ID { return b.id }

// Accept implements the snowman.Block interface
func (b *Block) Accept() error {
	vm := b.vm

	log.Trace(fmt.Sprintf("Block %s is accepted", b.ID()))
	vm.updateStatus(b.id, choices.Accepted)
	if err := vm.acceptedDB.Put(b.ethBlock.Number().Bytes(), b.id[:]); err != nil {
		return err
	}

	tx := vm.getAtomicTx(b.ethBlock)
	if tx == nil {
		return nil
	}
	utx, ok := tx.UnsignedTx.(UnsignedAtomicTx)
	if !ok {
		return errUnknownAtomicTxType
	}

	return utx.Accept(vm.ctx, nil)
}

// Reject implements the snowman.Block interface
// If [b] contains an atomic transaction, attempt to re-issue it
func (b *Block) Reject() error {
	log.Trace(fmt.Sprintf("Block %s is rejected", b.ID()))
	b.vm.updateStatus(b.ID(), choices.Rejected)
	tx := b.vm.getAtomicTx(b.ethBlock)
	if tx != nil {
		if err := b.vm.issueTx(tx); err != nil {
			log.Debug("Failed to re-issue atomic transaction", "txID", tx.ID(), "error", err)
		}
	}
	return nil
}

// Status implements the snowman.Block interface
func (b *Block) Status() choices.Status {
	status := b.vm.getCachedStatus(b.ID())
	if status == choices.Unknown && b.ethBlock != nil {
		return choices.Processing
	}
	return status
}

// Parent implements the snowman.Block interface
func (b *Block) Parent() snowman.Block {
	parentID := ids.ID(b.ethBlock.ParentHash())
	if block := b.vm.getBlock(parentID); block != nil {
		return block
	}
	return &missing.Block{BlkID: parentID}
}

// Height implements the snowman.Block interface
func (b *Block) Height() uint64 {
	return b.ethBlock.Number().Uint64()
}

// Verify implements the snowman.Block interface
func (b *Block) Verify() error {
	// Only enforce a minimum fee when bootstrapping has finished
	if b.vm.ctx.IsBootstrapped() {
		// Ensure the minimum gas price is paid for every transaction
		for _, tx := range b.ethBlock.Transactions() {
			if tx.GasPrice().Cmp(params.MinGasPrice) < 0 {
				return errInsufficientGasPrice
			}
		}
	}

	vm := b.vm
	tx := vm.getAtomicTx(b.ethBlock)
	if tx != nil {
		pState, err := b.vm.chain.BlockState(b.Parent().(*Block).ethBlock)
		if err != nil {
			return fmt.Errorf("failed to retrieve block state due to: %w", err)
		}
		switch atx := tx.UnsignedTx.(type) {
		case *UnsignedImportTx:
			if b.ethBlock.Hash() == vm.genesisHash {
				return nil
			}
			p := b.Parent()
			path := []*Block{}
			inputs := new(ids.Set)
			for {
				if p.Status() == choices.Accepted || p.(*Block).ethBlock.Hash() == vm.genesisHash {
					break
				}
				if ret, hit := vm.blockAtomicInputCache.Get(p.ID()); hit {
					inputs = ret.(*ids.Set)
					break
				}
				path = append(path, p.(*Block))
				p = p.Parent().(*Block)
			}
			for i := len(path) - 1; i >= 0; i-- {
				inputsCopy := new(ids.Set)
				p := path[i]
				atx := vm.getAtomicTx(p.ethBlock)
				if atx != nil {
					inputs.Union(atx.UnsignedTx.(UnsignedAtomicTx).InputUTXOs())
					inputsCopy.Union(*inputs)
				}
				vm.blockAtomicInputCache.Put(p.ID(), inputsCopy)
			}
			for _, in := range atx.InputUTXOs().List() {
				if inputs.Contains(in) {
					return errAtomicInputConflict
				}
			}
		case *UnsignedExportTx:
		default:
			return errUnknownAtomicTxType
		}

		utx := tx.UnsignedTx.(UnsignedAtomicTx)
		if err := utx.SemanticVerify(vm, tx); err != nil {
			return fmt.Errorf("block atomic transaction failed verification due to: %w", err)
		}
		bc := vm.chain.BlockChain()
		_, _, _, err = bc.Processor().Process(b.ethBlock, pState, *bc.GetVMConfig())
		if err != nil {
			return fmt.Errorf("block failed verification during EVM processing due to: %w", err)
		}
	}
	_, err := b.vm.chain.InsertChain([]*types.Block{b.ethBlock})
	return err
}

// Bytes implements the snowman.Block interface
func (b *Block) Bytes() []byte {
	res, err := rlp.EncodeToBytes(b.ethBlock)
	if err != nil {
		panic(err)
	}
	return res
}

func (b *Block) String() string { return fmt.Sprintf("EVM block, ID = %s", b.ID()) }
