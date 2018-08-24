package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/bytom/account"
	"github.com/bytom/blockchain/txbuilder"
	"github.com/bytom/consensus"
	"github.com/bytom/consensus/segwit"
	"github.com/bytom/crypto/sha3pool"
	"github.com/bytom/errors"
	"github.com/bytom/math/checked"
	"github.com/bytom/net/http/reqid"
	"github.com/bytom/protocol/bc"
	"github.com/bytom/protocol/bc/types"
)

var (
	defaultTxTTL    = 5 * time.Minute
	defaultBaseRate = float64(100000)
)

func (a *API) actionDecoder(action string) (func([]byte) (txbuilder.Action, error), bool) {
	decoders := map[string]func([]byte) (txbuilder.Action, error){
		"control_address":              txbuilder.DecodeControlAddressAction,
		"control_program":              txbuilder.DecodeControlProgramAction,
		"issue":                        a.wallet.AssetReg.DecodeIssueAction,
		"retire":                       txbuilder.DecodeRetireAction,
		"spend_account":                a.wallet.AccountMgr.DecodeSpendAction,
		"spend_account_unspent_output": a.wallet.AccountMgr.DecodeSpendUTXOAction,
	}
	decoder, ok := decoders[action]
	return decoder, ok
}

func onlyHaveInputActions(req *BuildRequest) (bool, error) {
	count := 0
	for i, act := range req.Actions {
		actionType, ok := act["type"].(string)
		if !ok {
			return false, errors.WithDetailf(ErrBadActionType, "no action type provided on action %d", i)
		}

		if strings.HasPrefix(actionType, "spend") || actionType == "issue" {
			count++
		}
	}

	return count == len(req.Actions), nil
}

func (a *API) buildSingle(ctx context.Context, req *BuildRequest) (*txbuilder.Template, error) {
	if err := a.completeMissingIDs(ctx, req); err != nil {
		return nil, err
	}

	if ok, err := onlyHaveInputActions(req); err != nil {
		return nil, err
	} else if ok {
		return nil, errors.WithDetail(ErrBadActionConstruction, "transaction contains only input actions and no output actions")
	}

	actions := make([]txbuilder.Action, 0, len(req.Actions))
	for i, act := range req.Actions {
		typ, ok := act["type"].(string)
		if !ok {
			return nil, errors.WithDetailf(ErrBadActionType, "no action type provided on action %d", i)
		}
		decoder, ok := a.actionDecoder(typ)
		if !ok {
			return nil, errors.WithDetailf(ErrBadActionType, "unknown action type %q on action %d", typ, i)
		}

		// Remarshal to JSON, the action may have been modified when we
		// filtered aliases.
		b, err := json.Marshal(act)
		if err != nil {
			return nil, err
		}
		action, err := decoder(b)
		if err != nil {
			return nil, errors.WithDetailf(ErrBadAction, "%s on action %d", err.Error(), i)
		}
		actions = append(actions, action)
	}
	actions = account.MergeSpendAction(actions)

	ttl := req.TTL.Duration
	if ttl == 0 {
		ttl = defaultTxTTL
	}
	maxTime := time.Now().Add(ttl)

	tpl, err := txbuilder.Build(ctx, req.Tx, actions, maxTime, req.TimeRange)
	if errors.Root(err) == txbuilder.ErrAction {
		// append each of the inner errors contained in the data.
		var Errs string
		var rootErr error
		for i, innerErr := range errors.Data(err)["actions"].([]error) {
			if i == 0 {
				rootErr = errors.Root(innerErr)
			}
			Errs = Errs + innerErr.Error()
		}
		err = errors.WithDetail(rootErr, Errs)
	}
	if err != nil {
		return nil, err
	}

	// ensure null is never returned for signing instructions
	if tpl.SigningInstructions == nil {
		tpl.SigningInstructions = []*txbuilder.SigningInstruction{}
	}
	return tpl, nil
}

// POST /build-transaction
func (a *API) build(ctx context.Context, buildReqs *BuildRequest) Response {
	subctx := reqid.NewSubContext(ctx, reqid.New())

	tmpl, err := a.buildSingle(subctx, buildReqs)
	if err != nil {
		return NewErrorResponse(err)
	}

	return NewSuccessResponse(tmpl)
}

type submitTxResp struct {
	TxID *bc.Hash `json:"tx_id"`
}

// POST /submit-transaction
func (a *API) submit(ctx context.Context, ins struct {
	Tx types.Tx `json:"raw_transaction"`
}) Response {
	if err := txbuilder.FinalizeTx(ctx, a.chain, &ins.Tx); err != nil {
		return NewErrorResponse(err)
	}

	log.WithField("tx_id", ins.Tx.ID.String()).Info("submit single tx")
	return NewSuccessResponse(&submitTxResp{TxID: &ins.Tx.ID})
}

