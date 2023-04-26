package polybft

import (
	"bytes"
	"fmt"
	"math/big"
	"sort"

	"github.com/0xPolygon/polygon-edge/consensus/polybft/bitmap"
	"github.com/0xPolygon/polygon-edge/consensus/polybft/contractsapi"
	bls "github.com/0xPolygon/polygon-edge/consensus/polybft/signer"
	"github.com/0xPolygon/polygon-edge/helper/hex"
	"github.com/0xPolygon/polygon-edge/txrelayer"
	"github.com/0xPolygon/polygon-edge/types"
	"github.com/hashicorp/go-hclog"
	"github.com/umbracle/ethgo"
)

var (
	// getValidatorABI is an ABI method on SupernetManager contract
	// that returns the validator data
	getValidatorABI, _ = contractsapi.CustomSupernetManager.Abi.Methods["getValidator"]

	bigZero = big.NewInt(0)
)

// StakeManager interface provides functions for handling stake change of validators
// and updating validator set based on changed stake
type StakeManager interface {
	PostBlock(req *PostBlockRequest) error
	UpdateValidatorSet(epoch uint64, currentValidatorSet AccountSet) (*ValidatorSetDelta, error)
}

// dummyStakeManager is a dummy implementation of StakeManager interface
// used only for unit testing
type dummyStakeManager struct{}

func (d *dummyStakeManager) PostBlock(req *PostBlockRequest) error { return nil }
func (d *dummyStakeManager) UpdateValidatorSet(epoch uint64,
	currentValidatorSet AccountSet) (*ValidatorSetDelta, error) {
	return &ValidatorSetDelta{}, nil
}

var _ StakeManager = (*stakeManager)(nil)

// stakeManager saves transfer events that happened in each block
// and calculates updated validator set based on changed stake
type stakeManager struct {
	logger                  hclog.Logger
	state                   *State
	rootChainRelayer        txrelayer.TxRelayer
	key                     ethgo.Key
	validatorSetContract    types.Address
	supernetManagerContract types.Address
	maxValidatorSetSize     int
}

// newStakeManager returns a new instance of stake manager
func newStakeManager(
	logger hclog.Logger,
	state *State,
	relayer txrelayer.TxRelayer,
	key ethgo.Key,
	validatorSetAddr, supernetManagerAddr types.Address,
	maxValidatorSetSize int,
) *stakeManager {
	return &stakeManager{
		logger:                  logger,
		state:                   state,
		rootChainRelayer:        relayer,
		key:                     key,
		validatorSetContract:    validatorSetAddr,
		supernetManagerContract: supernetManagerAddr,
		maxValidatorSetSize:     maxValidatorSetSize,
	}
}

// PostBlock is called on every insert of finalized block (either from consensus or syncer)
// It will read any exit event that happened in block and insert it to state boltDb
func (s *stakeManager) PostBlock(req *PostBlockRequest) error {
	epoch := req.Epoch

	if req.IsEpochEndingBlock {
		// transfer events that happened in epoch ending blocks,
		// should be added to the bucket of the next epoch
		epoch++
	}

	// commit exit events only when we finalize a block
	events, err := s.getTransferEventsFromReceipts(epoch, req.FullBlock.Receipts)
	if err != nil {
		return err
	}

	if len(events) > 0 {
		s.logger.Debug("Gotten transfer (stake changed) events from logs on block",
			"eventsNum", len(events), "block", req.FullBlock.Block.Number())
	}

	return s.state.StakeStore.insertTransferEvents(epoch, events)
}

