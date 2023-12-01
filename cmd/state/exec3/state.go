package exec3

import (
	"context"
	"fmt"
	"github.com/ledgerwatch/erigon-lib/common/dbg"
	"math/big"
	"sync"
	"sync/atomic"

	"github.com/ledgerwatch/log/v3"
	"golang.org/x/sync/errgroup"

	"github.com/ledgerwatch/erigon/core/rawdb"
	"github.com/ledgerwatch/erigon/eth/consensuschain"
	"github.com/ledgerwatch/erigon/rlp"

	"github.com/ledgerwatch/erigon-lib/common/datadir"

	"github.com/ledgerwatch/erigon-lib/chain"
	libcommon "github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/kv"

	"github.com/ledgerwatch/erigon/consensus"
	"github.com/ledgerwatch/erigon/core"
	"github.com/ledgerwatch/erigon/core/state"
	"github.com/ledgerwatch/erigon/core/types"
	"github.com/ledgerwatch/erigon/core/vm"
	"github.com/ledgerwatch/erigon/core/vm/evmtypes"
	"github.com/ledgerwatch/erigon/turbo/services"
)

var (
	ibsTrace = dbg.EnvBool("IBS_TRACE", false)
)

type Worker struct {
	lock        sync.Locker
	logger      log.Logger
	chainDb     kv.RoDB
	chainTx     kv.Tx
	background  bool // if true - worker does manage RoTx (begin/rollback) in .ResetTx()
	blockReader services.FullBlockReader
	in          *state.QueueWithRetry
	rs          *state.StateV3
	stateWriter *state.StateWriterBufferedV3
	stateReader state.ResettableStateReader
	historyMode atomic.Bool // if true - stateReader is HistoryReaderV3, otherwise it's state reader
	chainConfig *chain.Config
	getHeader   func(hash libcommon.Hash, number uint64) *types.Header

	ctx      context.Context
	engine   consensus.Engine
	genesis  *types.Genesis
	resultCh *state.ResultsQueue
	chain    consensus.ChainReader

	callTracer  *CallTracer
	taskGasPool *core.GasPool

	evm   *vm.EVM
	ibs   *state.IntraBlockState
	vmCfg vm.Config

	dirs datadir.Dirs
}

func NewWorker(lock sync.Locker, logger log.Logger, ctx context.Context, background bool, chainDb kv.RoDB, rs *state.StateV3, in *state.QueueWithRetry, blockReader services.FullBlockReader, chainConfig *chain.Config, genesis *types.Genesis, results *state.ResultsQueue, engine consensus.Engine, dirs datadir.Dirs) *Worker {
	w := &Worker{
		lock:        lock,
		logger:      logger,
		chainDb:     chainDb,
		in:          in,
		rs:          rs,
		background:  background,
		blockReader: blockReader,
		stateWriter: state.NewStateWriterBufferedV3(rs),
		stateReader: state.NewStateReaderV3(rs),
		chainConfig: chainConfig,

		ctx:         ctx,
		genesis:     genesis,
		resultCh:    results,
		engine:      engine,
		historyMode: atomic.Bool{},

		evm:         vm.NewEVM(evmtypes.BlockContext{}, evmtypes.TxContext{}, nil, chainConfig, vm.Config{}),
		callTracer:  NewCallTracer(),
		taskGasPool: new(core.GasPool),

		dirs: dirs,
	}

	w.vmCfg = vm.Config{Debug: true, Tracer: w.callTracer}
	w.getHeader = func(hash libcommon.Hash, number uint64) *types.Header {
		h, err := blockReader.Header(ctx, w.chainTx, hash, number)
		if err != nil {
			panic(err)
		}
		return h
	}

	w.ibs = state.New(w.stateReader)

	return w
}

func (rw *Worker) ResetState(rs *state.StateV3) {
	rw.rs = rs
	rw.SetReader(state.NewStateReaderV3(rs))
	rw.stateWriter = state.NewStateWriterBufferedV3(rs)
}

