package syncer

import (
	"context"
	"fmt"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gateway-fm/cdk-erigon-lib/common"
	ethereum "github.com/ledgerwatch/erigon"
	"github.com/ledgerwatch/log/v3"

	"encoding/binary"

	ethTypes "github.com/ledgerwatch/erigon/core/types"
	types "github.com/ledgerwatch/erigon/zk/rpcdaemon"
	"github.com/ledgerwatch/erigon/rpc"
)

var (
	batchWorkers = 2
)

var errorShortResponseLT32 = fmt.Errorf("response too short to contain hash data")
var errorShortResponseLT96 = fmt.Errorf("response too short to contain last batch number data")

const rollupSequencedBatchesSignature = "0x25280169" // hardcoded abi signature

type IEtherman interface {
	BlockByNumber(ctx context.Context, blockNumber *big.Int) (*ethTypes.Block, error)
	FilterLogs(ctx context.Context, query ethereum.FilterQuery) ([]ethTypes.Log, error)
	CallContract(ctx context.Context, msg ethereum.CallMsg, blockNumber *big.Int) ([]byte, error)
	TransactionByHash(ctx context.Context, hash common.Hash) (ethTypes.Transaction, bool, error)
}

type fetchJob struct {
	From uint64
	To   uint64
}

type jobResult struct {
	Size  uint64
	Error error
	Logs  []ethTypes.Log
}

type L1Syncer struct {
	etherMans            []IEtherman
	ethermanIndex        uint8
	ethermanMtx          *sync.Mutex
	l1ContractAddresses  []common.Address
	topics               [][]common.Hash
	blockRange           uint64
	queryDelay           uint64
	l1QueryBlocksThreads uint64

	latestL1Block uint64

	// atomic
	isSyncStarted      atomic.Bool
	isDownloading      atomic.Bool
	lastCheckedL1Block atomic.Uint64

	// Channels
	logsChan            chan []ethTypes.Log
	progressMessageChan chan string
}

func NewL1Syncer(etherMans []IEtherman, l1ContractAddresses []common.Address, topics [][]common.Hash, blockRange, queryDelay, l1QueryBlocksThreads uint64) *L1Syncer {
	return &L1Syncer{
		etherMans:            etherMans,
		ethermanIndex:        0,
		ethermanMtx:          &sync.Mutex{},
		l1ContractAddresses:  l1ContractAddresses,
		topics:               topics,
		blockRange:           blockRange,
		queryDelay:           queryDelay,
		l1QueryBlocksThreads: l1QueryBlocksThreads,
		progressMessageChan:  make(chan string),
		logsChan:             make(chan []ethTypes.Log),
	}
}

func (s *L1Syncer) getNextEtherman() IEtherman {
	s.ethermanMtx.Lock()
	defer s.ethermanMtx.Unlock()

	if s.ethermanIndex >= uint8(len(s.etherMans)) {
		s.ethermanIndex = 0
	}

	etherman := s.etherMans[s.ethermanIndex]
	s.ethermanIndex++

	return etherman
}

func (s *L1Syncer) IsSyncStarted() bool {
	return s.isSyncStarted.Load()
}

func (s *L1Syncer) IsDownloading() bool {
	return s.isDownloading.Load()
}

func (s *L1Syncer) GetLastCheckedL1Block() uint64 {
	return s.lastCheckedL1Block.Load()
}

// Channels
func (s *L1Syncer) GetLogsChan() chan []ethTypes.Log {
	return s.logsChan
}

func (s *L1Syncer) GetProgressMessageChan() chan string {
	return s.progressMessageChan
}

func (s *L1Syncer) Run(lastCheckedBlock uint64) {
	//if already started, don't start another thread
	if s.isSyncStarted.Load() {
		return
	}

	// set it to true to catch the first cycle run case where the check can pass before the latest block is checked
	s.isDownloading.Store(true)
	s.lastCheckedL1Block.Store(lastCheckedBlock)

	//start a thread to cheack for new l1 block in interval
	go func() {
		s.isSyncStarted.Store(true)
		defer s.isSyncStarted.Store(false)

		log.Info("Starting L1 syncer thread")
		defer log.Info("Stopping L1 syncer thread")

		for {
			latestL1Block, err := s.getLatestL1Block()
			if err != nil {
				log.Error("Error getting latest L1 block", "err", err)
			} else {
				if latestL1Block > s.lastCheckedL1Block.Load() {
					s.isDownloading.Store(true)
					if err := s.queryBlocks(); err != nil {
						log.Error("Error querying blocks", "err", err)
					} else {
						s.lastCheckedL1Block.Store(latestL1Block)
					}
				}
			}

			s.isDownloading.Store(false)
			time.Sleep(time.Duration(s.queryDelay) * time.Millisecond)
		}
	}()
}