// EstimateTxGasResp estimate transaction consumed gas
type EstimateTxGasResp struct {
	TotalNeu   int64 `json:"total_neu"`
	StorageNeu int64 `json:"storage_neu"`
	VMNeu      int64 `json:"vm_neu"`
}

// EstimateTxGas estimate consumed neu for transaction
func EstimateTxGas(template txbuilder.Template) (*EstimateTxGasResp, error) {
	// base tx size and not include sign
	data, err := template.Transaction.TxData.MarshalText()
	if err != nil {
		return nil, err
	}
	baseTxSize := int64(len(data))

	// extra tx size for sign witness parts
	signSize := estimateSignSize(template.SigningInstructions)

	// total gas for tx storage
	totalTxSizeGas, ok := checked.MulInt64(baseTxSize+signSize, consensus.StorageGasRate)
	if !ok {
		return nil, errors.New("calculate txsize gas got a math error")
	}

	// consume gas for run VM
	totalP2WPKHGas := int64(0)
	totalP2WSHGas := int64(0)
	baseP2WPKHGas := int64(1419)

	for pos, inpID := range template.Transaction.Tx.InputIDs {
		sp, err := template.Transaction.Spend(inpID)
		if err != nil {
			continue
		}

		resOut, err := template.Transaction.Output(*sp.SpentOutputId)
		if err != nil {
			continue
		}

		if segwit.IsP2WPKHScript(resOut.ControlProgram.Code) {
			totalP2WPKHGas += baseP2WPKHGas
		} else if segwit.IsP2WSHScript(resOut.ControlProgram.Code) {
			sigInst := template.SigningInstructions[pos]
			totalP2WSHGas += estimateP2WSHGas(sigInst)
		}
	}

	// total estimate gas
	totalGas := totalTxSizeGas + totalP2WPKHGas + totalP2WSHGas

	// rounding totalNeu with base rate 100000
	totalNeu := float64(totalGas*consensus.VMGasRate) / defaultBaseRate
	roundingNeu := math.Ceil(totalNeu)
	estimateNeu := int64(roundingNeu) * int64(defaultBaseRate)

	// TODO add priority

	return &EstimateTxGasResp{
		TotalNeu:   estimateNeu,
		StorageNeu: totalTxSizeGas * consensus.VMGasRate,
		VMNeu:      (totalP2WPKHGas + totalP2WSHGas) * consensus.VMGasRate,
	}, nil
}

// estimate p2wsh gas.
// OP_CHECKMULTISIG consume (984 * a - 72 * b - 63) gas,
// where a represent the num of public keys, and b represent the num of quorum.
func estimateP2WSHGas(sigInst *txbuilder.SigningInstruction) int64 {
	P2WSHGas := int64(0)
	baseP2WSHGas := int64(738)

	for _, witness := range sigInst.WitnessComponents {
		switch t := witness.(type) {
		case *txbuilder.SignatureWitness:
			P2WSHGas += baseP2WSHGas + (984*int64(len(t.Keys)) - 72*int64(t.Quorum) - 63)
		case *txbuilder.RawTxSigWitness:
			P2WSHGas += baseP2WSHGas + (984*int64(len(t.Keys)) - 72*int64(t.Quorum) - 63)
		}
	}
	return P2WSHGas
}

// estimate signature part size.
// if need multi-sign, calculate the size according to the length of keys.
func estimateSignSize(signingInstructions []*txbuilder.SigningInstruction) int64 {
	signSize := int64(0)
	baseWitnessSize := int64(300)

	for _, sigInst := range signingInstructions {
		for _, witness := range sigInst.WitnessComponents {
			switch t := witness.(type) {
			case *txbuilder.SignatureWitness:
				signSize += int64(t.Quorum) * baseWitnessSize
			case *txbuilder.RawTxSigWitness:
				signSize += int64(t.Quorum) * baseWitnessSize
			}
		}
	}
	return signSize
}

// POST /estimate-transaction-gas
func (a *API) estimateTxGas(ctx context.Context, in struct {
	TxTemplate txbuilder.Template `json:"transaction_template"`
}) Response {
	txGasResp, err := EstimateTxGas(in.TxTemplate)
	if err != nil {
		return NewErrorResponse(err)
	}
	return NewSuccessResponse(txGasResp)
}

