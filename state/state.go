package state

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"time"

	tmstate "github.com/tendermint/tendermint/proto/state"
	tmproto "github.com/tendermint/tendermint/proto/types"
	protoversion "github.com/tendermint/tendermint/proto/version"
	"github.com/tendermint/tendermint/types"
	tmtime "github.com/tendermint/tendermint/types/time"
	"github.com/tendermint/tendermint/version"
)

// database keys
var (
	stateKey = []byte("stateKey")
)

//-----------------------------------------------------------------------------

// initStateVersion sets the Consensus.Block and Software versions,
// but leaves the Consensus.App version blank.
// The Consensus.App version will be set during the Handshake, once
// we hear from the app what protocol version it is running.
var initStateVersion = tmstate.Version{
	Consensus: protoversion.Consensus{
		Block: version.BlockProtocol,
		App:   0,
	},
	Software: version.TMCoreSemVer,
}

//-----------------------------------------------------------------------------

// State is a short description of the latest committed block of the Tendermint consensus.
// It keeps all information necessary to validate new blocks,
// including the last validator set and the consensus params.
// All fields are exposed so the struct can be easily serialized,
// but none of them should be mutated directly.
// Instead, use state.Copy() or state.NextState(...).
// NOTE: not goroutine-safe.
type State struct {
	Version tmstate.Version

	// immutable
	ChainID string

	// LastBlockHeight=0 at genesis (ie. block(H=0) does not exist)
	LastBlockHeight int64
	LastBlockID     types.BlockID
	LastBlockTime   time.Time

	// LastValidators is used to validate block.LastCommit.
	// Validators are persisted to the database separately every time they change,
	// so we can query for historical validator sets.
	// Note that if s.LastBlockHeight causes a valset change,
	// we set s.LastHeightValidatorsChanged = s.LastBlockHeight + 1 + 1
	// Extra +1 due to nextValSet delay.
	NextValidators              *types.ValidatorSet
	Validators                  *types.ValidatorSet
	LastValidators              *types.ValidatorSet
	LastHeightValidatorsChanged int64

	// Consensus parameters used for validating blocks.
	// Changes returned by EndBlock and updated after Commit.
	ConsensusParams                  tmproto.ConsensusParams
	LastHeightConsensusParamsChanged int64

	// Merkle root of the results from executing prev block
	LastResultsHash []byte

	// the latest AppHash we've received from calling abci.Commit()
	AppHash []byte
}

// Copy makes a copy of the State for mutating.
func (state State) Copy() State {

	return State{
		Version: state.Version,
		ChainID: state.ChainID,

		LastBlockHeight: state.LastBlockHeight,
		LastBlockID:     state.LastBlockID,
		LastBlockTime:   state.LastBlockTime,

		NextValidators:              state.NextValidators.Copy(),
		Validators:                  state.Validators.Copy(),
		LastValidators:              state.LastValidators.Copy(),
		LastHeightValidatorsChanged: state.LastHeightValidatorsChanged,

		ConsensusParams:                  state.ConsensusParams,
		LastHeightConsensusParamsChanged: state.LastHeightConsensusParamsChanged,

		AppHash: state.AppHash,

		LastResultsHash: state.LastResultsHash,
	}
}

// Equals returns true if the States are identical.
func (state State) Equals(state2 State) bool {
	sbz, s2bz := state.Bytes(), state2.Bytes()
	return bytes.Equal(sbz, s2bz)
}

// Bytes serializes the State using go-amino.
func (state State) Bytes() []byte {
	sm, err := state.ToProto()
	if err != nil {
		panic(err)
	}
	bz, err := sm.Marshal()
	if err != nil {
		panic(err)
	}
	return bz
}

// IsEmpty returns true if the State is equal to the empty State.
func (state State) IsEmpty() bool {
	return state.Validators == nil // XXX can't compare to Empty
}

func (state *State) ToProto() (*tmstate.State, error) {
	if state == nil {
		return nil, errors.New("state is nil")
	}
	sm := new(tmstate.State)

	sm.Version = state.Version
	sm.ChainID = state.ChainID
	sm.LastBlockHeight = state.LastBlockHeight
	bip := state.LastBlockID.ToProto()
	sm.LastBlockID = *bip
	sm.LastBlockTime = state.LastBlockTime

	if state.Validators == nil {
		state.Validators = &types.ValidatorSet{}
	} else {
		vals, err := state.Validators.ToProto()
		if err != nil {
			return nil, err
		}
		sm.Validators = vals
	}
	if state.NextValidators == nil {
		state.NextValidators = &types.ValidatorSet{}
	} else {
		nVals, err := state.NextValidators.ToProto()
		if err != nil {
			return nil, err
		}
		sm.NextValidators = nVals
	}
	if state.LastValidators == nil {
		state.LastValidators = &types.ValidatorSet{}
	} else {
		lVals, err := state.LastValidators.ToProto()
		if err != nil {
			return nil, err
		}
		sm.LastValidators = lVals
	}

	sm.LastHeightValidatorsChanged = state.LastHeightValidatorsChanged
	sm.ConsensusParams = state.ConsensusParams
	sm.LastHeightConsensusParamsChanged = state.LastHeightConsensusParamsChanged
	sm.LastResultsHash = state.LastResultsHash
	sm.AppHash = state.AppHash

	return sm, nil
}