func (s *L1Syncer) GetBlock(number uint64) (*ethTypes.Block, error) {
	em := s.getNextEtherman()
	return em.BlockByNumber(context.Background(), new(big.Int).SetUint64(number))
}

func (s *L1Syncer) GetTransaction(hash common.Hash) (ethTypes.Transaction, bool, error) {
	em := s.getNextEtherman()
	return em.TransactionByHash(context.Background(), hash)
}

func (s *L1Syncer) GetOldAccInputHash(ctx context.Context, addr *common.Address, rollupId, batchNum uint64) (common.Hash, error) {
	loopCount := 0
	for {
		if loopCount == 10 {
			return common.Hash{}, fmt.Errorf("too many retries")
		}

		h, previousBatch, err := s.callGetRollupSequencedBatches(ctx, addr, rollupId, batchNum)
		if err != nil {
			// if there is an error previousBatch value is incorrect so we can just try a single batch behind
			if batchNum > 0 && (err == errorShortResponseLT32 || err == errorShortResponseLT96) {
				batchNum--
				continue
			}

			log.Debug("Error getting rollup sequenced batch", "err", err)
			time.Sleep(time.Duration(loopCount*2) * time.Second)
			loopCount++
			continue
		}

		if h != types.ZeroHash {
			return h, nil
		}

		// h is 0 and if previousBatch is 0 then we can just try a single batch behind
		if batchNum > 0 && previousBatch == 0 {
			batchNum--
			continue
		}

		// if the hash is zero, we need to go back to the previous batch
		batchNum = previousBatch
		loopCount++
	}
}

func (s *L1Syncer) L1QueryBlocks(logPrefix string, logs []ethTypes.Log) (map[uint64]*ethTypes.Block, error) {
	// more thread causes error on remote rpc server
	numThreads := int(s.l1QueryBlocksThreads)
	blocksMap := map[uint64]*ethTypes.Block{}
	logsSize := len(logs)

	if numThreads > 1 && logsSize > (numThreads<<2) {
		var wg sync.WaitGroup
		var err error
		blocksArray := make([]*ethTypes.Block, logsSize)

		wg.Add(numThreads)

		for i := 0; i < numThreads; i++ {
			go func(cpuI int) {
				defer wg.Done()

				durationTick := time.Now()
				for j := cpuI; j < logsSize; j += numThreads {
					l := logs[j]
					block, e := s.GetBlock(l.BlockNumber)
					if e != nil {
						err = e
						return
					}
					blocksArray[j] = block
					tryToLogL1QueryBlocks(logPrefix, j/numThreads, logsSize/numThreads, cpuI+1, &durationTick)
				}
			}(i)
		}
		wg.Wait()

		if err != nil {
			return nil, err
		}

		for _, block := range blocksArray {
			blocksMap[block.NumberU64()] = block
		}
	} else {
		durationTick := time.Now()
		for i, l := range logs {
			block, err := s.GetBlock(l.BlockNumber)
			if err != nil {
				return nil, err
			}
			blocksMap[l.BlockNumber] = block
			tryToLogL1QueryBlocks(logPrefix, i, logsSize, 1, &durationTick)
		}
	}

	return blocksMap, nil
}

func tryToLogL1QueryBlocks(logPrefix string, current, total, threadNum int, durationTick *time.Time) {
	if time.Since(*durationTick).Seconds() > 10 {
		log.Info(fmt.Sprintf("[%s] %s %d/%d", logPrefix, "Query L1 blocks", current, total), "thread", threadNum)
		*durationTick = time.Now()
	}
}

