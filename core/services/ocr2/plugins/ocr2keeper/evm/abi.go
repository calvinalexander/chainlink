package evm

import (
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/pkg/errors"
	"github.com/smartcontractkit/ocr2keepers/pkg/types"
)

type evmRegistryPackerV2_0 struct {
	abi abi.ABI
}

func (rp *evmRegistryPackerV2_0) UnpackCheckResult(key types.UpkeepKey, raw string) (types.UpkeepResult, error) {
	out, err := rp.abi.Methods["checkUpkeep"].Outputs.UnpackValues(hexutil.MustDecode(raw))
	if err != nil {
		return types.UpkeepResult{}, errors.Wrapf(err, "unpack checkUpkeep return: %s", raw)
	}

	result := types.UpkeepResult{
		Key:   key,
		State: types.Eligible,
	}

	upkeepNeeded := *abi.ConvertType(out[0], new(bool)).(*bool)
	rawPerformData := *abi.ConvertType(out[1], new([]byte)).(*[]byte)
	result.FailureReason = *abi.ConvertType(out[2], new(uint8)).(*uint8)
	result.GasUsed = *abi.ConvertType(out[3], new(*big.Int)).(**big.Int)
	result.FastGasWei = *abi.ConvertType(out[4], new(*big.Int)).(**big.Int)
	result.LinkNative = *abi.ConvertType(out[5], new(*big.Int)).(**big.Int)

	// TODO: not sure it it's best to short circuit here
	if !upkeepNeeded {
		result.State = types.NotEligible
	} else {
		var ret0 = new(res)
		err = pdataABI.UnpackIntoInterface(ret0, "check", rawPerformData)
		if err != nil {
			return types.UpkeepResult{}, err
		}

		result.CheckBlockNumber = ret0.Result.CheckBlockNumber
		result.CheckBlockHash = ret0.Result.CheckBlockhash
		result.PerformData = ret0.Result.PerformData
	}

	return result, nil
}

func (rp *evmRegistryPackerV2_0) UnpackPerformResult(raw string) (bool, error) {
	out, err := rp.abi.Methods["simulatePerformUpkeep"].
		Outputs.UnpackValues(hexutil.MustDecode(raw))
	if err != nil {
		return false, errors.Wrapf(err, "unpack simulatePerformUpkeep return: %s", raw)
	}

	return *abi.ConvertType(out[0], new(bool)).(*bool), nil
}

var (
	// rawPerformData is abi encoded tuple(uint32, bytes32, bytes). We create an ABI with dummy
	// function which returns this tuple in order to decode the bytes
	pdataABI, _ = abi.JSON(strings.NewReader(`[{
		"name":"check",
		"type":"function",
		"outputs":[{
			"name":"ret",
			"type":"tuple",
			"components":[
				{"type":"uint32","name":"checkBlockNumber"},
				{"type":"bytes32","name":"checkBlockhash"},
				{"type":"bytes","name":"performData"}
				]
			}]
		}]`,
	))
)

type res struct {
	Result performDataStruct
}
type performDataStruct struct {
	CheckBlockNumber uint32   `abi:"checkBlockNumber"`
	CheckBlockhash   [32]byte `abi:"checkBlockhash"`
	PerformData      []byte   `abi:"performData"`
}
