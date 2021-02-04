// Modifications Copyright 2018 The klaytn Authors
// Copyright 2017 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.
//
// This file is derived from quorum/consensus/istanbul/backend/api.go (2018/06/04).
// Modified and improved for the klaytn development.

package backend

import (
	"errors"
	"fmt"
	"github.com/influxdata/influxdb/pkg/deep"
	klaytnApi "github.com/klaytn/klaytn/api"
	"github.com/klaytn/klaytn/blockchain"
	"github.com/klaytn/klaytn/blockchain/types"
	"github.com/klaytn/klaytn/common"
	"github.com/klaytn/klaytn/consensus"
	"github.com/klaytn/klaytn/consensus/istanbul"
	istanbulCore "github.com/klaytn/klaytn/consensus/istanbul/core"
	"github.com/klaytn/klaytn/networks/rpc"
	"math/big"
	"reflect"
	"sort"
)

// API is a user facing RPC API to dump Istanbul state
type API struct {
	chain    consensus.ChainReader
	istanbul *backend
}

// GetSnapshot retrieves the state snapshot at a given block.
func (api *API) GetSnapshot(number *rpc.BlockNumber) (*Snapshot, error) {
	// Retrieve the requested block number (or current if none requested)
	var header *types.Header
	if number == nil || *number == rpc.LatestBlockNumber {
		header = api.chain.CurrentHeader()
	} else {
		header = api.chain.GetHeaderByNumber(uint64(number.Int64()))
	}
	// Ensure we have an actually valid block and return its snapshot
	if header == nil {
		return nil, errUnknownBlock
	}
	return api.istanbul.snapshot(api.chain, header, nil)
}

// GetSnapshotAtHash retrieves the state snapshot at a given block.
func (api *API) GetSnapshotAtHash(hash common.Hash) (*Snapshot, error) {
	header := api.chain.GetHeaderByHash(hash)
	if header == nil {
		return nil, errUnknownBlock
	}
	return api.istanbul.snapshot(api.chain, header, nil)
}

// GetValidators retrieves the list of authorized validators at the specified block.
func (api *API) GetValidators(number *rpc.BlockNumber) ([]common.Address, error) {
	// Retrieve the requested block number (or current if none requested)
	var header *types.Header
	if number == nil || *number == rpc.LatestBlockNumber {
		header = api.chain.CurrentHeader()
	} else {
		header = api.chain.GetHeaderByNumber(uint64(number.Int64()))
	}
	// Ensure we have an actually valid block and return the validators from its snapshot
	if header == nil {
		return nil, errUnknownBlock
	}
	snap, err := api.istanbul.snapshot(api.chain, header, nil)
	if err != nil {
		return nil, err
	}
	return snap.validators(), nil
}

// GetValidatorsAtHash retrieves the state snapshot at a given block.
func (api *API) GetValidatorsAtHash(hash common.Hash) ([]common.Address, error) {
	header := api.chain.GetHeaderByHash(hash)
	if header == nil {
		return nil, errUnknownBlock
	}
	snap, err := api.istanbul.snapshot(api.chain, header, nil)
	if err != nil {
		return nil, err
	}
	return snap.validators(), nil
}

// Candidates returns the current candidates the node tries to uphold and vote on.
func (api *API) Candidates() map[common.Address]bool {
	api.istanbul.candidatesLock.RLock()
	defer api.istanbul.candidatesLock.RUnlock()

	proposals := make(map[common.Address]bool)
	for address, auth := range api.istanbul.candidates {
		proposals[address] = auth
	}
	return proposals
}

// Propose injects a new authorization candidate that the validator will attempt to
// push through.
func (api *API) Propose(address common.Address, auth bool) {
	api.istanbul.candidatesLock.Lock()
	defer api.istanbul.candidatesLock.Unlock()

	api.istanbul.candidates[address] = auth
}

// Discard drops a currently running candidate, stopping the validator from casting
// further votes (either for or against).
func (api *API) Discard(address common.Address) {
	api.istanbul.candidatesLock.Lock()
	defer api.istanbul.candidatesLock.Unlock()

	delete(api.istanbul.candidates, address)
}

// API extended by Klaytn developers
type APIExtension struct {
	chain    consensus.ChainReader
	istanbul *backend
}

