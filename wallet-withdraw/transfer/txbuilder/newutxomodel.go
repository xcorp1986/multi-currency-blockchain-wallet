package txbuilder

import (
	"fmt"

	bmodels "upex-wallet/wallet-base/models"
	"upex-wallet/wallet-base/newbitx/misclib/log"
	"upex-wallet/wallet-config/withdraw/transfer/config"
	"upex-wallet/wallet-withdraw/base/models"
	"upex-wallet/wallet-withdraw/transfer/alarm"

	"github.com/shopspring/decimal"
)

var (
	feeFloatUp     = 0.10 // 交易费浮动百分比
	errEmptyInputs = fmt.Errorf("build extinfo failed, inputs is empty")
)

// BuildExtInfo def.
type BuildExtInfo struct {
	Inputs       []*TxIn
	TotalInput   decimal.Decimal
	MaxOutAmount decimal.Decimal
}

type utxoSelector func(acc *bmodels.Account, limitLen int) ([]*bmodels.UTXO, decimal.Decimal, bool, error)

func createBuildExtInfo(fromAccounts []*bmodels.Account, selectUTXO utxoSelector, maxTxInLen int, maxOutAmount decimal.Decimal) (*BuildExtInfo, error) {
	var (
		extInfo = &BuildExtInfo{
			MaxOutAmount: maxOutAmount, // 0
		}
		utxoLen int // 0
	)
	for _, acc := range fromAccounts {
		utxos, totalIn, ok, err := selectUTXO(acc, maxTxInLen-utxoLen)
		if err != nil {
			return nil, err
		}

		if !ok {
			continue
		}

		if acc.Balance.LessThan(totalIn) {
			return nil, fmt.Errorf("balance of %s mismatch to utxo, less", acc.Address)
		}
		extInfo.Inputs = append(extInfo.Inputs, &TxIn{
			Account:   acc,
			Cost:      totalIn,
			CostUTXOs: utxos,
		})
		extInfo.TotalInput = extInfo.TotalInput.Add(totalIn)
		utxoLen += len(utxos)
		if utxoLen >= maxTxInLen {
			break
		}
	}

	if len(extInfo.Inputs) == 0 {
		return nil, errEmptyInputs
	}

	return extInfo, nil
}

// UTXOModelTxBuilder def.
type UTXOModelTxBuilder interface {
	Support(string) bool
	DoBuild(*MetaData, *models.Tx, *BuildExtInfo) (*TxInfo, error)
}

type UTXOModelBuilder struct {
	cfg      *config.Config
	metaData *MetaData
	builder  UTXOModelTxBuilder
}

// NewUTXOModelBuilder factory func to instance a UTXO Builder
func NewUTXOModelBuilder(cfg *config.Config, builder UTXOModelTxBuilder) Builder {
	metaData, ok := FindMetaData(cfg.Currency)
	if !ok {
		panic(fmt.Errorf("can't get meta data of currency %s", cfg.Currency))
	}

	// maxFee := decimal.NewFromFloat(cfg.MaxFee)
	// don't need to maxFee 同builder
	// if maxFee.GreaterThan(metaData.Fee) {
	// 	metaData.Fee = maxFee
	// }

	return &UTXOModelBuilder{
		cfg:      cfg,
		metaData: metaData,
		builder:  builder,
	}
}

// BuildByMetaData build TxInfo by metaData, handle ErrFeeNotEnough.
func (b *UTXOModelBuilder) BuildByMetaData(doBuild func(*MetaData) (*TxInfo, error)) (*TxInfo, error) {
	txInfo, err := doBuild(b.metaData)

	if err != nil {
		if err, ok := err.(*ErrFeeNotEnough); ok {
			log.Warnf("%v, try to rebuild by new fee", err)
			feeFloatUp = b.cfg.FeeFloatUp
			return doBuild(b.metaData)
		}
		return nil, err
	}
	return txInfo, nil
}

// Model to instance a UTXO model
func (b *UTXOModelBuilder) Model() Model {
	return UTXOModel
}