func (rw *Worker) Tx() kv.Tx        { return rw.chainTx }
func (rw *Worker) DiscardReadList() { rw.stateReader.DiscardReadList() }
func (rw *Worker) ResetTx(chainTx kv.Tx) {
	if rw.background && rw.chainTx != nil {
		rw.chainTx.Rollback()
		rw.chainTx = nil
	}
	if chainTx != nil {
		rw.chainTx = chainTx
		rw.stateReader.SetTx(rw.chainTx)
		rw.stateWriter.SetTx(rw.chainTx)
		rw.chain = consensuschain.NewReader(rw.chainConfig, rw.chainTx, rw.blockReader, rw.logger)
	}
}

func (rw *Worker) Run() error {
	for txTask, ok := rw.in.Next(rw.ctx); ok; txTask, ok = rw.in.Next(rw.ctx) {
		rw.RunTxTask(txTask)
		if err := rw.resultCh.Add(rw.ctx, txTask); err != nil {
			return err
		}
	}
	return nil
}

func (rw *Worker) RunTxTask(txTask *state.TxTask) {
	rw.lock.Lock()
	defer rw.lock.Unlock()
	rw.RunTxTaskNoLock(txTask)
}

// Needed to set hisotry reader when need to offset few txs from block beginning and does not break processing,
// like compute gas used for block and then to set state reader to continue processing on latest data.
func (rw *Worker) SetReader(reader state.ResettableStateReader) {
	rw.stateReader = reader
	rw.stateReader.SetTx(rw.Tx())
	rw.ibs.Reset()
	rw.ibs = state.New(rw.stateReader)

	switch reader.(type) {
	case *state.HistoryReaderV3:
		rw.historyMode.Store(true)
	case *state.StateReaderV3:
		rw.historyMode.Store(false)
	default:
		rw.historyMode.Store(false)
		//fmt.Printf("[worker] unknown reader %T: historyMode is set to disabled\n", reader)
	}
}