var (
	errPendingNotAllowed       = errors.New("pending is not allowed")
	errInternalError           = errors.New("internal error")
	errStartNotPositive        = errors.New("start block number should be positive")
	errEndLargetThanLatest     = errors.New("end block number should be smaller than the latest block number")
	errStartLargerThanEnd      = errors.New("start should be smaller than end")
	errRequestedBlocksTooLarge = errors.New("number of requested blocks should be smaller than 50")
	errRangeNil                = errors.New("range values should not be nil")
	errExtractIstanbulExtra    = errors.New("extract Istanbul Extra from block header of the given block number")
	errNoBlockExist            = errors.New("block with the given block number is not existed")
	errNoBlockNumber           = errors.New("block number is not assigned")
)

// GetCouncil retrieves the list of authorized validators at the specified block.
func (api *APIExtension) GetCouncil(number *rpc.BlockNumber) ([]common.Address, error) {
	// Retrieve the requested block number (or current if none requested)
	var header *types.Header
	if number == nil || *number == rpc.LatestBlockNumber {
		header = api.chain.CurrentHeader()
	} else if *number == rpc.PendingBlockNumber {
		logger.Trace("Cannot get council of the pending block.", "number", number)
		return nil, errPendingNotAllowed
	} else {
		header = api.chain.GetHeaderByNumber(uint64(number.Int64()))
	}
	// Ensure we have an actually valid block and return the council from its snapshot
	if header == nil {
		logger.Trace("Failed to find the requested block", "number", number)
		return nil, errNoBlockExist // return nil if block is not found.
	}
	snap, err := api.istanbul.snapshot(api.chain, header, nil)
	if err != nil {
		logger.Error("Failed to get snapshot.", "hash", header.Hash(), "err", err)
		return nil, errInternalError
	}
	return snap.validators(), nil
}

func (api *APIExtension) GetCouncilSize(number *rpc.BlockNumber) (int, error) {
	council, err := api.GetCouncil(number)
	if err == nil {
		return len(council), nil
	} else {
		return -1, err
	}
}

func (api *APIExtension) GetCommittee(number *rpc.BlockNumber) ([]common.Address, error) {
	// Retrieve the requested block number (or current if none requested)
	var header *types.Header
	if number == nil || *number == rpc.LatestBlockNumber {
		header = api.chain.CurrentHeader()
	} else if *number == rpc.PendingBlockNumber {
		logger.Trace("Cannot get validators of the pending block.", "number", number)
		return nil, errPendingNotAllowed
	} else {
		header = api.chain.GetHeaderByNumber(uint64(number.Int64()))
	}

	if header == nil {
		return nil, errNoBlockExist
	}

	istanbulExtra, err := types.ExtractIstanbulExtra(header)
	if err == nil {
		return istanbulExtra.Validators, nil
	} else {
		return nil, errExtractIstanbulExtra
	}
}

func (api *APIExtension) GetCommitteeSize(number *rpc.BlockNumber) (int, error) {
	committee, err := api.GetCommittee(number)
	if err == nil {
		return len(committee), nil
	} else {
		return -1, err
	}
}

type ValidationResult struct {
	BlockNumber              uint64              `json:"blockNumber"`
	Round                    byte                `json:"round"`
	Proposer                 common.Address      `json:"proposer"`
	ProposerFromBlock        common.Address      `json:"proposerFromBlock"`
	IsValidProposer          bool                `json:"isValidProposer"`
	Committee                common.AddressSlice `json:"committee"`
	CommitteeSealedFromBlock common.AddressSlice `json:"committeeSealedFromBlock"`
	CommitteeFromBlock       common.AddressSlice `json:"committeeFromBlock"`
	IsValidCommittee         bool                `json:"isValidCommittee"`
	IsValidSeal              bool                `json:"isValidSeal"`
}

