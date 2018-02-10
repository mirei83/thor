package runtime

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common/math"
	"github.com/pkg/errors"
	cs "github.com/vechain/thor/contracts"
	"github.com/vechain/thor/state"
	"github.com/vechain/thor/thor"
	Tx "github.com/vechain/thor/tx"
	"github.com/vechain/thor/vm"
)

// Runtime is to support transaction execution.
type Runtime struct {
	vmConfig   vm.Config
	getBlockID func(uint32) thor.Hash
	state      *state.State

	// block env
	blockBeneficiary thor.Address
	blockNumber      uint32
	blockTime        uint64
	blockGasLimit    uint64
}

// New create a Runtime object.
func New(
	state *state.State,
	blockBeneficiary thor.Address,
	blockNumber uint32,
	blockTime,
	blockGasLimit uint64,
	getBlockID func(uint32) thor.Hash) *Runtime {
	return &Runtime{
		getBlockID:       getBlockID,
		state:            state,
		blockBeneficiary: blockBeneficiary,
		blockNumber:      blockNumber,
		blockTime:        blockTime,
		blockGasLimit:    blockGasLimit,
	}
}

func (rt *Runtime) State() *state.State            { return rt.state }
func (rt *Runtime) BlockBeneficiary() thor.Address { return rt.blockBeneficiary }
func (rt *Runtime) BlockNumber() uint32            { return rt.blockNumber }
func (rt *Runtime) BlockTime() uint64              { return rt.blockTime }
func (rt *Runtime) BlockGasLimit() uint64          { return rt.blockGasLimit }

// SetVMConfig config VM.
// Returns this runtime.
func (rt *Runtime) SetVMConfig(config vm.Config) *Runtime {
	rt.vmConfig = config
	return rt
}

func (rt *Runtime) execute(
	clause *Tx.Clause,
	index int,
	gas uint64,
	txOrigin thor.Address,
	txGasPrice *big.Int,
	txID thor.Hash,
	isStatic bool,
) *vm.Output {
	to := clause.To()
	if isStatic && to == nil {
		panic("static call requires 'To'")
	}
	ctx := vm.Context{
		Beneficiary: rt.blockBeneficiary,
		BlockNumber: new(big.Int).SetUint64(uint64(rt.blockNumber)),
		Time:        new(big.Int).SetUint64(rt.blockTime),
		GasLimit:    new(big.Int).SetUint64(rt.blockGasLimit),

		Origin:   txOrigin,
		GasPrice: txGasPrice,
		TxHash:   txID,

		GetHash:     rt.getBlockID,
		ClauseIndex: uint64(index),
	}

	env := vm.New(ctx, rt.state, rt.vmConfig)
	env.HookContract(cs.Authority.Address, func(input []byte) func(useGas func(gas uint64) bool, caller thor.Address) ([]byte, error) {
		return cs.Authority.HandleNative(rt.state, input)
	})

	env.HookContract(cs.Params.Address, func(input []byte) func(useGas func(gas uint64) bool, caller thor.Address) ([]byte, error) {
		return cs.Params.HandleNative(rt.state, input)
	})
	env.HookContract(cs.Energy.Address, func(input []byte) func(useGas func(gas uint64) bool, caller thor.Address) ([]byte, error) {
		return cs.Energy.HandleNative(rt.state, rt.blockTime, input)
	})

	if to == nil {
		return env.Create(txOrigin, clause.Data(), gas, clause.Value())
	}
	if isStatic {
		return env.StaticCall(txOrigin, *to, clause.Data(), gas)
	}
	return env.Call(txOrigin, *to, clause.Data(), gas, clause.Value())
}

// StaticCall executes signle clause which ensure no modifications to state.
func (rt *Runtime) StaticCall(
	clause *Tx.Clause,
	index int,
	gas uint64,
	txOrigin thor.Address,
	txGasPrice *big.Int,
	txID thor.Hash,
) *vm.Output {
	return rt.execute(clause, index, gas, txOrigin, txGasPrice, txID, true)
}