// UpdateValidatorSet returns an updated validator set
// based on stake change (transfer) events from ValidatorSet contract
func (s *stakeManager) UpdateValidatorSet(epoch uint64, currentValidatorSet AccountSet) (*ValidatorSetDelta, error) {
	s.logger.Info("Calculating validators set update...", "epoch", epoch)

	transferEvents, err := s.state.StakeStore.getTransferEvents(epoch)
	if err != nil {
		return nil, fmt.Errorf("failed to get transfer events for epoch: %d. Error: %w", epoch, err)
	}

	if len(transferEvents) == 0 {
		s.logger.Info("Calculating validators set finished. No transfer events for given epoch.",
			"epoch", epoch)

		return &ValidatorSetDelta{}, nil
	}

	// stake counter holds sorted stakes by current and (possible) new validators
	// he will add to map current stake (voting power) of the current validators
	// on object instantiation
	stakeCounter := newStakeCounter(currentValidatorSet.Copy())

	// update the stake counter with stake changes from transfer events
	for _, event := range transferEvents {
		if event.IsStake() {
			// then this amount was minted To validator address
			stakeCounter.addStake(event.To, event.Value)
		} else if event.IsUnstake() {
			// then this amount was burned From validator address
			stakeCounter.removeStake(event.From, event.Value)
		} else {
			// this should not happen, but lets log it if it does
			s.logger.Debug("Found a transfer event that represents neither stake nor unstake")
		}
	}

	removedBitmap := bitmap.Bitmap{}
	updatedValidators := AccountSet{}
	addedValidators := AccountSet{}

	// sort validators by stake since we update the validator set based on highest stakes
	// this also returns sorted slice of validators (stake, address) pairs
	stakeInfos := stakeCounter.sortByStake(s.maxValidatorSetSize)

	for addr, v := range stakeCounter.currentValidatorSet {
		// remove existing validators from validator set if they
		// did not make it to the set
		if _, exists := stakeCounter.stakeMap[addr]; !exists {
			removedBitmap.Set(v.index)
		}
	}

	for _, stakeInfo := range stakeInfos {
		// check if its a current validator
		if currentValidator, exists := stakeCounter.currentValidatorSet[stakeInfo.address]; exists {
			if stakeInfo.stake.Cmp(bigZero) == 0 {
				// validator un-staked all, remove it from validator set
				removedBitmap.Set(currentValidator.index)

				s.logger.Debug("Validator removed from validator set since he unstaked all",
					"validator", stakeInfo.address)
			} else if stakeInfo.stake.Cmp(currentValidator.VotingPower) != 0 {
				// validator updated its stake so put it in the updated validators list
				currentValidator.VotingPower = new(big.Int).Set(stakeInfo.stake)
				updatedValidators = append(updatedValidators, currentValidator.ValidatorMetadata)

				s.logger.Debug("Validator updated its stake and remains in validator set",
					"validator", stakeInfo.address, "newVotingPower", currentValidator.VotingPower)
			} else {
				// he did not change stake so he will remain in the validator set but will not be in delta
				s.logger.Debug("Validator did not change its stake, but remains in validator set",
					"validator", stakeInfo.address, "votingPower", currentValidator.VotingPower)
			}
		} else {
			// its a new validator, add it to delta in added validators
			validatorData, err := s.getNewValidatorInfo(stakeInfo.address, stakeInfo.stake)
			if err != nil {
				return nil, fmt.Errorf("could not retrieve validator data. Address: %v. Error: %w",
					stakeInfo.address, err)
			}

			addedValidators = append(addedValidators, validatorData)

			s.logger.Debug("New validator added to validator set",
				"validator", stakeInfo.address, "votingPower", validatorData.VotingPower)
		}
	}

	s.logger.Info("Calculating validators set update finished.", "epoch", epoch)

	delta := &ValidatorSetDelta{
		Added:   addedValidators,
		Updated: updatedValidators,
		Removed: removedBitmap,
	}

	if s.logger.IsDebug() {
		newValidatorSet, err := currentValidatorSet.Copy().ApplyDelta(delta)
		if err != nil {
			return nil, err
		}

		s.logger.Debug("New validator set", "validatorSet", newValidatorSet)
	}

	return delta, nil
}

// getTransferEventsFromReceipts parses logs from receipts to find transfer events
func (s *stakeManager) getTransferEventsFromReceipts(epoch uint64,
	receipts []*types.Receipt) ([]*contractsapi.TransferEvent, error) {
	events := make([]*contractsapi.TransferEvent, 0)

	for i := 0; i < len(receipts); i++ {
		if receipts[i].Status == nil || *receipts[i].Status != types.ReceiptSuccess {
			continue
		}

		for _, log := range receipts[i].Logs {
			if log.Address != s.validatorSetContract {
				continue
			}

			var transferEvent contractsapi.TransferEvent

			doesMatch, err := transferEvent.ParseLog(convertLog(log))
			if err != nil {
				return nil, err
			}

			if !doesMatch {
				continue
			}

			events = append(events, &transferEvent)
		}
	}

	return events, nil
}