func (api *APIExtension) ValidateConsensusInfo(block *types.Block) (ValidationResult, error) {
	result := ValidationResult{}

	blockNumber := block.NumberU64()
	if blockNumber == 0 {
		return ValidationResult{}, nil
	}

	result.BlockNumber = blockNumber

	round := block.Header().Round()
	view := &istanbul.View{
		Sequence: new(big.Int).Set(block.Number()),
		Round:    new(big.Int).SetInt64(int64(round)),
	}
	result.Round = round

	parentHash := block.ParentHash()
	parentBlockHeader := api.chain.GetHeader(parentHash, blockNumber-1)
	snap, err := api.istanbul.snapshot(api.chain, parentBlockHeader, nil)
	if err != nil {
		return ValidationResult{}, err
	}

	// get the ProposerFromBlock of this block.
	result.ProposerFromBlock, err = ecrecover(block.Header())
	if err != nil {
		return ValidationResult{}, err
	}

	// calc Proposer
	lastProposer := api.istanbul.GetProposer(blockNumber - 1)
	newValSet := snap.ValSet.Copy()
	newValSet.CalcProposer(lastProposer, uint64(round))
	result.Proposer = newValSet.GetProposer().Address()

	// check Proposer
	result.IsValidProposer = result.ProposerFromBlock == result.Proposer

	// get the Committee list of this block.
	committee := snap.ValSet.SubListWithProposer(parentHash, result.Proposer, view)
	committeeAddrs := make(common.AddressSlice, len(committee))
	for i, v := range committee {
		committeeAddrs[i] = v.Address()
	}
	//sort.Sort(committeeAddrs)
	result.Committee = committeeAddrs

	//verify the Committee list of the block using istanbul
	proposalSeal := istanbulCore.PrepareCommittedSeal(block.Hash())
	extra, err := types.ExtractIstanbulExtra(block.Header())
	committeSealAddr := make(common.AddressSlice, len(extra.CommittedSeal))
	sealErr := false

	for i, seal := range extra.CommittedSeal {
		addr, err := istanbul.GetSignatureAddress(proposalSeal, seal)
		committeSealAddr[i] = addr
		if err != nil {
			return ValidationResult{}, err
		}

		var found bool = false
		for _, v := range committeeAddrs {
			if addr == v {
				found = true
				break
			}
		}
		if found == false {
			sealErr = true
		}
	}

	result.IsValidSeal = sealErr == false

	result.CommitteeSealedFromBlock = committeSealAddr
	result.CommitteeFromBlock = extra.Validators

	//sort.Sort(result.CommitteeSealedFromBlock)
	//sort.Sort(result.CommitteeFromBlock)

	result.IsValidCommittee = deep.Equal(result.Committee, result.CommitteeFromBlock)

	return result, nil
}

type ConsensusInfo struct {
	proposer               common.Address
	originProposer         common.Address // the proposal of 0 Round at the same block number
	roundProposer          common.AddressSlice
	roundCommitte          []common.AddressSlice
	committee              common.AddressSlice
	committeeFromExtraSeal common.AddressSlice
	validatorsFromExtra    common.AddressSlice
	round                  byte
}

func (api *APIExtension) getConsensusInfo(block *types.Block) (ConsensusInfo, error) {
	blockNumber := block.NumberU64()
	if blockNumber == 0 {
		return ConsensusInfo{}, nil
	}

	round := block.Header().Round()
	view := &istanbul.View{
		Sequence: new(big.Int).Set(block.Number()),
		Round:    new(big.Int).SetInt64(int64(round)),
	}

	// get the proposer of this block.
	proposer, err := ecrecover(block.Header())
	if err != nil {
		return ConsensusInfo{}, err
	}

	// get the snapshot of the previous block.
	parentHash := block.ParentHash()
	parentHeader := api.chain.GetHeader(parentHash, blockNumber-1)
	snap, err := api.istanbul.snapshot(api.chain, parentHeader, nil)
	if err != nil {
		return ConsensusInfo{}, err
	}

	// get origin proposer at 0 round.
	originProposer := common.Address{}

	// get all Proposer at each Round
	const maxRound = 11
	roundProposer := make([]common.Address, maxRound)
	roundCommitte := make([]common.AddressSlice, 0, maxRound)
	lastProposer := api.istanbul.GetProposer(blockNumber - 1)

	newValSet := snap.ValSet.Copy()
	newValSet.CalcProposer(lastProposer, 0)
	originProposer = newValSet.GetProposer().Address()

	for i := 0; i < maxRound; i++ {
		vs := snap.ValSet.Copy()
		vs.CalcProposer(lastProposer, uint64(i))
		roundProposer[i] = vs.GetProposer().Address()

		committee := vs.SubList(parentHash, view)
		committeeAddrs := make(common.AddressSlice, len(committee))
		for i, v := range committee {
			committeeAddrs[i] = v.Address()
		}
		sort.Sort(committeeAddrs[2:])
		roundCommitte = append(roundCommitte, committeeAddrs)
	}

	// get the Committee list of this block.
	//snap.ValSet.SubList()
	committee := snap.ValSet.SubListWithProposer(parentHash, proposer, view)
	committeeAddrs := make(common.AddressSlice, len(committee))
	for i, v := range committee {
		committeeAddrs[i] = v.Address()
	}

	sort.Sort(committeeAddrs[2:])
	cInfo := ConsensusInfo{
		proposer:       proposer,
		originProposer: originProposer,
		roundProposer:  roundProposer,
		roundCommitte:  roundCommitte,
		committee:      committeeAddrs,
		round:          round,
	}

	//verify the Committee list of the block using istanbul
	proposalSeal := istanbulCore.PrepareCommittedSeal(block.Hash())
	extra, err := types.ExtractIstanbulExtra(block.Header())
	istanbulAddrs := make(common.AddressSlice, (len(committeeAddrs)-1)*2/3+1)
	sealErr := false

	for i, seal := range extra.CommittedSeal {
		addr, err := istanbul.GetSignatureAddress(proposalSeal, seal)
		istanbulAddrs[i] = addr
		if err != nil {
			return ConsensusInfo{}, err
		}

		var found bool = false
		for _, v := range committeeAddrs {
			if addr == v {
				found = true
				break
			}
		}
		if found == false {
			sealErr = true
			logger.Error("validator is different!", "snap", committeeAddrs, "istanbul", istanbulAddrs)
			//return Proposer, committeeAddrs, errors.New("validator set is different from Istanbul engine!!")
		}
	}

	cInfo.committeeFromExtraSeal = istanbulAddrs
	cInfo.validatorsFromExtra = extra.Validators

	sort.Sort(cInfo.committeeFromExtraSeal)
	sort.Sort(cInfo.validatorsFromExtra[2:])

	if sealErr {
		//		return cInfo, errors.New("validator set is different from Istanbul engine!!")
	}

	return cInfo, nil
}