// newCreateBuildExtInfo  build a BuildExtInfo instance, confirm tx inputs and outputs
func (b *UTXOModelBuilder) newCreateBuildExtInfo(fromAccounts []*bmodels.Account, task *models.Tx, metaData *MetaData, maxOutAmount decimal.Decimal) (*BuildExtInfo, error) {

	var (
		cost   = task.Amount
		txType = models.TxTypeName(task.TxType)
	)
	if cost.LessThanOrEqual(decimal.Zero) {
		return nil, fmt.Errorf("Cost is not  allowed,should be greater than zero")
	}

	cost = cost.Add(b.oneOutPutFee(txType))
	var (
		extInfo = &BuildExtInfo{ // instance a BuildExtInfo
			MaxOutAmount: maxOutAmount, // 0
		}
		utxoLen    int
		inPutNums  = 0 // init inPuts  number
		outPutNums = 1 // init OutPuts number
	)
	// loop all match account from model.Accounts
	for _, account := range fromAccounts {
		var (
			totalIn      decimal.Decimal
			AccountUTXOS = []*bmodels.UTXO{}
		)

		if cost.Equal(decimal.Zero) || cost.LessThan(b.oneOutPutFee(txType)) {
			break
		}
		// according the address in account table select lowest required utxos
		utxos, ok := models.SelectUTXOWithTransFee(account.Address, metaData.MaxTxInLen-utxoLen, true)
		if !ok {
			return nil, fmt.Errorf("Balance of %s mismatch to utxo, greater ", account.Address)
		}

		for _, u := range utxos {
			// only a just output || if change fee and meet one output transaction fee
			if cost.Equal(decimal.Zero) || cost.LessThanOrEqual(b.oneOutPutFee(txType)) {
				break
			}

			AccountUTXOS = append(AccountUTXOS, u) // update utxos
			cost = cost.Add(b.oneInPutFee(txType)) // add a input transaction fee
			inPutNums++                            // input number add one
			cost = cost.Sub(u.Amount)              // cost subtract current uxto amount
			totalIn = totalIn.Add(u.Amount)        // sum all utxo amount

			// If the cost is less than 0, it means that after consuming the current UTXO, there is a balance, and change is needed
			// outPut number at most 2
			if cost.LessThan(decimal.Zero) && outPutNums < 2 {
				outPutNums++                            // if need change, need add  one outputs
				cost = cost.Add(b.oneOutPutFee(txType)) // cost need add one outPut transaction fee
			}
		}

		// update inputs
		extInfo.Inputs = append(extInfo.Inputs, &TxIn{
			Account:   account,
			Cost:      totalIn,
			CostUTXOs: AccountUTXOS,
		})

		// calculate all of inputs of using utxos' amount
		extInfo.TotalInput = extInfo.TotalInput.Add(totalIn)

		// every account with utxos must less than MaxTxInLen--will case to many inputs
		utxoLen += len(AccountUTXOS)
		if utxoLen >= metaData.MaxTxInLen {
			break
		}
	}

	if len(extInfo.Inputs) == 0 {
		return nil, errEmptyInputs
	}

	// update metaFee
	err := b.updateMateFee(inPutNums, outPutNums, metaData, task)
	if err != nil {
		return nil, err
	}

	// check total cost is less than totalInput
	totalCost := task.Amount.Add(metaData.Fee)
	if extInfo.TotalInput.LessThan(totalCost) {
		return nil, alarm.NewErrorBalanceLessCost(metaData.Fee, extInfo.TotalInput)
	}

	return extInfo, nil
}

// updateMateFee ,according inputNums and outPutNums update metaData.Fee
func (b *UTXOModelBuilder) updateMateFee(inPutNums, outPutNums int, metaData *MetaData, task *models.Tx) (err error) {

	if inPutNums == 0 && outPutNums == 0 {
		return fmt.Errorf("Update tx Fee err, no inputs and outputs ")
	}
	withdrawFee := b.EstimateTransFee(inPutNums, outPutNums, models.TxTypeName(task.TxType))

	if withdrawFee.Equal(decimal.Zero) {
		err = fmt.Errorf("total transaction fee equal zero ")
	}
	log.Infof("The task of withdraw SequenceID:[%v],Amount:[%v],need use [%v] inputs and [%v]outputs, pay [%v] %v",
		task.SequenceID, task.Amount, inPutNums, outPutNums, withdrawFee, b.cfg.Currency)
	metaData.Fee = withdrawFee
	return
}