// getValidatorInfo returns data for new validator (bls key, is active) from the supernet contract
func (s *stakeManager) getNewValidatorInfo(address types.Address, stake *big.Int) (*ValidatorMetadata, error) {
	encoded, err := getValidatorABI.Encode([]interface{}{address})
	if err != nil {
		return nil, err
	}

	response, err := s.rootChainRelayer.Call(
		s.key.Address(),
		ethgo.Address(s.supernetManagerContract),
		encoded)
	if err != nil {
		return nil, fmt.Errorf("failed to invoke validators function on the supernet manager: %w", err)
	}

	byteResponse, err := hex.DecodeHex(response)
	if err != nil {
		return nil, fmt.Errorf("unable to decode hex response, %w", err)
	}

	decoded, err := getValidatorABI.Outputs.Decode(byteResponse)
	if err != nil {
		return nil, err
	}

	//nolint:godox
	// TODO - @goran-ethernal change this to use the generated stub
	// once we remove old ChildValidatorSet stubs and generate new ones
	// from the new contract
	output, ok := decoded.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("could not convert decoded outputs to map")
	}

	outputMap, ok := output["0"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("could not convert decoded outputs to map")
	}

	blsKey, ok := outputMap["blsKey"].([4]*big.Int)
	if !ok {
		return nil, fmt.Errorf("failed to decode blskey")
	}

	pubKey, err := bls.UnmarshalPublicKeyFromBigInt(blsKey)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal BLS public key: %w", err)
	}

	return &ValidatorMetadata{
		Address:     address,
		VotingPower: stake,
		BlsKey:      pubKey,
		IsActive:    true,
	}, nil
}

// stakeInfo holds info about validator stake
type stakeInfo struct {
	stake   *big.Int
	address types.Address
}
type expandedValidatorMetadata struct {
	*ValidatorMetadata
	index uint64
}

// stakeCounter sorts and returns stake info for all validators
type stakeCounter struct {
	stakeMap            map[types.Address]*big.Int
	currentValidatorSet map[types.Address]*expandedValidatorMetadata
}

// newStakeCounter returns a new instance of stake counter
func newStakeCounter(currentValidatorSet AccountSet) *stakeCounter {
	stakeCounter := &stakeCounter{
		stakeMap:            make(map[types.Address]*big.Int),
		currentValidatorSet: make(map[types.Address]*expandedValidatorMetadata, len(currentValidatorSet)),
	}

	for i, v := range currentValidatorSet {
		stakeCounter.stakeMap[v.Address] = new(big.Int).Set(v.VotingPower)
		stakeCounter.currentValidatorSet[v.Address] = &expandedValidatorMetadata{
			ValidatorMetadata: v,
			index:             uint64(i),
		}
	}

	return stakeCounter
}

// addStake adds given amount for a validator to stake map
func (sc *stakeCounter) addStake(address types.Address, amount *big.Int) {
	if stakeInfo, exists := sc.stakeMap[address]; exists {
		stakeInfo.Add(stakeInfo, amount)
	} else {
		sc.stakeMap[address] = new(big.Int).Set(amount)
	}
}

// removeStake removes given amount for a validator from stake map
func (sc *stakeCounter) removeStake(address types.Address, amount *big.Int) {
	stakeInfo := sc.stakeMap[address]
	stakeInfo.Sub(stakeInfo, amount)
}

// sortByStake returns all validator pairs (address, stake) in sorted order
// also remove addresses from sc.stakeMap that are after maxValidatorSetSize
func (sc *stakeCounter) sortByStake(maxValidatorSetSize int) []stakeInfo {
	stakeInfos := make([]stakeInfo, 0, len(sc.stakeMap))
	for k, v := range sc.stakeMap {
		stakeInfos = append(stakeInfos, stakeInfo{
			address: k,
			stake:   v,
		})
	}

	sort.Slice(stakeInfos, func(i, j int) bool {
		v1, v2 := stakeInfos[i], stakeInfos[j]

		switch v1.stake.Cmp(v2.stake) {
		case 1:
			return true
		case 0:
			return bytes.Compare(v1.address[:], v2.address[:]) < 0
		default:
			return false
		}
	})

	if len(stakeInfos) <= maxValidatorSetSize {
		return stakeInfos
	}

	for _, si := range stakeInfos[maxValidatorSetSize:] {
		delete(sc.stakeMap, si.address)
	}

	return stakeInfos[:maxValidatorSetSize]
}