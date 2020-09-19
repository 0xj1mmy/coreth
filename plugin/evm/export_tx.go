// (c) 2019-2020, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package evm

import (
	"fmt"
	"math/big"

	"github.com/ava-labs/coreth/core/state"

	"github.com/ava-labs/avalanchego/chains/atomic"
	"github.com/ava-labs/avalanchego/database"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow"
	"github.com/ava-labs/avalanchego/utils/crypto"
	safemath "github.com/ava-labs/avalanchego/utils/math"
	"github.com/ava-labs/avalanchego/vms/components/avax"
	"github.com/ava-labs/avalanchego/vms/secp256k1fx"
	"github.com/ethereum/go-ethereum/log"
)

// UnsignedExportTx is an unsigned ExportTx
type UnsignedExportTx struct {
	avax.Metadata
	// true iff this transaction has already passed syntactic verification
	syntacticallyVerified bool
	// ID of the network on which this tx was issued
	NetworkID uint32 `serialize:"true" json:"networkID"`
	// ID of this blockchain.
	BlockchainID ids.ID `serialize:"true" json:"blockchainID"`
	// Which chain to send the funds to
	DestinationChain ids.ID `serialize:"true" json:"destinationChain"`
	// Inputs
	Ins []EVMInput `serialize:"true" json:"inputs"`
	// Outputs that are exported to the chain
	ExportedOutputs []*avax.TransferableOutput `serialize:"true" json:"exportedOutputs"`
}

// InputUTXOs returns an empty set
func (tx *UnsignedExportTx) InputUTXOs() ids.Set { return ids.Set{} }

// Verify this transaction is well-formed
func (tx *UnsignedExportTx) Verify(
	avmID ids.ID,
	ctx *snow.Context,
	feeAmount uint64,
	feeAssetID ids.ID,
) error {
	switch {
	case tx == nil:
		return errNilTx
	case tx.syntacticallyVerified: // already passed syntactic verification
		return nil
	case tx.DestinationChain.IsZero():
		return errWrongChainID
	case !tx.DestinationChain.Equals(avmID):
		return errWrongChainID
	case len(tx.ExportedOutputs) == 0:
		return errNoExportOutputs
	case tx.NetworkID != ctx.NetworkID:
		return errWrongNetworkID
	case !ctx.ChainID.Equals(tx.BlockchainID):
		return errWrongBlockchainID
	}

	for _, in := range tx.Ins {
		if err := in.Verify(); err != nil {
			return err
		}
	}

	for _, out := range tx.ExportedOutputs {
		if err := out.Verify(); err != nil {
			return err
		}
	}
	if !avax.IsSortedTransferableOutputs(tx.ExportedOutputs, Codec) {
		return errOutputsNotSorted
	}

	tx.syntacticallyVerified = true
	return nil
}

// SemanticVerify this transaction is valid.
func (tx *UnsignedExportTx) SemanticVerify(
	vm *VM,
	stx *Tx,
) TxError {
	if err := tx.Verify(vm.ctx.XChainID, vm.ctx, vm.txFee, vm.ctx.AVAXAssetID); err != nil {
		return permError{err}
	}

	f := crypto.FactorySECP256K1R{}
	for i, cred := range stx.Creds {
		if err := cred.Verify(); err != nil {
			return permError{err}
		}
		pubKey, err := f.RecoverPublicKey(tx.UnsignedBytes(), cred.(*secp256k1fx.Credential).Sigs[0][:])
		if err != nil {
			return permError{err}
		}
		if tx.Ins[i].Address != PublicKeyToEthAddress(pubKey) {
			return permError{errPublicKeySignatureMismatch}
		}
	}

	// do flow-checking
	fc := avax.NewFlowChecker()
	fc.Produce(vm.ctx.AVAXAssetID, vm.txFee)

	for _, out := range tx.ExportedOutputs {
		fc.Produce(out.AssetID(), out.Output().Amount())
	}

	for _, in := range tx.Ins {
		fc.Consume(in.AssetID, in.Amount)
	}

	if err := fc.Verify(); err != nil {
		return permError{err}
	}

	// TODO: verify UTXO outputs via gRPC
	return nil
}