func (rw *Worker) RunTxTaskNoLock(txTask *state.TxTask) {
	if txTask.HistoryExecution && !rw.historyMode.Load() {
		// in case if we cancelled execution and commitment happened in the middle of the block, we have to process block
		// from the beginning until committed txNum and only then disable history mode.
		// Needed to correctly evaluate spent gas and other things.
		rw.SetReader(state.NewHistoryReaderV3())
	} else if !txTask.HistoryExecution && rw.historyMode.Load() {
		rw.SetReader(state.NewStateReaderV3(rw.rs))
	}

	if rw.background && rw.chainTx == nil {
		var err error
		if rw.chainTx, err = rw.chainDb.BeginRo(rw.ctx); err != nil {
			panic(err)
		}
		rw.stateReader.SetTx(rw.chainTx)
		rw.stateWriter.SetTx(rw.chainTx)
		rw.chain = consensuschain.NewReader(rw.chainConfig, rw.chainTx, rw.blockReader, rw.logger)
	}
	txTask.Error = nil

	rw.stateReader.SetTxNum(txTask.TxNum)
	rw.stateWriter.SetTxNum(rw.ctx, txTask.TxNum)
	rw.stateReader.ResetReadSet()
	rw.stateWriter.ResetWriteSet()

	rw.ibs.Reset()
	ibs := rw.ibs
	ibs.SetTrace(ibsTrace)

	rules := txTask.Rules
	var err error
	header := txTask.Header
	//fmt.Printf("txNum=%d blockNum=%d history=%t\n", txTask.TxNum, txTask.BlockNum, txTask.HistoryExecution)

	switch {
	case txTask.TxIndex == -1:
		if txTask.BlockNum == 0 {
			// Genesis block
			//fmt.Printf("txNum=%d, blockNum=%d, Genesis\n", txTask.TxNum, txTask.BlockNum)
			_, ibs, err = core.GenesisToBlock(rw.genesis, rw.dirs.Tmp)
			if err != nil {
				panic(err)
			}
			// For Genesis, rules should be empty, so that empty accounts can be included
			rules = &chain.Rules{}
			break
		}
		// Block initialisation
		//fmt.Printf("txNum=%d, blockNum=%d, initialisation of the block\n", txTask.TxNum, txTask.BlockNum)
		syscall := func(contract libcommon.Address, data []byte, ibs *state.IntraBlockState, header *types.Header, constCall bool) ([]byte, error) {
			return core.SysCallContract(contract, data, rw.chainConfig, ibs, header, rw.engine, constCall /* constCall */)
		}
		rw.engine.Initialize(rw.chainConfig, rw.chain, header, ibs, syscall, rw.logger)
		txTask.Error = ibs.FinalizeTx(rules, noop)
	case txTask.Final:
		if txTask.BlockNum == 0 {
			break
		}

		//fmt.Printf("txNum=%d, blockNum=%d, finalisation of the block\n", txTask.TxNum, txTask.BlockNum)
		// End of block transaction in a block
		syscall := func(contract libcommon.Address, data []byte) ([]byte, error) {
			return core.SysCallContract(contract, data, rw.chainConfig, ibs, header, rw.engine, false /* constCall */)
		}

		_, _, err := rw.engine.Finalize(rw.chainConfig, types.CopyHeader(header), ibs, txTask.Txs, txTask.Uncles, nil, txTask.Withdrawals, rw.chain, syscall, rw.logger)
		if err != nil {
			txTask.Error = err
		} else {
			//incorrect unwind to block 2
			//if err := ibs.CommitBlock(rules, rw.stateWriter); err != nil {
			//	txTask.Error = err
			//}
			txTask.TraceTos = map[libcommon.Address]struct{}{}
			txTask.TraceTos[txTask.Coinbase] = struct{}{}
			for _, uncle := range txTask.Uncles {
				txTask.TraceTos[uncle.Coinbase] = struct{}{}
			}
		}
	default:
		txHash := txTask.Tx.Hash()
		rw.taskGasPool.Reset(txTask.Tx.GetGas())
		rw.callTracer.Reset()
		rw.vmCfg.SkipAnalysis = txTask.SkipAnalysis
		ibs.SetTxContext(txHash, txTask.BlockHash, txTask.TxIndex)
		msg := txTask.TxAsMessage
		rw.evm.ResetBetweenBlocks(txTask.EvmBlockContext, core.NewEVMTxContext(msg), ibs, rw.vmCfg, rules)

		// MA applytx
		applyRes, err := core.ApplyMessage(rw.evm, msg, rw.taskGasPool, true /* refunds */, false /* gasBailout */)
		fmt.Printf("tx res: %t, %d, %s, %x, revertRes=%x, returnData=%x\n", applyRes.Failed(), txTask.TxIndex, err, txTask.Tx.Hash(), applyRes.Revert(), applyRes.Return())
		if err != nil {
			txTask.Error = err
		} else {
			//fmt.Printf("sender %v spent gas %d\n", txTask.TxAsMessage.From(), applyRes.UsedGas)
			txTask.UsedGas = applyRes.UsedGas
			//fmt.Printf("txn %d usedGas=%d\n", txTask.TxNum, txTask.UsedGas)
			// Update the state with pending changes
			ibs.SoftFinalise()
			//txTask.Error = ibs.FinalizeTx(rules, noop)
			txTask.Logs = ibs.GetLogs(txHash)
			txTask.TraceFroms = rw.callTracer.Froms()
			txTask.TraceTos = rw.callTracer.Tos()
		}

	}
	// Prepare read set, write set and balanceIncrease set and send for serialisation
	if txTask.Error == nil {
		txTask.BalanceIncreaseSet = ibs.BalanceIncreaseSet()
		//for addr, bal := range txTask.BalanceIncreaseSet {
		//	fmt.Printf("BalanceIncreaseSet [%x]=>[%d]\n", addr, &bal)
		//}
		if err = ibs.MakeWriteSet(rules, rw.stateWriter); err != nil {
			panic(err)
		}
		txTask.ReadLists = rw.stateReader.ReadSet()
		txTask.WriteLists = rw.stateWriter.WriteSet()
		txTask.AccountPrevs, txTask.AccountDels, txTask.StoragePrevs, txTask.CodePrevs = rw.stateWriter.PrevAndDels()
	}
}