// EstimateTransFee according request api to calculate transfer fee.
func (b *UTXOModelBuilder) EstimateTransFee(inputNums, outPutNums int, txType string) (Fee decimal.Decimal) {
	transactionFee, _ := models.CalculateTransactionFee(txType, b.cfg)

	var (
		totalSize             = decimal.NewFromFloat(float64(inputNums*148 + outPutNums*43))
		floatUp               = decimal.NewFromFloat(1).Add(decimal.NewFromFloat(feeFloatUp))
		decimalTransactionFee = decimal.NewFromFloat(transactionFee)
		satToBTC              = decimal.NewFromFloat(0.00000001)
	)

	// sat cover to btc
	// decimalFee := float64(totalSize) * transactionFee * (1.0 + feeFloatUp) * 0.00000001
	decimalFee, _ := totalSize.Mul(decimalTransactionFee).Mul(floatUp).Mul(satToBTC).Float64()
	// precision is 6
	strFee := fmt.Sprintf("%.6f", decimalFee)
	Fee, _ = decimal.NewFromString(strFee)
	return
}

// oneOutPutFee
func (b *UTXOModelBuilder) oneOutPutFee(txType string) (oneOutPutFee decimal.Decimal) {
	oneOutPutFee = b.EstimateTransFee(0, 1, txType)
	return
}

// oneInPutFee, calculate one inputs transaction fee
func (b *UTXOModelBuilder) oneInPutFee(txType string) (oneInPutFee decimal.Decimal) {
	oneInPutFee = b.EstimateTransFee(1, 0, txType)
	return
}

// OneInOutPutFee one input and one out put fee
func (b *UTXOModelBuilder) OneInOutPutFee(txType string) (OneInOutPutFee decimal.Decimal) {
	OneInOutPutFee = b.EstimateTransFee(1, 1, txType)
	return
}

// Use suggestion transaction fee do build
func (b *UTXOModelBuilder) buildBySuggestTransactionFee(metaData *MetaData, task *models.Tx, MaxOutAmount decimal.Decimal) (*TxInfo, error) {

	txType := models.TxTypeName(task.TxType)
	filterFee := b.OneInOutPutFee(txType)
	if task.Amount.LessThanOrEqual(decimal.Zero) {
		return nil, fmt.Errorf("transfer amount must be greater than zero")
	}
	// filter Accounts
	fromAccounts := bmodels.GetAllMatchedAccounts(filterFee.String(), bmodels.AddressTypeSystem)
	// 1. sort Accounts according account.Balance
	fromAccounts, ok := models.SortAccountsByBalance(fromAccounts)

	if !ok {
		return nil, alarm.NewErrorBalanceLessThanFee(filterFee)
	}

	extInfo, err := b.newCreateBuildExtInfo(fromAccounts, task, metaData, MaxOutAmount)
	if err != nil {
		return nil, fmt.Errorf("create builder extInfo failed,%v", err)
	}

	return b.builder.DoBuild(metaData, task, extInfo)
}

// BuildWithdraw , use suggest transaction fee to calculate inputsNums and outPutNums
// Overwrite BuildWithdraw
func (b *UTXOModelBuilder) BuildWithdraw(task *models.Tx) (*TxInfo, error) {
	var (
		MaxOutAmount = decimal.Zero
	)

	txInfo, err := b.buildBySuggestTransactionFee(b.metaData, task, MaxOutAmount)
	if err != nil {
		errMsg := ""
		switch err.(type) {
		case *alarm.ErrorBalanceLessThanFee:
			errMsg = err.(*alarm.ErrorBalanceLessThanFee).ErrorDetail
		case *alarm.ErrorBalanceNotEnough:
			errMsg = err.(*alarm.ErrorBalanceNotEnough).ErrorDetail
		case *alarm.ErrorBalanceLessCost:
			errMsg = err.(*alarm.ErrorBalanceLessCost).ErrorDetail
		case *ErrFeeNotEnough:
			log.Warnf("%v, try to rebuild by new fee", err)
			// modify transaction fee according to configuration
			feeFloatUp = b.cfg.FeeFloatUp
			return b.buildBySuggestTransactionFee(b.metaData, task, MaxOutAmount)
		}

		if errMsg != "" {
			go alarm.SendEmail(b.cfg, task, err, errMsg)
		}

		return nil, err
	}
	return txInfo, nil
}