// Accept this transaction.
func (tx *UnsignedExportTx) Accept(ctx *snow.Context, _ database.Batch) error {
	txID := tx.ID()

	elems := make([]*atomic.Element, len(tx.ExportedOutputs))
	for i, out := range tx.ExportedOutputs {
		utxo := &avax.UTXO{
			UTXOID: avax.UTXOID{
				TxID:        txID,
				OutputIndex: uint32(i),
			},
			Asset: avax.Asset{ID: out.AssetID()},
			Out:   out.Out,
		}

		utxoBytes, err := Codec.Marshal(utxo)
		if err != nil {
			return err
		}

		elem := &atomic.Element{
			Key:   utxo.InputID().Bytes(),
			Value: utxoBytes,
		}
		if out, ok := utxo.Out.(avax.Addressable); ok {
			elem.Traits = out.Addresses()
		}

		elems[i] = elem
	}

	return ctx.SharedMemory.Put(tx.DestinationChain, elems)
}

// Create a new transaction
func (vm *VM) newExportTx(
	assetID ids.ID, // AssetID of the tokens to export
	amount uint64, // Amount of tokens to export
	chainID ids.ID, // Chain to send the UTXOs to
	to ids.ShortID, // Address of chain recipient
	keys []*crypto.PrivateKeySECP256K1R, // Pay the fee and provide the tokens
) (*Tx, error) {
	if !vm.ctx.XChainID.Equals(chainID) {
		return nil, errWrongChainID
	}

	var toBurn uint64
	var err error
	if assetID == vm.ctx.AVAXAssetID {
		toBurn, err = safemath.Add64(amount, vm.txFee)
		if err != nil {
			return nil, errOverflowExport
		}
	} else {
		toBurn = vm.txFee
	}
	// burn AVAX
	ins, signers, err := vm.GetSpendableCanonical(keys, vm.ctx.AVAXAssetID, toBurn)
	if err != nil {
		return nil, fmt.Errorf("couldn't generate tx inputs/outputs: %w", err)
	}

	// burn non-AVAX
	if assetID != vm.ctx.AVAXAssetID {
		ins2, signers2, err := vm.GetSpendableCanonical(keys, assetID, amount)
		if err != nil {
			return nil, fmt.Errorf("couldn't generate tx inputs/outputs: %w", err)
		}
		ins = append(ins, ins2...)
		signers = append(signers, signers2...)
	}

	// Create the transaction
	utx := &UnsignedExportTx{
		NetworkID:        vm.ctx.NetworkID,
		BlockchainID:     vm.ctx.ChainID,
		DestinationChain: chainID,
		Ins:              ins,
		ExportedOutputs: []*avax.TransferableOutput{{ // Exported to X-Chain
			Asset: avax.Asset{ID: assetID},
			Out: &secp256k1fx.TransferOutput{
				Amt: amount,
				OutputOwners: secp256k1fx.OutputOwners{
					Locktime:  0,
					Threshold: 1,
					Addrs:     []ids.ShortID{to},
				},
			},
		}},
	}
	tx := &Tx{UnsignedTx: utx}
	if err := tx.Sign(vm.codec, signers); err != nil {
		return nil, err
	}
	return tx, utx.Verify(vm.ctx.XChainID, vm.ctx, vm.txFee, vm.ctx.AVAXAssetID)
}

func (tx *UnsignedExportTx) EVMStateTransfer(vm *VM, state *state.StateDB) error {
	for _, from := range tx.Ins {
		log.Info("crosschain C->X", "addr", from.Address, "amount", from.Amount)
		amount := new(big.Int).Mul(
			new(big.Int).SetUint64(from.Amount), x2cRate)
		if from.AssetID == vm.ctx.AVAXAssetID {
			if state.GetBalance(from.Address).Cmp(amount) < 0 {
				return errInsufficientFunds
			}
			state.SubBalance(from.Address, amount)
		} else {
			assetID := from.AssetID.Key()
			if state.GetBalanceMultiCoin(from.Address, assetID).Cmp(amount) < 0 {
				return errInsufficientFunds
			}
			state.SubBalanceMultiCoin(from.Address, assetID, amount)
		}
		if state.GetNonce(from.Address) != from.Nonce {
			return errInvalidNonce
		}
		state.SetNonce(from.Address, from.Nonce+1)
	}
	return nil
}