type ChainReader struct {
	config      *chain.Config
	tx          kv.Tx
	blockReader services.FullBlockReader
}

func NewChainReader(config *chain.Config, tx kv.Tx, blockReader services.FullBlockReader) ChainReader {
	return ChainReader{config: config, tx: tx, blockReader: blockReader}
}

func (cr ChainReader) Config() *chain.Config        { return cr.config }
func (cr ChainReader) CurrentHeader() *types.Header { panic("") }
func (cr ChainReader) GetHeader(hash libcommon.Hash, number uint64) *types.Header {
	if cr.blockReader != nil {
		h, _ := cr.blockReader.Header(context.Background(), cr.tx, hash, number)
		return h
	}
	return rawdb.ReadHeader(cr.tx, hash, number)
}
func (cr ChainReader) GetHeaderByNumber(number uint64) *types.Header {
	if cr.blockReader != nil {
		h, _ := cr.blockReader.HeaderByNumber(context.Background(), cr.tx, number)
		return h
	}
	return rawdb.ReadHeaderByNumber(cr.tx, number)

}
func (cr ChainReader) GetHeaderByHash(hash libcommon.Hash) *types.Header {
	if cr.blockReader != nil {
		number := rawdb.ReadHeaderNumber(cr.tx, hash)
		if number == nil {
			return nil
		}
		return cr.GetHeader(hash, *number)
	}
	h, _ := rawdb.ReadHeaderByHash(cr.tx, hash)
	return h
}
func (cr ChainReader) GetTd(hash libcommon.Hash, number uint64) *big.Int {
	td, err := rawdb.ReadTd(cr.tx, hash, number)
	if err != nil {
		log.Error("ReadTd failed", "err", err)
		return nil
	}
	return td
}
func (cr ChainReader) FrozenBlocks() uint64 {
	return cr.blockReader.FrozenBlocks()
}
func (cr ChainReader) GetBlock(hash libcommon.Hash, number uint64) *types.Block {
	panic("")
}
func (cr ChainReader) HasBlock(hash libcommon.Hash, number uint64) bool {
	panic("")
}
func (cr ChainReader) BorEventsByBlock(hash libcommon.Hash, number uint64) []rlp.RawValue {
	panic("")
}

func NewWorkersPool(lock sync.Locker, logger log.Logger, ctx context.Context, background bool, chainDb kv.RoDB, rs *state.StateV3, in *state.QueueWithRetry, blockReader services.FullBlockReader, chainConfig *chain.Config, genesis *types.Genesis, engine consensus.Engine, workerCount int, dirs datadir.Dirs) (reconWorkers []*Worker, applyWorker *Worker, rws *state.ResultsQueue, clear func(), wait func()) {
	reconWorkers = make([]*Worker, workerCount)

	resultChSize := workerCount * 8
	rws = state.NewResultsQueue(resultChSize, workerCount) // workerCount * 4
	{
		// we all errors in background workers (except ctx.Cancel), because applyLoop will detect this error anyway.
		// and in applyLoop all errors are critical
		ctx, cancel := context.WithCancel(ctx)
		g, ctx := errgroup.WithContext(ctx)
		for i := 0; i < workerCount; i++ {
			reconWorkers[i] = NewWorker(lock, logger, ctx, background, chainDb, rs, in, blockReader, chainConfig, genesis, rws, engine, dirs)
		}
		if background {
			for i := 0; i < workerCount; i++ {
				i := i
				g.Go(func() error {
					return reconWorkers[i].Run()
				})
			}
			wait = func() { g.Wait() }
		}

		var clearDone bool
		clear = func() {
			if clearDone {
				return
			}
			clearDone = true
			cancel()
			g.Wait()
			for _, w := range reconWorkers {
				w.ResetTx(nil)
			}
			//applyWorker.ResetTx(nil)
		}
	}
	applyWorker = NewWorker(lock, logger, ctx, false, chainDb, rs, in, blockReader, chainConfig, genesis, rws, engine, dirs)

	return reconWorkers, applyWorker, rws, clear, wait
}