// new BuildGather
func (b *UTXOModelBuilder) BuildGather(task *models.Tx) (*TxInfo, error) {

	var (
		txType            = models.TxTypeName(task.TxType)
		maxWithdrawAmount = decimal.NewFromFloat(b.cfg.MaxWithdrawAmount)
	)

	maxOutAmount := maxWithdrawAmount.Mul(decimal.NewFromFloat(0.05))

	buildExt := func(metaData *MetaData) (*BuildExtInfo, error) {
		// Build from normal address.
		filterFee := b.OneInOutPutFee(txType)
		fromAccounts := bmodels.GetAllMatchedAccounts(filterFee.String(), bmodels.AddressTypeNormal)
		if len(fromAccounts) > 0 {
			return createBuildExtInfo(
				fromAccounts,
				func(acc *bmodels.Account, limitLen int) ([]*bmodels.UTXO, decimal.Decimal, bool, error) {
					utxos, totalIn, ok := models.SelectUTXO(acc.Address, decimal.Zero, limitLen)
					return utxos, totalIn, ok, nil
				},
				metaData.MaxTxInLen,
				maxOutAmount)
		}

		// Build from system address.
		fromAccounts = bmodels.GetAllMatchedAccounts(filterFee.String(), bmodels.AddressTypeSystem)
		if len(fromAccounts) > 0 {
			return createBuildExtInfo(
				fromAccounts,
				func(acc *bmodels.Account, limitLen int) ([]*bmodels.UTXO, decimal.Decimal, bool, error) {
					maxSmallUTXOAmount := maxOutAmount.Mul(decimal.NewFromFloat(0.7))
					utxos, totalIn, ok := models.SelectSmallUTXO(acc.Address, maxSmallUTXOAmount, limitLen)
					return utxos, totalIn, ok, nil
				},
				metaData.MaxTxInLen,
				maxOutAmount)
		}
		return nil, nil
	}

	txInfo, err := b.BuildByMetaData(
		func(metaData *MetaData) (*TxInfo, error) {

			extInfo, err := buildExt(b.metaData)
			if err != nil {
				if err == errEmptyInputs {
					return nil, nil
				}
				return nil, err
			}

			if extInfo == nil {
				return nil, nil
			}

			inPutNums := len(extInfo.Inputs)
			outPutNums := 1

			// update metaFee
			err = b.updateMateFee(inPutNums, outPutNums, metaData, task)
			if err != nil {
				return nil, fmt.Errorf("build by MetaData for %s fail, %v", txType, err)
			}

			task.Amount = extInfo.TotalInput.Sub(metaData.Fee)

			if task.Amount.LessThan(decimal.Zero) {
				return nil, alarm.NewErrorBalanceLessCost(metaData.Fee, extInfo.TotalInput)
			}
			return b.builder.DoBuild(metaData, task, extInfo)
		})

	if err != nil {
		switch err.(type) {
		case *alarm.ErrorBalanceLessCost:
			errMsg := err.(*alarm.ErrorBalanceLessCost).ErrorDetail
			go alarm.SendEmail(b.cfg, task, err, errMsg)
		}
	}

	return txInfo, err
}

// OutputsAdder def.
type OutputsAdder func(string, uint64)

// MakeOutputs totalIn = extInfo.TotalInput
func MakeOutputs(
	totalIn, mainOut, maxOutAmount decimal.Decimal,
	outAddress, changeAddress string,
	metaData *MetaData,
	addOutput OutputsAdder) {

	if mainOut.GreaterThan(decimal.Zero) {
		if maxOutAmount.GreaterThan(decimal.Zero) {
			amount := mainOut
			v := maxOutAmount.Mul(decimal.New(1, int32(metaData.Precision))).IntPart()
			for amount.GreaterThan(maxOutAmount) {
				addOutput(outAddress, uint64(v))
				amount = amount.Sub(maxOutAmount)
			}
			if amount.GreaterThan(decimal.Zero) {
				v := amount.Mul(decimal.New(1, int32(metaData.Precision))).IntPart()
				addOutput(outAddress, uint64(v))
			}
		} else {
			v := mainOut.Mul(decimal.New(1, int32(metaData.Precision))).IntPart()
			addOutput(outAddress, uint64(v))
		}
	}

	cost := mainOut.Add(metaData.Fee)
	if totalIn.GreaterThan(cost) {
		changeValue := totalIn.Sub(cost).Mul(decimal.New(1, int32(metaData.Precision))).IntPart()
		addOutput(changeAddress, uint64(changeValue))
	}
}
