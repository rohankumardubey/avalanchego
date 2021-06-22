// (c) 2021, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package proposer

import (
	"sort"
	"time"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/validators"
	"github.com/ava-labs/avalanchego/utils/math"
	"github.com/ava-labs/avalanchego/utils/sampler"
)

const (
	maxWindows     = 5
	windowDuration = 3 * time.Second
)

var _ Windower = &windower{}

type Windower interface {
	Delay(
		chainHeight,
		pChainHeight uint64,
		validatorID ids.ShortID,
	) (time.Duration, error)
}

// windower interfaces with P-Chain and it is responsible for calculating the
// delay for the block submission window of a given validator
type windower struct {
	vm       validators.VM
	subnetID ids.ID
	sampler  sampler.WeightedWithoutReplacement
}

func New(vm validators.VM, subnetID ids.ID) Windower {
	return &windower{
		vm:       vm,
		subnetID: subnetID,
		sampler:  sampler.NewDeterministicWeightedWithoutReplacement(),
	}
}

func (w *windower) Delay(chainHeight, pChainHeight uint64, validatorID ids.ShortID) (time.Duration, error) {
	// get the validator set by the p-chain height
	validatorsMap, err := w.vm.GetValidatorSet(pChainHeight, w.subnetID)
	if err != nil {
		return 0, err
	}

	// convert the map of validators to a slice
	validators := make(validatorsSlice, 0, len(validatorsMap))
	weight := uint64(0)
	for k, v := range validatorsMap {
		validators = append(validators, validatorData{
			id:     k,
			weight: v,
		})
		newWeight, err := math.Add64(weight, v)
		if err != nil {
			return 0, err
		}
		weight = newWeight
	}

	// canonically sort validators
	// Note: validators are sorted by ID, sorting by weight would not create a
	// canonically sorted list
	sort.Sort(validators)

	// convert the slice of validators to a slice of weights
	validatorWeights := make([]uint64, len(validators))
	for i, v := range validators {
		validatorWeights[i] = v.weight
	}

	if err := w.sampler.Initialize(validatorWeights); err != nil {
		return 0, err
	}

	numToSample := maxWindows
	if uint64(maxWindows) < weight {
		numToSample = int(weight)
	}

	w.sampler.Seed(int64(chainHeight))

	indices, err := w.sampler.Sample(numToSample)
	if err != nil {
		return 0, err
	}

	delay := time.Duration(0)
	for _, index := range indices {
		nodeID := validators[index].id
		if nodeID == validatorID {
			return delay, nil
		}
		delay += windowDuration
	}
	return delay, nil
}