func getPeginTxnOutputIndex(rawTx types.Tx, controlProg []byte) int {
	for index, output := range rawTx.Outputs {
		if bytes.Equal(output.ControlProgram, controlProg) {
			return index
		}
	}
	return 0
}

func (a *API) claimPeginTx(ctx context.Context, ins struct {
	Password    string   `json:"password"`
	RawTx       types.Tx `json:"raw_transaction"`
	TxOutProof  string   `json:"tx_out_proof"`
	ClaimScript string   `json:"claim_script"`
}) Response {
	// raw transaction
	// proof验证
	// 增加spv验证以及连接主链api查询交易的确认数

	// 找出与claim script有关联的交易的输出
	var address string
	var controlProg []byte
	nOut := len(ins.RawTx.Outputs)
	if ins.ClaimScript == "" {
		// 遍历寻找与交易输出有关的claim script
		cps, err := a.wallet.AccountMgr.ListControlProgram()
		if err != nil {
			return NewErrorResponse(err)
		}

		for _, cp := range cps {
			address, controlProg = a.wallet.AccountMgr.GetPeginControlPrograms(cp.ControlProgram)
			if controlProg == nil {
				continue
			}
			// 获取交易的输出
			nOut = getPeginTxnOutputIndex(ins.RawTx, controlProg)
		}
	} else {
		address, controlProg = a.wallet.AccountMgr.GetPeginControlPrograms([]byte(ins.ClaimScript))
		// 获取交易的输出
		nOut = getPeginTxnOutputIndex(ins.RawTx, controlProg)
	}
	if nOut == len(ins.RawTx.Outputs) {
		return NewErrorResponse(errors.New("Failed to find output in bytom to the mainchain_address from getpeginaddress"))
	}
	fmt.Println(address, controlProg, nOut)

	// 根据ClaimScript 获取account id

	var hash [32]byte
	sha3pool.Sum256(hash[:], []byte(ins.ClaimScript))
	data := a.wallet.DB.Get(account.ContractKey(hash))
	if data == nil {
		return NewErrorResponse(errors.New("Failed to find control program through claim script"))
	}

	cp := &account.CtrlProgram{}
	if err := json.Unmarshal(data, cp); err != nil {
		return NewErrorResponse(errors.New("Failed on unmarshal control program"))
	}
	// 构造交易
	// 用输出作为交易输入 生成新的交易
	buildReqs := &BuildRequest{}
	act := make(map[string]interface{})
	// gas
	act["type"] = "spend_account"
	act["asset_id"] = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	act["amount"] = 0
	act["account_id"] = cp.AccountID
	buildReqs.Actions = append(buildReqs.Actions, act)
	// 输入
	act["amount"] = ins.RawTx.Outputs[nOut].Amount
	buildReqs.Actions = append(buildReqs.Actions, act)
	// 输出
	act["type"] = "control_address"
	act["amount"] = ins.RawTx.Outputs[nOut].Amount
	program, err := a.wallet.AccountMgr.CreateAddress(cp.AccountID, false)
	if err != nil {
		return NewErrorResponse(err)
	}
	act["address"] = program.Address
	delete(act, "account_id")
	buildReqs.Actions = append(buildReqs.Actions, act)
	tmpl, err := a.buildSingle(ctx, buildReqs)
	if err != nil {
		return NewErrorResponse(err)
	}
	// todo把一些主链的信息加到交易的stack中
	var stack [][]byte

	//amount
	amount := strconv.FormatUint(ins.RawTx.Outputs[nOut].Amount, 10)
	stack = append(stack, []byte(amount))
	// 主链的gennesisBlockHash
	stack = append(stack, []byte(consensus.ActiveNetParams.ParentGenesisBlockHash))
	// claim script
	stack = append(stack, []byte(ins.ClaimScript))
	// raw tx
	tx, _ := json.Marshal(ins.RawTx)
	stack = append(stack, tx)
	// proof
	stack = append(stack, []byte(ins.TxOutProof))

	tmpl.Transaction.Inputs[0].Peginwitness = stack

	tmpl.Transaction.Inputs[0].IsPegin = true

	// 交易签名
	if err := txbuilder.Sign(ctx, tmpl, ins.Password, a.PseudohsmSignTemplate); err != nil {
		log.WithField("build err", err).Error("fail on sign transaction.")
		return NewErrorResponse(err)
	}

	// submit
	if err := txbuilder.FinalizeTx(ctx, a.chain, &ins.RawTx); err != nil {
		return NewErrorResponse(err)
	}

	log.WithField("tx_id", ins.RawTx.ID.String()).Info("claim script tx")
	return NewSuccessResponse(&submitTxResp{TxID: &ins.RawTx.ID})
}