func (s *L1Syncer) getLatestL1Block() (uint64, error) {
	em := s.getNextEtherman()
	latestBlock, err := em.BlockByNumber(context.Background(), big.NewInt(rpc.FinalizedBlockNumber.Int64()))
	if err != nil {
		return 0, err
	}

	latest := latestBlock.NumberU64()
	s.latestL1Block = latest

	return latest, nil
}

func (s *L1Syncer) queryBlocks() error {
	startBlock := s.lastCheckedL1Block.Load()

	log.Debug("GetHighestSequence", "startBlock", s.lastCheckedL1Block.Load())

	// define the blocks we're going to fetch up front
	fetches := make([]fetchJob, 0)
	low := startBlock
	for {
		high := low + s.blockRange
		if high > s.latestL1Block {
			// at the end of our search
			high = s.latestL1Block
		}

		fetches = append(fetches, fetchJob{
			From: low,
			To:   high,
		})

		if high == s.latestL1Block {
			break
		}
		low += s.blockRange + 1
	}

	stop := make(chan bool)
	jobs := make(chan fetchJob, len(fetches))
	results := make(chan jobResult, len(fetches))

	for i := 0; i < batchWorkers; i++ {
		go s.getSequencedLogs(jobs, results, stop)
	}

	for _, fetch := range fetches {
		jobs <- fetch
	}
	close(jobs)

	ticker := time.NewTicker(10 * time.Second)
	var progress uint64 = 0
	aimingFor := s.latestL1Block - startBlock
	complete := 0
loop:
	for {
		select {
		case res := <-results:
			complete++
			if res.Error != nil {
				close(stop)
				return res.Error
			}
			progress += res.Size
			if len(res.Logs) > 0 {
				s.logsChan <- res.Logs
			}

			if complete == len(fetches) {
				// we've got all the results we need
				close(stop)
				break loop
			}
		case <-ticker.C:
			if aimingFor == 0 {
				continue
			}
			s.progressMessageChan <- fmt.Sprintf("L1 Blocks processed progress (amounts): %d/%d (%d%%)", progress, aimingFor, (progress*100)/aimingFor)
		}
	}

	return nil
}

func (s *L1Syncer) getSequencedLogs(jobs <-chan fetchJob, results chan jobResult, stop chan bool) {
	for {
		select {
		case <-stop:
			return
		case j, ok := <-jobs:
			if !ok {
				return
			}
			query := ethereum.FilterQuery{
				FromBlock: new(big.Int).SetUint64(j.From),
				ToBlock:   new(big.Int).SetUint64(j.To),
				Addresses: s.l1ContractAddresses,
				Topics:    s.topics,
			}

			var logs []ethTypes.Log
			var err error
			retry := 0
			for {
				em := s.getNextEtherman()
				logs, err = em.FilterLogs(context.Background(), query)
				if err != nil {
					log.Debug("getSequencedLogs retry error", "err", err)
					retry++
					if retry > 5 {
						results <- jobResult{
							Error: err,
							Logs:  nil,
						}
						return
					}
					time.Sleep(time.Duration(retry*2) * time.Second)
					continue
				}
				break
			}

			results <- jobResult{
				Size:  j.To - j.From,
				Error: nil,
				Logs:  logs,
			}
		}
	}
}

func (s *L1Syncer) callGetRollupSequencedBatches(ctx context.Context, addr *common.Address, rollupId, batchNum uint64) (common.Hash, uint64, error) {
	rollupID := fmt.Sprintf("%064x", rollupId)
	batchNumber := fmt.Sprintf("%064x", batchNum)

	em := s.getNextEtherman()
	resp, err := em.CallContract(ctx, ethereum.CallMsg{
		To:   addr,
		Data: common.FromHex(rollupSequencedBatchesSignature + rollupID + batchNumber),
	}, nil)

	if err != nil {
		return common.Hash{}, 0, err
	}

	if len(resp) < 32 {
		return common.Hash{}, 0, errorShortResponseLT32
	}
	h := common.BytesToHash(resp[:32])

	if len(resp) < 96 {
		return common.Hash{}, 0, errorShortResponseLT96
	}
	lastBatchNumber := binary.BigEndian.Uint64(resp[88:96])

	return h, lastBatchNumber, nil
}