func (state *State) FromProto(pb *tmstate.State) error {
	if pb == nil {
		return errors.New("nil State")
	}

	state.Version = pb.Version
	state.ChainID = pb.ChainID
	state.LastBlockHeight = pb.LastBlockHeight

	vals := new(types.ValidatorSet)
	if err := vals.FromProto(pb.Validators); err != nil {
		return err
	}
	nVals := new(types.ValidatorSet)
	if err := nVals.FromProto(pb.NextValidators); err != nil {
		return err
	}
	lVals := new(types.ValidatorSet)
	if err := lVals.FromProto(pb.LastValidators); err != nil {
		return err
	}

	state.Validators = vals
	state.NextValidators = nVals
	state.LastValidators = lVals

	state.LastHeightValidatorsChanged = pb.LastHeightValidatorsChanged
	state.ConsensusParams = pb.ConsensusParams
	state.LastHeightConsensusParamsChanged = pb.LastHeightConsensusParamsChanged
	state.LastResultsHash = pb.LastResultsHash
	state.AppHash = pb.AppHash

	return nil
}

//------------------------------------------------------------------------
// Create a block from the latest state

// MakeBlock builds a block from the current state with the given txs, commit,
// and evidence. Note it also takes a proposerAddress because the state does not
// track rounds, and hence does not know the correct proposer. TODO: fix this!
func (state State) MakeBlock(
	height int64,
	txs []types.Tx,
	commit *types.Commit,
	evidence []types.Evidence,
	proposerAddress []byte,
) (*types.Block, *types.PartSet) {

	// Build base block with block data.
	block := types.MakeBlock(height, txs, commit, evidence)

	// Set time.
	var timestamp time.Time
	if height == 1 {
		timestamp = state.LastBlockTime // genesis time
	} else {
		timestamp = MedianTime(commit, state.LastValidators)
	}

	// Fill rest of header with state data.
	block.Header.Populate(
		state.Version.Consensus, state.ChainID,
		timestamp, state.LastBlockID,
		state.Validators.Hash(), state.NextValidators.Hash(),
		types.HashConsensusParams(state.ConsensusParams), state.AppHash, state.LastResultsHash,
		proposerAddress,
	)

	return block, block.MakePartSet(types.BlockPartSizeBytes)
}

// MedianTime computes a median time for a given Commit (based on Timestamp field of votes messages) and the
// corresponding validator set. The computed time is always between timestamps of
// the votes sent by honest processes, i.e., a faulty processes can not arbitrarily increase or decrease the
// computed value.
func MedianTime(commit *types.Commit, validators *types.ValidatorSet) time.Time {
	weightedTimes := make([]*tmtime.WeightedTime, len(commit.Signatures))
	totalVotingPower := int64(0)

	for i, commitSig := range commit.Signatures {
		if commitSig.Absent() {
			continue
		}
		_, validator, ok := validators.GetByAddress(commitSig.ValidatorAddress)
		// If there's no condition, TestValidateBlockCommit panics; not needed normally.
		if ok {
			totalVotingPower += validator.VotingPower
			weightedTimes[i] = tmtime.NewWeightedTime(commitSig.Timestamp, validator.VotingPower)
		}
	}

	return tmtime.WeightedMedian(weightedTimes, totalVotingPower)
}

//------------------------------------------------------------------------
// Genesis

// MakeGenesisStateFromFile reads and unmarshals state from the given
// file.
//
// Used during replay and in tests.
func MakeGenesisStateFromFile(genDocFile string) (State, error) {
	genDoc, err := MakeGenesisDocFromFile(genDocFile)
	if err != nil {
		return State{}, err
	}
	return MakeGenesisState(genDoc)
}

// MakeGenesisDocFromFile reads and unmarshals genesis doc from the given file.
func MakeGenesisDocFromFile(genDocFile string) (*types.GenesisDoc, error) {
	genDocJSON, err := ioutil.ReadFile(genDocFile)
	if err != nil {
		return nil, fmt.Errorf("couldn't read GenesisDoc file: %v", err)
	}
	genDoc, err := types.GenesisDocFromJSON(genDocJSON)
	if err != nil {
		return nil, fmt.Errorf("error reading GenesisDoc: %v", err)
	}
	return genDoc, nil
}

// MakeGenesisState creates state from types.GenesisDoc.
func MakeGenesisState(genDoc *types.GenesisDoc) (State, error) {
	err := genDoc.ValidateAndComplete()
	if err != nil {
		return State{}, fmt.Errorf("error in genesis file: %v", err)
	}

	var validatorSet, nextValidatorSet *types.ValidatorSet
	if genDoc.Validators == nil {
		validatorSet = types.NewValidatorSet(nil)
		nextValidatorSet = types.NewValidatorSet(nil)
	} else {
		validators := make([]*types.Validator, len(genDoc.Validators))
		for i, val := range genDoc.Validators {
			validators[i] = types.NewValidator(val.PubKey, val.Power)
		}
		validatorSet = types.NewValidatorSet(validators)

		nextValidatorSet = types.NewValidatorSet(validators).CopyIncrementProposerPriority(1)
	}
	protoParam := tmproto.ConsensusParams{
		Block:     genDoc.ConsensusParams.Block,
		Evidence:  genDoc.ConsensusParams.Evidence,
		Validator: genDoc.ConsensusParams.Validator,
	}

	return State{
		Version: initStateVersion,
		ChainID: genDoc.ChainID,

		LastBlockHeight: 0,
		LastBlockID:     types.BlockID{},
		LastBlockTime:   genDoc.GenesisTime,

		NextValidators:              nextValidatorSet,
		Validators:                  validatorSet,
		LastValidators:              nil,
		LastHeightValidatorsChanged: 1,

		ConsensusParams:                  protoParam,
		LastHeightConsensusParamsChanged: 1,

		AppHash: genDoc.AppHash,
	}, nil
}
