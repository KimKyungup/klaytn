// Modifications Copyright 2019 The klaytn Authors
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
// This file is derived from quorum/consensus/istanbul/validator/default.go (2018/06/04).
// Modified and improved for the klaytn development.

package validator

import (
	"errors"
	"fmt"
	"github.com/klaytn/klaytn/common"
	"github.com/klaytn/klaytn/consensus"
	"github.com/klaytn/klaytn/consensus/istanbul"
	"github.com/klaytn/klaytn/params"
	"github.com/klaytn/klaytn/reward"
	"math"
	"math/rand"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

type weightedValidator struct {
	address common.Address

	rewardAddress atomic.Value
	votingPower   uint64 // TODO-Klaytn-Issue1336 This should be updated for governance implementation
	weight        uint64
}

func (val *weightedValidator) Address() common.Address {
	return val.address
}

func (val *weightedValidator) String() string {
	return val.Address().String()
}

func (val *weightedValidator) Equal(val2 *weightedValidator) bool {
	return val.address == val2.address
}

func (val *weightedValidator) Hash() int64 {
	return val.address.Hash().Big().Int64()
}

func (val *weightedValidator) RewardAddress() common.Address {
	rewardAddress := val.rewardAddress.Load()
	if rewardAddress == nil {
		return common.Address{}
	}
	return rewardAddress.(common.Address)
}

func (val *weightedValidator) SetRewardAddress(rewardAddress common.Address) {
	val.rewardAddress.Store(rewardAddress)
}

func (val *weightedValidator) VotingPower() uint64 {
	return val.votingPower
}

func (val *weightedValidator) Weight() uint64 {
	return atomic.LoadUint64(&val.weight)
}

func newWeightedValidator(addr common.Address, reward common.Address, votingpower uint64, weight uint64) istanbul.Validator {
	weightedValidator := &weightedValidator{
		address:     addr,
		votingPower: votingpower,
		weight:      weight,
	}
	weightedValidator.SetRewardAddress(reward)
	return weightedValidator
}

type weightedCouncil struct {
	subSize    uint64
	validators istanbul.Validators
	policy     istanbul.ProposerPolicy

	proposer    atomic.Value // istanbul.Validator
	validatorMu sync.RWMutex
	selector    istanbul.ProposalSelector

	proposers         []istanbul.Validator
	proposersBlockNum uint64 // block number when proposers is determined

	stakingInfo *reward.StakingInfo

	blockNum uint64 // block number when council is determined
}

func RecoverWeightedCouncilProposer(valSet istanbul.ValidatorSet, proposerAddrs []common.Address) {
	weightedCouncil, ok := valSet.(*weightedCouncil)
	if !ok {
		logger.Error("Not weightedCouncil type. Return without recovering.")
		return
	}

	proposers := []istanbul.Validator{}

	for i, proposerAddr := range proposerAddrs {
		_, val := weightedCouncil.GetByAddress(proposerAddr)
		if val == nil {
			logger.Error("Proposer is not available now.", "proposer address", proposerAddr)
		}
		proposers = append(proposers, val)

		// TODO-Klaytn-Issue1166 Disable Trace log later
		logger.Trace("RecoverWeightedCouncilProposer() proposers", "i", i, "address", val.Address().String())
	}
	weightedCouncil.proposers = proposers
}

func NewWeightedCouncil(addrs []common.Address, rewards []common.Address, votingPowers []uint64, weights []uint64, policy istanbul.ProposerPolicy, committeeSize uint64, blockNum uint64, proposersBlockNum uint64, chain consensus.ChainReader) *weightedCouncil {

	if policy != istanbul.WeightedRandom {
		logger.Error("unsupported proposer policy for weighted council", "policy", policy)
		return nil
	}

	valSet := &weightedCouncil{}
	valSet.subSize = committeeSize
	valSet.policy = policy

	// prepare rewards if necessary
	if rewards == nil {
		rewards = make([]common.Address, len(addrs))
		for i := range addrs {
			rewards[i] = common.Address{}
		}
	}

	// prepare weights if necessary
	if weights == nil {
		// initialize with 0 weight.
		weights = make([]uint64, len(addrs))
	}

	// prepare votingPowers if necessary
	if votingPowers == nil {
		votingPowers = make([]uint64, len(addrs))
		if chain == nil {
			logger.Crit("Requires chain to initialize voting powers.")
		}

		//stateDB, err := chain.State()
		//if err != nil {
		//	logger.Crit("Failed to get statedb from chain.")
		//}

		for i := range addrs {
			// TODO-Klaytn-TokenEconomy: Use default value until the formula to calculate votingpower released
			votingPowers[i] = 1000
			//staking := stateDB.GetBalance(addr)
			//if staking.Cmp(common.Big0) == 0 {
			//	votingPowers[i] = 1
			//} else {
			//	votingPowers[i] = 2
			//}
		}
	}

	if len(addrs) != len(rewards) ||
		len(addrs) != len(votingPowers) ||
		len(addrs) != len(weights) {
		logger.Error("incomplete information for weighted council", "num addrs", len(addrs), "num rewards", len(rewards), "num votingPowers", len(votingPowers), "num weights", len(weights))
		return nil
	}

	// init validators
	valSet.validators = make([]istanbul.Validator, len(addrs))
	for i, addr := range addrs {
		valSet.validators[i] = newWeightedValidator(addr, rewards[i], votingPowers[i], weights[i])
	}

	// sort validator
	sort.Sort(valSet.validators)

	// init proposer
	if valSet.Size() > 0 {
		valSet.proposer.Store(valSet.GetByIndex(0))
	}
	valSet.selector = weightedRandomProposer

	valSet.blockNum = blockNum
	valSet.proposers = make([]istanbul.Validator, len(addrs))
	copy(valSet.proposers, valSet.validators)
	valSet.proposersBlockNum = proposersBlockNum

	logger.Trace("Allocate new weightedCouncil", "weightedCouncil", valSet)

	return valSet
}

func GetWeightedCouncilData(valSet istanbul.ValidatorSet) (validators []common.Address, rewardAddrs []common.Address, votingPowers []uint64, weights []uint64, proposers []common.Address, proposersBlockNum uint64) {

	weightedCouncil, ok := valSet.(*weightedCouncil)
	if !ok {
		logger.Error("not weightedCouncil type.")
		return
	}

	if weightedCouncil.Policy() == istanbul.WeightedRandom {
		numVals := len(weightedCouncil.validators)
		validators = make([]common.Address, numVals)
		rewardAddrs = make([]common.Address, numVals)
		votingPowers = make([]uint64, numVals)
		weights = make([]uint64, numVals)
		for i, val := range weightedCouncil.List() {
			weightedVal := val.(*weightedValidator)
			validators[i] = weightedVal.address
			rewardAddrs[i] = weightedVal.RewardAddress()
			votingPowers[i] = weightedVal.votingPower
			weights[i] = atomic.LoadUint64(&weightedVal.weight)
		}

		proposers = make([]common.Address, len(weightedCouncil.proposers))
		for i, proposer := range weightedCouncil.proposers {
			proposers[i] = proposer.Address()
		}
		proposersBlockNum = weightedCouncil.proposersBlockNum
	} else {
		logger.Error("invalid proposer policy for weightedCouncil")
	}
	return
}

func weightedRandomProposer(valSet istanbul.ValidatorSet, lastProposer common.Address, round uint64) istanbul.Validator {
	weightedCouncil, ok := valSet.(*weightedCouncil)
	if !ok {
		logger.Error("weightedRandomProposer() Not weightedCouncil type.")
		return nil
	}

	numProposers := len(weightedCouncil.proposers)
	if numProposers == 0 {
		logger.Error("weightedRandomProposer() No available proposers.")
		return nil
	}

	// At Refresh(), proposers is already randomly shuffled considering weights.
	// So let's just round robin this array
	blockNum := weightedCouncil.blockNum
	picker := (blockNum + round - params.CalcProposerBlockNumber(blockNum+1)) % uint64(numProposers)
	proposer := weightedCouncil.proposers[picker]

	// Enable below more detailed log when debugging
	// logger.Trace("Select a proposer using weighted random", "proposer", proposer.String(), "picker", picker, "blockNum of council", blockNum, "round", round, "blockNum of proposers updated", weightedCouncil.proposersBlockNum, "number of proposers", numProposers, "all proposers", weightedCouncil.proposers)

	return proposer
}

func (valSet *weightedCouncil) Size() uint64 {
	valSet.validatorMu.RLock()
	defer valSet.validatorMu.RUnlock()
	return uint64(len(valSet.validators))
}

func (valSet *weightedCouncil) SubGroupSize() uint64 {
	return valSet.subSize
}

func (valSet *weightedCouncil) SetSubGroupSize(size uint64) {
	valSet.subSize = size
}

func (valSet *weightedCouncil) List() []istanbul.Validator {
	valSet.validatorMu.RLock()
	defer valSet.validatorMu.RUnlock()
	return valSet.validators
}

func (valSet *weightedCouncil) SubList(prevHash common.Hash, view *istanbul.View) []istanbul.Validator {
	return valSet.SubListWithProposer(prevHash, valSet.GetProposer().Address(), view)
}

func (valSet *weightedCouncil) SubListWithProposer(prevHash common.Hash, proposer common.Address, view *istanbul.View) []istanbul.Validator {
	valSet.validatorMu.RLock()
	defer valSet.validatorMu.RUnlock()
	if uint64(len(valSet.validators)) <= valSet.subSize {
		logger.Trace("Council is less than subsize Choose all validators", "valSet.Number", valSet.blockNum, "prevHash", prevHash.Hex(), "proposer", proposer, "committee", valSet.validators, "committee size", len(valSet.validators), "subsize", valSet.subSize)
		return valSet.validators
	}

	hashstring := strings.TrimPrefix(prevHash.Hex(), "0x")
	if len(hashstring) > 15 {
		hashstring = hashstring[:15]
	}

	seed, err := strconv.ParseInt(hashstring, 16, 64)
	if err != nil {
		logger.Error("input", "hash", prevHash.Hex())
		logger.Error("fail to make sub-list of validators", "seed", seed, "err", err)
		return valSet.validators
	}

	// shuffle
	committee := make([]istanbul.Validator, valSet.subSize)
	_, proposerValidator := valSet.getByAddress(proposer)
	if proposerValidator == nil {
		logger.Error("fail to make sub-list of validators, because proposer is invalid", "address of proposer", proposer)
		return valSet.validators
	}
	committee[0] = proposerValidator
	if valSet.subSize == 1 {
		logger.Debug("Sub size is 1, current", "prevHash", prevHash.Hex(), "proposer", proposer, "committee", committee, "committee size", len(committee), "subSize", valSet.subSize)
		return committee
	}
	// next proposer
	nextProposerIdx := uint64(1)
	for committee[1] == nil {
		nextProposer := valSet.selector(valSet, proposer, view.Round.Uint64()+nextProposerIdx)
		if committee[0].Address() != nextProposer.Address() {
			committee[1] = nextProposer
			break
		}
		nextProposerIdx += 1
	}

	proposerIdx, _ := valSet.getByAddress(committee[0].Address())
	nextproposerIdx, _ := valSet.getByAddress(committee[1].Address())

	// TODO-Klaytn-RemoveLater remove this check code if the implementation is stable.
	if proposerIdx < 0 || nextproposerIdx < 0 {
		vals := "["
		for _, v := range valSet.validators {
			vals += fmt.Sprintf("%s,", v.Address().Hex())
		}
		vals += "]"
		logger.Error("current proposer or next proposer not found in Council", "proposerIdx", proposerIdx, "nextproposerIdx", nextproposerIdx, "proposer", committee[0].Address().Hex(),
			"nextproposer", committee[1].Address().Hex(), "validators", vals)
	}

	if proposerIdx == nextproposerIdx {
		logger.Error("fail to make propser", "current proposer idx", proposerIdx, "next idx", nextproposerIdx)
	}

	limit := len(valSet.validators)
	picker := rand.New(rand.NewSource(seed))

	pickSize := limit - 2
	indexs := make([]int, pickSize)
	idx := 0
	for i := 0; i < limit; i++ {
		if i != proposerIdx && i != nextproposerIdx {
			indexs[idx] = i
			idx++
		}
	}
	for i := 0; i < pickSize; i++ {
		randIndex := picker.Intn(pickSize)
		indexs[i], indexs[randIndex] = indexs[randIndex], indexs[i]
	}

	for i := uint64(0); i < valSet.subSize-2; i++ {
		committee[i+2] = valSet.validators[indexs[i]]
	}

	if prevHash.Hex() == "0x0000000000000000000000000000000000000000000000000000000000000000" {
		logger.Debug("### subList", "prevHash", prevHash.Hex())
	}

	logger.Trace("New committee", "valSet.Number", valSet.blockNum, "prevHash", prevHash.Hex(), "proposer", proposer, "committee", committee, "committee size", len(committee), "subSize", valSet.subSize)
	return committee
}

func (valSet *weightedCouncil) CheckInSubList(prevHash common.Hash, view *istanbul.View, addr common.Address) bool {
	for _, val := range valSet.SubList(prevHash, view) {
		if val.Address() == addr {
			return true
		}
	}
	return false
}

func (valSet *weightedCouncil) IsSubSet() bool {
	// TODO-Klaytn-RemoveLater We don't use this interface anymore. Eventually let's remove this function from ValidatorSet interface.
	return valSet.Size() > valSet.subSize
}

func (valSet *weightedCouncil) GetByIndex(i uint64) istanbul.Validator {
	valSet.validatorMu.RLock()
	defer valSet.validatorMu.RUnlock()
	if i < uint64(len(valSet.validators)) {
		return valSet.validators[i]
	}
	return nil
}

func (valSet *weightedCouncil) getByAddress(addr common.Address) (int, istanbul.Validator) {
	for i, val := range valSet.validators {
		if addr == val.Address() {
			return i, val
		}
	}
	return -1, nil
}

func (valSet *weightedCouncil) GetByAddress(addr common.Address) (int, istanbul.Validator) {
	valSet.validatorMu.RLock()
	defer valSet.validatorMu.RUnlock()
	return valSet.getByAddress(addr)
}

func (valSet *weightedCouncil) GetProposer() istanbul.Validator {
	//logger.Trace("GetProposer()", "proposer", valSet.proposer)
	return valSet.proposer.Load().(istanbul.Validator)
}

func (valSet *weightedCouncil) IsProposer(address common.Address) bool {
	_, val := valSet.GetByAddress(address)
	return reflect.DeepEqual(valSet.GetProposer(), val)
}

func (valSet *weightedCouncil) chooseProposerByRoundRobin(lastProposer common.Address, round uint64) istanbul.Validator {
	seed := uint64(0)
	if emptyAddress(lastProposer) {
		seed = round
	} else {
		offset := 0
		if idx, val := valSet.getByAddress(lastProposer); val != nil {
			offset = idx
		}
		seed = uint64(offset) + round
	}
	pick := seed % uint64(len(valSet.validators))
	return valSet.validators[pick]
}

func (valSet *weightedCouncil) CalcProposer(lastProposer common.Address, round uint64) {
	valSet.validatorMu.RLock()
	defer valSet.validatorMu.RUnlock()

	newProposer := valSet.selector(valSet, lastProposer, round)
	if newProposer == nil {
		if len(valSet.validators) == 0 {
			// TODO-Klaytn We must make a policy about the mininum number of validators, which can prevent this case.
			logger.Error("NO VALIDATOR! Use lastProposer as a workaround")
			newProposer = newWeightedValidator(lastProposer, common.Address{}, 0, 0)
		} else {
			logger.Warn("Failed to select a new proposer, thus fall back to roundRobinProposer")
			newProposer = valSet.chooseProposerByRoundRobin(lastProposer, round)
		}
	}

	logger.Debug("Update a proposer", "old", valSet.proposer, "new", newProposer, "last proposer", lastProposer.String(), "round", round, "blockNum of council", valSet.blockNum, "blockNum of proposers", valSet.proposersBlockNum)
	valSet.proposer.Store(newProposer)
}

func (valSet *weightedCouncil) AddValidator(address common.Address) bool {
	valSet.validatorMu.Lock()
	defer valSet.validatorMu.Unlock()
	for _, v := range valSet.validators {
		if v.Address() == address {
			return false
		}
	}

	// TODO-Klaytn-Issue1336 Update for governance implementation. How to determine initial value for rewardAddress and votingPower ?
	valSet.validators = append(valSet.validators, newWeightedValidator(address, common.Address{}, 1000, 0))

	// sort validator
	sort.Sort(valSet.validators)
	return true
}

// removeValidatorFromProposers makes new candidate proposers by removing a validator with given address from existing proposers.
func (valSet *weightedCouncil) removeValidatorFromProposers(address common.Address) {
	newProposers := make([]istanbul.Validator, 0, len(valSet.proposers))

	for _, v := range valSet.proposers {
		if v.Address() != address {
			newProposers = append(newProposers, v)
		}
	}

	valSet.proposers = newProposers
}

func (valSet *weightedCouncil) RemoveValidator(address common.Address) bool {
	valSet.validatorMu.Lock()
	defer valSet.validatorMu.Unlock()

	for i, v := range valSet.validators {
		if v.Address() == address {
			valSet.validators = append(valSet.validators[:i], valSet.validators[i+1:]...)
			valSet.removeValidatorFromProposers(address)
			return true
		}
	}
	return false
}

func (valSet *weightedCouncil) ReplaceValidators(vals []istanbul.Validator) bool {
	valSet.validatorMu.Lock()
	defer valSet.validatorMu.Unlock()

	valSet.validators = istanbul.Validators(make([]istanbul.Validator, len(vals)))
	copy(valSet.validators, istanbul.Validators(vals))
	return true
}

func (valSet *weightedCouncil) GetValidators() []istanbul.Validator {
	return valSet.validators
}

func (valSet *weightedCouncil) Copy() istanbul.ValidatorSet {
	valSet.validatorMu.RLock()
	defer valSet.validatorMu.RUnlock()

	var newWeightedCouncil = weightedCouncil{
		subSize:           valSet.subSize,
		policy:            valSet.policy,
		proposer:          valSet.proposer,
		selector:          valSet.selector,
		stakingInfo:       valSet.stakingInfo,
		proposersBlockNum: valSet.proposersBlockNum,
		blockNum:          valSet.blockNum,
	}
	newWeightedCouncil.validators = make([]istanbul.Validator, len(valSet.validators))
	copy(newWeightedCouncil.validators, valSet.validators)

	newWeightedCouncil.proposers = make([]istanbul.Validator, len(valSet.proposers))
	copy(newWeightedCouncil.proposers, valSet.proposers)

	return &newWeightedCouncil
}

func (valSet *weightedCouncil) F() int {
	if valSet.Size() > valSet.subSize {
		return int(math.Ceil(float64(valSet.subSize)/3)) - 1
	} else {
		return int(math.Ceil(float64(valSet.Size())/3)) - 1
	}
}

func (valSet *weightedCouncil) Policy() istanbul.ProposerPolicy { return valSet.policy }

// Refresh recalculates up-to-date proposers only when blockNum is the proposer update interval.
// It returns an error if it can't make up-to-date proposers
//   (1) due toe wrong parameters
//   (2) due to lack of staking information
// It returns no error when weightedCouncil:
//   (1) already has up-do-date proposers
//   (2) successfully calculated up-do-date proposers
func (valSet *weightedCouncil) Refresh(hash common.Hash, blockNum uint64) error {
	valSet.validatorMu.Lock()
	defer valSet.validatorMu.Unlock()

	if valSet.proposersBlockNum == blockNum {
		// already refreshed
		return nil
	}

	// Check errors
	numValidators := len(valSet.validators)
	if numValidators == 0 {
		return errors.New("No validator")
	}

	hashString := strings.TrimPrefix(hash.Hex(), "0x")
	if len(hashString) > 15 {
		hashString = hashString[:15]
	}
	seed, err := strconv.ParseInt(hashString, 16, 64)
	if err != nil {
		return err
	}

	newStakingInfo := reward.GetStakingInfo(blockNum + 1)

	valSet.stakingInfo = newStakingInfo
	if valSet.stakingInfo == nil {
		// Just return without updating proposer
		return errors.New("skip refreshing proposers due to no staking info")
	}

	weightedValidators, stakingAmounts, err := valSet.getStakingAmountsOfValidators(newStakingInfo)
	if err != nil {
		return err
	}
	totalStaking := calcTotalAmount(weightedValidators, newStakingInfo, stakingAmounts)
	calcWeight(weightedValidators, stakingAmounts, totalStaking)

	valSet.refreshProposers(seed, blockNum)

	logger.Debug("Refresh done.", "blockNum", blockNum, "hash", hash, "valSet.blockNum", valSet.blockNum, "stakingInfo.BlockNum", valSet.stakingInfo.BlockNum)
	logger.Debug("New proposers calculated", "new proposers", valSet.proposers)

	return nil
}

// getStakingAmountsOfValidators calculates stakingAmounts of validators.
// If validators have multiple staking contracts, stakingAmounts will be a sum of stakingAmounts with the same rewardAddress.
//  - []*weightedValidator : a list of validators which type is converted to weightedValidator
//  - []float64 : a list of stakingAmounts.
func (valSet *weightedCouncil) getStakingAmountsOfValidators(stakingInfo *reward.StakingInfo) ([]*weightedValidator, []float64, error) {
	numValidators := len(valSet.validators)
	weightedValidators := make([]*weightedValidator, numValidators)
	stakingAmounts := make([]float64, numValidators)
	addedStaking := make([]bool, len(stakingInfo.CouncilNodeAddrs))

	for vIdx, val := range valSet.validators {
		weightedVal, ok := val.(*weightedValidator)
		if !ok {
			return nil, nil, errors.New(fmt.Sprintf("not weightedValidator. val=%s", val.Address().String()))
		}
		weightedValidators[vIdx] = weightedVal

		sIdx, err := stakingInfo.GetIndexByNodeAddress(weightedVal.address)
		if err == nil {
			rewardAddr := stakingInfo.CouncilRewardAddrs[sIdx]
			weightedVal.SetRewardAddress(rewardAddr)
			stakingAmounts[vIdx] = float64(stakingInfo.CouncilStakingAmounts[sIdx])
			addedStaking[sIdx] = true
		} else {
			weightedVal.SetRewardAddress(common.Address{})
		}
	}

	for sIdx, isAdded := range addedStaking {
		if isAdded {
			continue
		}
		for vIdx, val := range weightedValidators {
			if val.RewardAddress() == stakingInfo.CouncilRewardAddrs[sIdx] {
				stakingAmounts[vIdx] += float64(stakingInfo.CouncilStakingAmounts[sIdx])
				break
			}
		}
	}

	logger.Debug("stakingAmounts of validators", "validators", weightedValidators, "stakingAmounts", stakingAmounts)
	return weightedValidators, stakingAmounts, nil
}

// calcTotalAmount calculates totalAmount of stakingAmounts.
// If UseGini is true, gini is reflected to stakingAmounts.
func calcTotalAmount(weightedValidators []*weightedValidator, stakingInfo *reward.StakingInfo, stakingAmounts []float64) float64 {
	if len(stakingInfo.CouncilNodeAddrs) == 0 {
		return 0
	}
	totalStaking := float64(0)
	if stakingInfo.UseGini {
		var tempStakingAmounts []float64
		for vIdx, val := range weightedValidators {
			_, err := stakingInfo.GetIndexByNodeAddress(val.address)
			if err == nil {
				tempStakingAmounts = append(tempStakingAmounts, stakingAmounts[vIdx])
			}
		}
		stakingInfo.Gini = reward.CalcGiniCoefficient(tempStakingAmounts)

		for i := range stakingAmounts {
			stakingAmounts[i] = math.Round(math.Pow(stakingAmounts[i], 1.0/(1+stakingInfo.Gini)))
			totalStaking += stakingAmounts[i]
		}
	} else {
		for _, stakingAmount := range stakingAmounts {
			totalStaking += stakingAmount
		}
	}

	logger.Debug("calculate totalStaking", "UseGini", stakingInfo.UseGini, "Gini", stakingInfo.Gini, "totalStaking", totalStaking, "stakingAmounts", stakingAmounts)
	return totalStaking
}

// calcWeight updates each validator's weight based on the ratio of its staking amount vs. the total staking amount.
func calcWeight(weightedValidators []*weightedValidator, stakingAmounts []float64, totalStaking float64) {
	localLogger := logger.NewWith()
	if totalStaking > 0 {
		for i, weightedVal := range weightedValidators {
			weight := uint64(math.Round(stakingAmounts[i] * 100 / totalStaking))
			if weight <= 0 {
				// A validator, who holds zero or small stake, has minimum weight, 1.
				weight = 1
			}
			atomic.StoreUint64(&weightedVal.weight, weight)
			localLogger = localLogger.NewWith(weightedVal.String(), weight)
		}
	} else {
		for _, weightedVal := range weightedValidators {
			atomic.StoreUint64(&weightedVal.weight, 0)
			localLogger = localLogger.NewWith(weightedVal.String(), 0)
		}
	}
	localLogger.Debug("calculation weight finished")
}

func (valSet *weightedCouncil) refreshProposers(seed int64, blockNum uint64) {
	var candidateValsIdx []int // This is a slice which stores index of validator. it is used for shuffling

	for index, val := range valSet.validators {
		weight := val.Weight()
		for i := uint64(0); i < weight; i++ {
			candidateValsIdx = append(candidateValsIdx, index)
		}
	}

	if len(candidateValsIdx) == 0 {
		// All validators has zero weight. Let's use all validators as candidate proposers.
		for index := 0; index < len(valSet.validators); index++ {
			candidateValsIdx = append(candidateValsIdx, index)
		}
		logger.Trace("Refresh uses all validators as candidate proposers, because all weight is zero.", "candidateValsIdx", candidateValsIdx)
	}

	proposers := make([]istanbul.Validator, len(candidateValsIdx))

	limit := len(candidateValsIdx)
	picker := rand.New(rand.NewSource(seed))

	// shuffle
	for i := 0; i < limit; i++ {
		randIndex := picker.Intn(limit)
		candidateValsIdx[i], candidateValsIdx[randIndex] = candidateValsIdx[randIndex], candidateValsIdx[i]
	}

	for i := 0; i < limit; i++ {
		proposers[i] = valSet.validators[candidateValsIdx[i]]
		// Below log is too verbose. Use is only when debugging.
		// logger.Trace("Refresh calculates new proposers", "i", i, "proposers[i]", proposers[i].String())
	}

	valSet.proposers = proposers
	valSet.proposersBlockNum = blockNum
}

func (valSet *weightedCouncil) SetBlockNum(blockNum uint64) {
	valSet.blockNum = blockNum
}

func (valSet *weightedCouncil) Proposers() []istanbul.Validator {
	return valSet.proposers
}

func (valSet *weightedCouncil) TotalVotingPower() uint64 {
	sum := uint64(0)
	for _, v := range valSet.List() {
		sum += v.VotingPower()
	}
	return sum
}

func (valSet *weightedCouncil) Selector(valS istanbul.ValidatorSet, lastProposer common.Address, round uint64) istanbul.Validator {
	return valSet.selector(valS, lastProposer, round)
}