func (api *APIExtension) makeRPCBlockOutput(b *types.Block,
	cInfo ConsensusInfo, transactions types.Transactions, receipts types.Receipts) map[string]interface{} {
	head := b.Header() // copies the header once
	hash := head.Hash()

	td := big.NewInt(0)
	if bc, ok := api.chain.(*blockchain.BlockChain); ok {
		td = bc.GetTd(hash, b.NumberU64())
	}
	r, err := klaytnApi.RpcOutputBlock(b, td, false, false)
	if err != nil {
		logger.Error("failed to RpcOutputBlock", "err", err)
		return nil
	}

	// make transactions
	numTxs := len(transactions)
	rpcTransactions := make([]map[string]interface{}, numTxs)
	for i, tx := range transactions {
		rpcTransactions[i] = klaytnApi.RpcOutputReceipt(tx, hash, head.Number.Uint64(), uint64(i), receipts[i])
	}

	r["Committee"] = cInfo.committee
	r["committeeFromExtra"] = cInfo.validatorsFromExtra
	r["committeeSealFromExtra"] = cInfo.committeeFromExtraSeal
	r["Proposer"] = cInfo.proposer
	r["Round"] = cInfo.round
	r["originProposer"] = cInfo.originProposer
	r["roundProposer"] = cInfo.roundProposer
	r["roundCommitte"] = cInfo.roundCommitte
	r["transactions"] = rpcTransactions

	return r
}

func (api *APIExtension) ValidateBlock(number *rpc.BlockNumber) (interface{}, error) {
	b, ok := api.chain.(*blockchain.BlockChain)
	if !ok {
		logger.Error("chain is not a type of blockchain.BlockChain", "type", reflect.TypeOf(api.chain))
		return nil, errInternalError
	}
	var block *types.Block
	var blockNumber uint64

	if number == nil {
		logger.Trace("block number is not assigned")
		return nil, errNoBlockNumber
	}

	if *number == rpc.PendingBlockNumber {
		logger.Trace("Cannot get consensus information of the PendingBlock.")
		return nil, errPendingNotAllowed
	}

	if *number == rpc.LatestBlockNumber {
		block = b.CurrentBlock()
		blockNumber = block.NumberU64()
	} else {
		// rpc.EarliestBlockNumber == 0, no need to treat it as a special case.
		blockNumber = uint64(number.Int64())
		block = b.GetBlockByNumber(blockNumber)
	}

	if block == nil {
		logger.Trace("Finding a block by number failed.", "blockNum", blockNumber)
		return nil, fmt.Errorf("the block does not exist (block number: %d)", blockNumber)
	}

	return api.ValidateConsensusInfo(block)
}