// Call executes single clause.
func (rt *Runtime) Call(
	clause *Tx.Clause,
	index int,
	gas uint64,
	txOrigin thor.Address,
	txGasPrice *big.Int,
	txID thor.Hash,
) *vm.Output {
	return rt.execute(clause, index, gas, txOrigin, txGasPrice, txID, false)
}

// ExecuteTransaction executes a transaction.
// Note that the elements of returned []*vm.Output may be nil if corresponded clause failed.
func (rt *Runtime) ExecuteTransaction(tx *Tx.Transaction) (receipt *Tx.Receipt, vmOutputs []*vm.Output, err error) {
	// precheck
	origin, err := tx.Signer()
	if err != nil {
		return nil, nil, err
	}
	intrinsicGas, err := tx.IntrinsicGas()
	if err != nil {
		return nil, nil, err
	}
	gas := tx.Gas()
	if intrinsicGas > gas {
		return nil, nil, errors.New("intrinsic gas exceeds provided gas")
	}

	gasPrice := tx.GasPrice()
	clauses := tx.Clauses()

	energyPrepayed := new(big.Int).SetUint64(gas)
	energyPrepayed.Mul(energyPrepayed, gasPrice)

	energyPayer, ok := cs.Energy.Consume(rt.state, rt.blockTime, origin, commonTo(clauses), energyPrepayed)
	if !ok {
		return nil, nil, errors.New("insufficient energy")
	}

	// checkpoint to be reverted when clause failure.
	clauseCheckpoint := rt.state.NewCheckpoint()

	leftOverGas := gas - intrinsicGas

	receipt = &Tx.Receipt{Outputs: make([]*Tx.Output, len(clauses))}
	vmOutputs = make([]*vm.Output, len(clauses))

	for i, clause := range clauses {
		vmOutput := rt.execute(clause, i, leftOverGas, origin, gasPrice, tx.ID(), false)
		vmOutputs[i] = vmOutput

		gasUsed := leftOverGas - vmOutput.LeftOverGas
		leftOverGas = vmOutput.LeftOverGas

		// Apply refund counter, capped to half of the used gas.
		halfUsed := new(big.Int).SetUint64(gasUsed / 2)
		refund := math.BigMin(vmOutput.RefundGas, halfUsed)

		// won't overflow
		leftOverGas += refund.Uint64()

		if vmOutput.VMErr != nil {
			// vm exception here
			// revert all executed clauses
			rt.state.RevertTo(clauseCheckpoint)
			receipt.Outputs = nil
			break
		}

		// transform vm output to clause output
		var logs []*Tx.Log
		for _, vmLog := range vmOutput.Logs {
			logs = append(logs, (*Tx.Log)(vmLog))
		}
		receipt.Outputs[i] = &Tx.Output{Logs: logs}
	}

	receipt.GasUsed = gas - leftOverGas

	// entergy to return = leftover gas * gas price
	energyToReturn := new(big.Int).SetUint64(leftOverGas)
	energyToReturn.Mul(energyToReturn, gasPrice)

	// return overpayed energy to payer
	payerBalance := cs.Energy.GetBalance(rt.state, rt.blockTime, energyPayer)
	cs.Energy.SetBalance(rt.state, rt.blockTime, energyPayer, payerBalance.Add(payerBalance, energyToReturn))

	return receipt, vmOutputs, nil
}

// returns common 'To' field of clauses if any.
// Empty address returned if no common 'To'.
func commonTo(clauses []*Tx.Clause) thor.Address {
	if len(clauses) == 0 {
		return thor.Address{}
	}

	firstTo := clauses[0].To()
	if firstTo == nil {
		return thor.Address{}
	}

	for _, clause := range clauses[1:] {
		to := clause.To()
		if to == nil {
			return thor.Address{}
		}
		if *to != *firstTo {
			return thor.Address{}
		}
	}
	return *firstTo
}
