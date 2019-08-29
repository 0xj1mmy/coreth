package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"github.com/ava-labs/coreth"
	"github.com/ava-labs/coreth/eth"
	"github.com/ava-labs/go-ethereum/common"
	"github.com/ava-labs/go-ethereum/common/compiler"
	"github.com/ava-labs/go-ethereum/common/hexutil"
	"github.com/ava-labs/go-ethereum/core"
	"github.com/ava-labs/go-ethereum/core/types"
	"github.com/ava-labs/go-ethereum/log"
	"github.com/ava-labs/go-ethereum/params"
	"go/build"
	"math/big"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func checkError(err error) {
	if err != nil {
		panic(err)
	}
}

func main() {
	// configure the chain
	config := eth.DefaultConfig
	chainConfig := &params.ChainConfig{
		ChainID:             big.NewInt(1),
		HomesteadBlock:      big.NewInt(0),
		DAOForkBlock:        big.NewInt(0),
		DAOForkSupport:      true,
		EIP150Block:         big.NewInt(0),
		EIP150Hash:          common.HexToHash("0x2086799aeebeae135c246c65021c82b4e15a2c451340993aacfd2751886514f0"),
		EIP155Block:         big.NewInt(0),
		EIP158Block:         big.NewInt(0),
		ByzantiumBlock:      big.NewInt(0),
		ConstantinopleBlock: big.NewInt(0),
		PetersburgBlock:     big.NewInt(0),
		IstanbulBlock:       nil,
		Ethash:              nil,
	}

	// configure the genesis block
	genBalance := big.NewInt(100000000000000000)
	genKey, _ := coreth.NewKey(rand.Reader)

	config.Genesis = &core.Genesis{
		Config:     chainConfig,
		Nonce:      0,
		Number:     0,
		ExtraData:  hexutil.MustDecode("0x00"),
		GasLimit:   100000000,
		Difficulty: big.NewInt(0),
		Alloc:      core.GenesisAlloc{genKey.Address: {Balance: genBalance}},
	}

	// grab the control of block generation and disable auto uncle
	config.Miner.ManualMining = true
	config.Miner.ManualUncle = true

	// compile the smart contract
	gopath := os.Getenv("GOPATH")
	if gopath == "" {
		gopath = build.Default.GOPATH
	}
	counterSrc, err := filepath.Abs(gopath + "/src/github.com/ava-labs/coreth/examples/counter/counter.sol")
	checkError(err)
	contracts, err := compiler.CompileSolidity("", counterSrc)
	checkError(err)
	contract, _ := contracts[fmt.Sprintf("%s:%s", counterSrc, "Counter")]

	// info required to generate a transaction
	chainID := chainConfig.ChainID
	nonce := uint64(0)
	gasLimit := 10000000
	gasPrice := big.NewInt(1000000000)

	blockCount := 0
	chain := coreth.NewETHChain(&config, nil)
	firstBlock := false
	var contractAddr common.Address
	postGen := func(block *types.Block) bool {
		if blockCount == 15 {
			state, err := chain.CurrentState()
			checkError(err)
			log.Info(fmt.Sprintf("genesis balance = %s", state.GetBalance(genKey.Address)))
			log.Info(fmt.Sprintf("contract balance = %s", state.GetBalance(contractAddr)))
			log.Info(fmt.Sprintf("state = %s", state.Dump(true, false, true)))
			log.Info(fmt.Sprintf("x = %s", state.GetState(contractAddr, common.BigToHash(big.NewInt(0))).String()))
			return true
		}
		if !firstBlock {
			firstBlock = true
			receipts := chain.GetReceiptsByHash(block.Hash())
			if len(receipts) != 1 {
				panic("# receipts is not 1")
			}
			contractAddr = receipts[0].ContractAddress
			txHash := receipts[0].TxHash
			log.Info(fmt.Sprintf("deploy tx = %s", txHash.String()))
			log.Info(fmt.Sprintf("contract addr = %s", contractAddr.String()))
			call := common.Hex2Bytes("1003e2d20000000000000000000000000000000000000000000000000000000000000001")
			state, _ := chain.CurrentState()
			log.Info(fmt.Sprintf("code = %s", hex.EncodeToString(state.GetCode(contractAddr))))
			go func() {
				for i := 0; i < 10; i++ {
					tx := types.NewTransaction(nonce, contractAddr, big.NewInt(0), uint64(gasLimit), gasPrice, call)
					signedTx, err := types.SignTx(tx, types.NewEIP155Signer(chainID), genKey.PrivateKey)
					checkError(err)
					chain.AddRemoteTxs([]*types.Transaction{signedTx})
					time.Sleep(1000 * time.Millisecond)
					nonce++
				}
			}()
		}
		return false
	}
	chain.SetOnSeal(func(block *types.Block) error {
		go func() {
			// the minimum time gap is 1s
			time.Sleep(1000 * time.Millisecond)
			// generate 15 blocks
			blockCount++
			if postGen(block) {
				return
			}
			chain.GenBlock()
		}()
		return nil
	})

	// start the chain
	chain.Start()

	_ = contract
	code := common.Hex2Bytes(contract.Code[2:])
	tx := types.NewContractCreation(nonce, big.NewInt(0), uint64(gasLimit), gasPrice, code)
	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(chainID), genKey.PrivateKey)
	checkError(err)
	chain.AddRemoteTxs([]*types.Transaction{signedTx})
	time.Sleep(1000 * time.Millisecond)
	nonce++

	chain.GenBlock()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	signal.Notify(c, os.Interrupt, syscall.SIGINT)
	<-c
	chain.Stop()
}