// TODO-Klaytn: This API functions should be managed with API functions with namespace "klay"
func (api *APIExtension) GetBlockWithConsensusInfoByNumber(number *rpc.BlockNumber) (map[string]interface{}, error) {
	b, ok := api.chain.(*blockchain.BlockChain)
	if !ok {
		logger.Error("chain is not a type of blockchain.BlockChain", "type", reflect.TypeOf(api.chain))
		return nil, errInternalError
	}
	var block *types.Block
	var blockNumber uint64

	if number == nil {
		logger.Trace("block number is not assigned")
		return nil, errNoBlockNumber
	}

	if *number == rpc.PendingBlockNumber {
		logger.Trace("Cannot get consensus information of the PendingBlock.")
		return nil, errPendingNotAllowed
	}

	if *number == rpc.LatestBlockNumber {
		block = b.CurrentBlock()
		blockNumber = block.NumberU64()
	} else {
		// rpc.EarliestBlockNumber == 0, no need to treat it as a special case.
		blockNumber = uint64(number.Int64())
		block = b.GetBlockByNumber(blockNumber)
	}

	if block == nil {
		logger.Trace("Finding a block by number failed.", "blockNum", blockNumber)
		return nil, fmt.Errorf("the block does not exist (block number: %d)", blockNumber)
	}
	blockHash := block.Hash()

	cInfo, err := api.getConsensusInfo(block)
	if err != nil {
		logger.Error("Getting the proposer and validators failed.", "blockHash", blockHash, "err", err)
		return nil, errInternalError
	}

	receipts := b.GetBlockReceiptsInCache(blockHash)
	if receipts == nil {
		receipts = b.GetReceiptsByBlockHash(blockHash)
	}

	return api.makeRPCBlockOutput(block, cInfo, block.Transactions(), receipts), nil
}

func (api *APIExtension) GetBlockWithConsensusInfoByNumberRange(start *rpc.BlockNumber, end *rpc.BlockNumber) (map[string]interface{}, error) {
	blocks := make(map[string]interface{})

	if start == nil || end == nil {
		logger.Trace("the range values should not be nil.", "start", start, "end", end)
		return nil, errRangeNil
	}

	// check error status.
	s := start.Int64()
	e := end.Int64()
	if s < 0 {
		logger.Trace("start should be positive", "start", s)
		return nil, errStartNotPositive
	}

	eChain := api.chain.CurrentHeader().Number.Int64()
	if e > eChain {
		logger.Trace("end should be smaller than the lastest block number", "end", end, "eChain", eChain)
		return nil, errEndLargetThanLatest
	}

	if s > e {
		logger.Trace("start should be smaller than end", "start", s, "end", e)
		return nil, errStartLargerThanEnd
	}

	if (e - s) > 50 {
		logger.Trace("number of requested blocks should be smaller than 50", "start", s, "end", e)
		return nil, errRequestedBlocksTooLarge
	}

	// gather s~e blocks
	for i := s; i <= e; i++ {
		strIdx := fmt.Sprintf("0x%x", i)

		blockNum := rpc.BlockNumber(i)
		b, err := api.GetBlockWithConsensusInfoByNumber(&blockNum)
		if err != nil {
			logger.Error("error on GetBlockWithConsensusInfoByNumber", "err", err)
			blocks[strIdx] = nil
		} else {
			blocks[strIdx] = b
		}
	}

	return blocks, nil
}

func (api *APIExtension) GetBlockWithConsensusInfoByHash(blockHash common.Hash) (map[string]interface{}, error) {
	b, ok := api.chain.(*blockchain.BlockChain)
	if !ok {
		logger.Error("chain is not a type of blockchain.Blockchain, returning...", "type", reflect.TypeOf(api.chain))
		return nil, errInternalError
	}

	block := b.GetBlockByHash(blockHash)
	if block == nil {
		logger.Trace("Finding a block failed.", "blockHash", blockHash)
		return nil, fmt.Errorf("the block does not exist (block hash: %s)", blockHash.String())
	}

	cInfo, err := api.getConsensusInfo(block)
	if err != nil {
		logger.Error("Getting the proposer and validators failed.", "blockHash", blockHash, "err", err)
		return nil, errInternalError
	}

	receipts := b.GetBlockReceiptsInCache(blockHash)
	if receipts == nil {
		receipts = b.GetReceiptsByBlockHash(blockHash)
	}

	return api.makeRPCBlockOutput(block, cInfo, block.Transactions(), receipts), nil
}

func (api *API) GetTimeout() uint64 {
	return istanbul.DefaultConfig.Timeout
}
