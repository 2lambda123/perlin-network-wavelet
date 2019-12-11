// Copyright (c) 2019 Perlin
//
// Permission is hereby granted, free of charge, to any person obtaining a copy of
// this software and associated documentation files (the "Software"), to deal in
// the Software without restriction, including without limitation the rights to
// use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of
// the Software, and to permit persons to whom the Software is furnished to do so,
// subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS
// FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR
// COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER
// IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN
// CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.

package wavelet

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"github.com/minio/highwayhash"
	"github.com/perlin-network/noise/edwards25519"
	"github.com/perlin-network/wavelet/internal/cuckoo"
	"github.com/perlin-network/wavelet/internal/filebuffer"
	"github.com/perlin-network/wavelet/internal/radix"
	"github.com/perlin-network/wavelet/internal/stall"
	"github.com/perlin-network/wavelet/internal/worker"
	"io"
	"math/rand"
	"reflect"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/perlin-network/noise"
	"github.com/perlin-network/noise/skademlia"
	"github.com/perlin-network/wavelet/avl"
	"github.com/perlin-network/wavelet/conf"
	"github.com/perlin-network/wavelet/log"
	"github.com/perlin-network/wavelet/store"
	"github.com/perlin-network/wavelet/sys"
	"github.com/pkg/errors"
	"golang.org/x/crypto/blake2b"
	"google.golang.org/grpc"
	"google.golang.org/grpc/peer"
)

type bitset uint8

const (
	outOfSync   bitset = 0 //nolint:staticcheck
	synced             = 1
	finalized          = 2
	fullySynced        = 3
)

var (
	ErrMissingTx = errors.New("missing transaction")
)

type Ledger struct {
	client  *skademlia.Client
	metrics *Metrics
	indexer *radix.Indexer

	accounts     *Accounts
	blocks       *Blocks
	transactions *Transactions
	db           store.KV

	finalizer *Snowball
	syncer    *Snowball

	consensus sync.WaitGroup

	sync chan struct{}

	syncStatus     bitset
	syncStatusLock sync.RWMutex

	filePool *filebuffer.Pool

	stallDetector *stall.Detector

	stopWG   sync.WaitGroup
	cancelGC context.CancelFunc

	transactionFilterLock sync.RWMutex
	transactionFilter     *cuckoo.Filter

	queryPeerBlockCache  map[edwards25519.PublicKey]*Block
	queryBlockValidCache map[BlockID]struct{}
	queryStateCache      *StateLRU

	queryWorkerPool *worker.Pool

	collapseResultsLogger *CollapseResultsLogger
}

type config struct {
	GCDisabled  bool
	Genesis     *string
	MaxMemoryMB uint64
}

type Option func(cfg *config)

// WithoutGC disables GC. Used for testing purposes.
func WithoutGC() Option {
	return func(cfg *config) {
		cfg.GCDisabled = true
	}
}

func WithGenesis(genesis *string) Option {
	return func(cfg *config) {
		cfg.Genesis = genesis
	}
}

func WithMaxMemoryMB(n uint64) Option {
	return func(cfg *config) {
		cfg.MaxMemoryMB = n
	}
}

func NewLedger(kv store.KV, client *skademlia.Client, opts ...Option) (*Ledger, error) {
	var cfg config

	for _, opt := range opts {
		opt(&cfg)
	}

	metrics := NewMetrics(context.TODO())
	indexer := radix.NewIndexer()
	accounts := NewAccounts(kv)

	var block *Block

	blocks, err := NewBlocks(kv, conf.GetPruningLimit())
	if err != nil {
		if errors.Cause(err) != store.ErrNotFound {
			return nil, errors.Wrap(err, "error getting blocks from db")
		}

		genesis := performInception(accounts.tree, cfg.Genesis)

		if err := accounts.Commit(nil); err != nil {
			return nil, errors.Wrap(err, "error committing accounts from genesis")
		}

		ptr := &genesis

		if _, err := blocks.Save(ptr); err != nil {
			return nil, errors.Wrap(err, "error saving genesis block to db")
		}

		block = ptr
	} else {
		block = blocks.Latest()
	}

	transactions := NewTransactions(*block)

	finalizer := NewSnowball()
	syncer := NewSnowball()

	ledger := &Ledger{
		client:  client,
		metrics: metrics,
		indexer: indexer,

		accounts:     accounts,
		blocks:       blocks,
		transactions: transactions,
		db:           kv,

		finalizer: finalizer,
		syncer:    syncer,

		syncStatus: finalized, // we start node as out of sync, but finalized

		sync: make(chan struct{}),

		filePool: filebuffer.NewPool(sys.SyncPooledFileSize, ""),

		transactionFilter: cuckoo.NewFilter(),

		queryPeerBlockCache:  make(map[edwards25519.PublicKey]*Block),
		queryBlockValidCache: make(map[BlockID]struct{}),
		queryStateCache:      NewStateLRU(16),

		queryWorkerPool: worker.NewWorkerPool(),

		collapseResultsLogger: NewCollapseResultsLogger(),
	}

	if err := ledger.loadTransactions(); err != nil && errors.Cause(err) != store.ErrNotFound {
		return nil, errors.Wrap(err, "error loading transactions from db")
	}

	if !cfg.GCDisabled {
		ctx, cancel := context.WithCancel(context.Background())

		ledger.stopWG.Add(1)

		go accounts.GC(ctx, &ledger.stopWG)

		ledger.cancelGC = cancel
	}

	stallDetector := stall.NewStallDetector(stall.Config{
		MaxMemoryMB: cfg.MaxMemoryMB,
	}, stall.Delegate{
		PrepareShutdown: func(err error) {
			logger := log.Node()
			logger.Error().Err(err).Msg("Shutting down node...")
		},
	})

	ledger.stopWG.Add(1)

	go stallDetector.Run(&ledger.stopWG)

	ledger.stallDetector = stallDetector

	ledger.queryWorkerPool.Start(16)
	ledger.PerformConsensus()

	go ledger.SyncToLatestBlock()

	return ledger, nil
}

// Close stops all goroutines and waits for them to complete.
func (l *Ledger) Close() {
	syncOpen := true
	select {
	case _, syncOpen = <-l.sync:
	default:
	}

	if syncOpen {
		close(l.sync)
	}

	l.consensus.Wait()

	if l.cancelGC != nil {
		l.cancelGC()
	}

	l.queryWorkerPool.Stop()

	l.stallDetector.Stop()

	l.collapseResultsLogger.Stop()

	l.stopWG.Wait()
}

// AddTransaction adds a transaction to the ledger and adds it's id to a probabilistic
// data structure used to sync transactions.
func (l *Ledger) AddTransaction(verifySignature bool, txs ...Transaction) {
	l.transactions.BatchAdd(txs, verifySignature)
	l.transactionFilterLock.Lock()

	for _, tx := range txs {
		l.transactionFilter.Insert(tx.ID)
	}

	l.transactionFilterLock.Unlock()
}

// Find searches through complete transaction and account indices for a specified
// query string. All indices that queried are in the form of tries. It is safe
// to call this method concurrently.
func (l *Ledger) Find(query string, max int) (results []string) {
	var err error

	if max > 0 {
		results = make([]string, 0, max)
	}

	prefix := []byte(query)

	if len(query)%2 == 1 { // Cut off a single character.
		prefix = prefix[:len(prefix)-1]
	}

	prefix, err = hex.DecodeString(string(prefix))
	if err != nil {
		return nil
	}

	fullQuery := append(keyAccounts[:], prefix...)

	l.Snapshot().IterateFrom(fullQuery, func(key, _ []byte) bool {
		if !bytes.HasPrefix(key, fullQuery) {
			return false
		}

		if max > 0 && len(results) >= max {
			return false
		}

		results = append(results, hex.EncodeToString(key[len(keyAccounts):]))
		return true
	})

	var count = -1
	if max > 0 {
		count = max - len(results)
	}

	return append(results, l.indexer.Find(query, count)...)
}

// Protocol returns an implementation of WaveletServer to handle incoming
// RPC and streams for the ledger. The protocol is agnostic to whatever
// choice of network stack is used with Wavelet, though by default it is
// intended to be used with gRPC and Noise.
func (l *Ledger) Protocol() *Protocol {
	return &Protocol{ledger: l}
}

// Finalizer returns the Snowball finalizer which finalizes the contents of individual
// blocks.
func (l *Ledger) Finalizer() *Snowball {
	return l.finalizer
}

// Blocks returns the block manager for the ledger.
func (l *Ledger) Blocks() *Blocks {
	return l.blocks
}

// Transactions returns the transaction manager for the ledger.
func (l *Ledger) Transactions() *Transactions {
	return l.transactions
}

// Restart restart wavelet process by means of stall detector (approach is platform dependent)
func (l *Ledger) Restart() error {
	return l.stallDetector.TryRestart()
}

// PerformConsensus spawns workers related to performing consensus, such as pulling
// missing transactions and incrementally finalizing intervals of transactions in
// the ledgers graph.
func (l *Ledger) PerformConsensus() {
	l.consensus.Add(3)

	go l.PullMissingTransactions()

	go l.SyncTransactions()

	go l.FinalizeBlocks()
}

func (l *Ledger) Snapshot() *avl.Tree {
	return l.accounts.Snapshot()
}

// SyncTransactions is an infinite loop which constantly sends transaction ids from its index
// into a Cuckoo Filter to randomly sampled number of peers and adds to it's state all received
// transactions.
func (l *Ledger) SyncTransactions() { // nolint:gocognit
	defer l.consensus.Done()

	for {
		select {
		case <-l.sync:
			return
		case <-time.After(3 * time.Second):
		}

		snowballK := conf.GetSnowballK()

		peers, err := SelectPeers(l.client.ClosestPeers(), snowballK)
		if err != nil {
			continue
		}

		logger := log.Sync("sync_tx")

		l.transactionFilterLock.RLock()
		cf := l.transactionFilter.MarshalBinary()
		l.transactionFilterLock.RUnlock()

		if err != nil {
			logger.Error().Err(err).Msg("failed to marshal set membership filter data")
			continue
		}

		var wg sync.WaitGroup

		bfReq := &TransactionsSyncRequest{
			Data: &TransactionsSyncRequest_Filter{
				Filter: cf,
			},
		}

		for _, p := range peers {
			wg.Add(1)

			go func(conn *grpc.ClientConn) {
				defer wg.Done()

				ctx, cancel := context.WithTimeout(context.Background(), conf.GetDownloadTxTimeout())
				defer cancel()

				stream, err := NewWaveletClient(conn).SyncTransactions(ctx)
				if err != nil {
					logger.Error().Err(err).Msg("failed to create sync transactions stream")
					return
				}

				defer func() {
					if err := stream.CloseSend(); err != nil {
						logger.Error().Err(err).Msg("failed to send set membership filter data")
					}
				}()

				if err := stream.Send(bfReq); err != nil {
					logger.Error().Err(err).Msg("failed to send set membership filter data")
					return
				}

				res, err := stream.Recv()
				if err != nil {
					logger.Error().Err(err).Msg("failed to receive sync transactions header")
					return
				}

				count := res.GetTransactionsNum()
				if count == 0 {
					return
				}

				if count > conf.GetTXSyncLimit() {
					logger.Debug().
						Uint64("count", count).
						Str("peer_address", conn.Target()).
						Msg("Bad number of transactions would be received")
					return
				}

				logger.Debug().
					Uint64("count", count).
					Msg("Requesting transaction(s) to sync.")

				for count > 0 {
					req := TransactionsSyncRequest_ChunkSize{
						ChunkSize: conf.GetTXSyncChunkSize(),
					}

					if count < req.ChunkSize {
						req.ChunkSize = count
					}

					if err := stream.Send(&TransactionsSyncRequest{Data: &req}); err != nil {
						logger.Error().Err(err).Msg("failed to receive sync transactions header")
						return
					}

					res, err := stream.Recv()
					if err != nil {
						logger.Error().Err(err).Msg("failed to receive sync transactions header")
						return
					}

					txResponse := res.GetTransactions()
					if txResponse == nil {
						return
					}

					transactions := make([]Transaction, 0, len(txResponse.Transactions))
					for _, txBody := range txResponse.Transactions {
						tx, err := UnmarshalTransaction(bytes.NewReader(txBody))
						if err != nil {
							logger.Error().Err(err).Msg("failed to unmarshal synced transaction")
							continue
						}

						if !tx.VerifySignature() {
							logger.Error().
								Hex("tx_id", tx.ID[:]).
								Msg("bad signature")
							continue
						}

						transactions = append(transactions, tx)
					}

					downloadedNum := len(transactions)
					count -= uint64(downloadedNum)

					l.AddTransaction(false, transactions...)

					l.metrics.downloadedTX.Mark(int64(downloadedNum))
					l.metrics.receivedTX.Mark(int64(downloadedNum))
				}
			}(p.Conn())
		}

		wg.Wait()
	}
}

// PullMissingTransactions is a goroutine which continuously pulls missing transactions
// from randomly sampled peers in the network. It does so by sending a list of
// transaction IDs which node is missing.
func (l *Ledger) PullMissingTransactions() {
	defer l.consensus.Done()

	for {
		select {
		case <-l.sync:
			return
		case <-time.After(100 * time.Millisecond):
		}

		snowballK := conf.GetSnowballK()

		peers, err := SelectPeers(l.client.ClosestPeers(), snowballK)
		if err != nil {
			continue
		}

		logger := log.Sync("pull_missing_tx")

		// Build list of transaction IDs
		missingIDs := l.transactions.MissingIDs()

		missingTxPullLimit := conf.GetMissingTxPullLimit()
		if uint64(len(missingIDs)) > missingTxPullLimit {
			missingIDs = missingIDs[:missingTxPullLimit]
		}

		req := &TransactionPullRequest{TransactionIds: make([][]byte, 0, len(missingIDs))}

		for _, txID := range missingIDs {
			txID := txID
			req.TransactionIds = append(req.TransactionIds, txID[:])
		}

		type response struct {
			txs []Transaction
		}

		responseChan := make(chan response)

		for _, p := range peers {
			go func(conn *grpc.ClientConn) {
				var response response

				defer func() {
					responseChan <- response
				}()

				client := NewWaveletClient(conn)

				ctx, cancel := context.WithTimeout(context.Background(), conf.GetDownloadTxTimeout())
				defer cancel()

				batch, err := client.PullTransactions(ctx, req)
				if err != nil {
					logger.Error().Err(err).Msg("failed to download missing transactions")

					return
				}

				response.txs = make([]Transaction, 0, len(batch.Transactions))

				for _, buf := range batch.Transactions {
					tx, err := UnmarshalTransaction(bytes.NewReader(buf))
					if err != nil {
						logger.Error().
							Err(err).
							Hex("tx_id", tx.ID[:]).
							Msg("error unmarshaling downloaded tx")

						continue
					}

					response.txs = append(response.txs, tx)
				}
			}(p.Conn())
		}

		count := int64(0)
		pulled := make(map[TransactionID]Transaction)

		for i := 0; i < len(peers); i++ {
			res := <-responseChan

			for i := range res.txs {
				if _, ok := pulled[res.txs[i].ID]; !ok {
					pulled[res.txs[i].ID] = res.txs[i]
					count += int64(res.txs[i].LogicalUnits())
				}
			}
		}

		close(responseChan)

		pulledTXs := make([]Transaction, 0, len(pulled))

		for _, tx := range pulled {
			if !tx.VerifySignature() {
				logger.Error().
					Hex("tx_id", tx.ID[:]).
					Msg("bad signature")

				continue
			}

			pulledTXs = append(pulledTXs, tx)
		}

		l.AddTransaction(false, pulledTXs...)

		if count > 0 {
			logger.Info().
				Int64("count", count).
				Msg("Pulled missing transaction(s).")
		}

		l.metrics.downloadedTX.Mark(count)
		l.metrics.receivedTX.Mark(count)
	}
}

// FinalizeBlocks continuously attempts to finalize blocks.
func (l *Ledger) FinalizeBlocks() {
	defer l.consensus.Done()

	for {
		select {
		case <-l.sync:
			return
		default:
		}

		decided := l.finalizer.Decided()

		preferred := l.finalizer.Preferred()
		if preferred == nil {
			proposedBlock := l.proposeBlock()
			logger := log.Consensus("proposal")

			if proposedBlock != nil {
				logger.Debug().
					Hex("block_id", proposedBlock.ID[:]).
					Uint64("block_index", proposedBlock.Index).
					Int("num_transactions", len(proposedBlock.Transactions)).
					Msg("Proposing block...")

				l.finalizer.Prefer(&finalizationVote{
					block: proposedBlock,
				})
			}
		} else {
			if decided {
				l.finalize(*preferred.Value().(*Block))
			} else {
				l.query()
			}
		}
	}
}

// proposeBlock takes all transactions from the first quarter of the mempool
// and creates a new block, which will be proposed to be finalized as the
// next block in the chain.
func (l *Ledger) proposeBlock() *Block {
	proposing := l.transactions.ProposableIDs()
	if len(proposing) == 0 {
		return nil
	}

	latest := l.blocks.Latest()

	results, err := l.collapseTransactions(latest.Index+1, latest, proposing, false)
	if err != nil {
		logger := log.Node()
		logger.Error().
			Err(err).
			Msg("error collapsing transactions during block proposal")

		return nil
	}

	proposed := NewBlock(latest.Index+1, results.snapshot.Checksum(), proposing...)

	return &proposed
}

func (l *Ledger) finalize(block Block) {
	current := l.blocks.Latest()

	logger := log.Consensus("finalized")

	results, err := l.collapseTransactions(block.Index, current, block.Transactions, true)
	if err != nil {
		logger := log.Node()
		logger.Error().
			Err(err).
			Msg("error collapsing transactions during finalization")

		return
	}

	if checksum := results.snapshot.Checksum(); checksum != block.Merkle {
		logger := log.Node()
		logger.Error().
			Uint64("target_block_id", block.Index).
			Hex("expected_merkle_root", block.Merkle[:]).
			Hex("yielded_merkle_root", checksum[:]).
			Msg("Merkle root does not match")

		return
	}

	pruned := l.transactions.ReshufflePending(block)
	l.transactionFilterLock.Lock()
	for _, txID := range pruned {
		l.transactionFilter.Delete(txID)
	}
	l.transactionFilterLock.Unlock()

	evicted, err := l.blocks.Save(&block)
	if err != nil {
		logger := log.Node()
		logger.Error().
			Err(err).
			Msg("Failed to save preferred block to database")

		return
	}

	if err := l.storeTransactions(&block, evicted); err != nil {
		logger := log.Node()
		logger.Error().
			Err(err).
			Msg("Failed to save transactions from preferred block to database")

		return
	}

	if err = l.accounts.Commit(results.snapshot); err != nil {
		logger := log.Node()
		logger.Error().
			Err(err).
			Msg("Failed to commit collaped state to our database")

		return
	}

	l.metrics.acceptedTX.Mark(int64(results.appliedCount))
	l.metrics.finalizedBlocks.Mark(1)

	l.LogChanges(results.snapshot, current.Index)

	l.applySync(finalized)

	// Reset sampler(s).
	l.finalizer.Reset()

	// Reset querying-related cache(s).
	for id := range l.queryBlockValidCache {
		delete(l.queryBlockValidCache, id)
	}

	logger.Info().
		Int("num_applied_tx", results.appliedCount).
		Int("num_rejected_tx", results.rejectedCount).
		Int("num_pruned_tx", len(pruned)).
		Uint64("old_block_height", current.Index).
		Uint64("new_block_height", block.Index).
		Hex("old_block_id", current.ID[:]).
		Hex("new_block_id", block.ID[:]).
		Msg("Finalized block.")
}

func (l *Ledger) query() {
	snowballK := conf.GetSnowballK()

	peers, err := SelectPeers(l.client.ClosestPeers(), snowballK)
	if err != nil {
		return
	}

	current := l.blocks.Latest()

	type response struct {
		vote finalizationVote

		cacheValid bool
		cacheBlock *Block
	}

	responseChan := make(chan response)

	for _, p := range peers {
		conn := p.Conn()
		cached := l.queryPeerBlockCache[p.ID().Checksum()]

		f := func() {
			var response response

			defer func() {
				responseChan <- response
			}()

			req := &QueryRequest{BlockIndex: current.Index + 1}

			if cached != nil {
				req.CacheBlockId = make([]byte, SizeBlockID)
				copy(req.CacheBlockId, cached.ID[:])

				response.cacheBlock = cached
			}

			f := func() {
				client := NewWaveletClient(conn)

				ctx, cancel := context.WithTimeout(context.Background(), conf.GetQueryTimeout())
				defer cancel()

				p := &peer.Peer{}

				res, err := client.Query(ctx, req, grpc.Peer(p))
				if err != nil {
					logger := log.Node()
					logger.Error().
						Err(err).
						Msg("error while querying peer")

					return
				}

				l.metrics.queried.Mark(1)

				info := noise.InfoFromPeer(p)
				if info == nil {
					return
				}

				voter, ok := info.Get(skademlia.KeyID).(*skademlia.ID)
				if !ok {
					return
				}

				response.vote.voter = voter

				if res.CacheValid {
					response.cacheValid = true
					return
				}

				block, err := UnmarshalBlock(bytes.NewReader(res.GetBlock()))
				if err != nil {
					return
				}

				if block.ID == ZeroBlockID {
					return
				}

				response.vote.block = &block
			}
			l.metrics.queryLatency.Time(f)
		}

		l.queryWorkerPool.Queue(f)
	}

	votes := make([]Vote, 0, len(peers))
	voters := make(map[AccountID]struct{}, len(peers))

	for i := 0; i < cap(votes); i++ {
		response := <-responseChan

		if response.vote.voter == nil {
			continue
		}

		if _, recorded := voters[response.vote.voter.PublicKey()]; recorded {
			continue // To make sure the sampling process is fair, only allow one vote per peer.
		}

		voters[response.vote.voter.PublicKey()] = struct{}{}

		if response.cacheValid {
			response.vote.block = response.cacheBlock
		} else if response.vote.block != nil {
			l.queryPeerBlockCache[response.vote.voter.Checksum()] = response.vote.block
		}

		votes = append(votes, &response.vote)
	}

	l.filterInvalidVotes(current, votes)
	l.finalizer.Tick(calculateTallies(l.accounts, votes))
}

// SyncToLatestBlock continuously checks if the node is out of sync from its peers.
// If the majority of its peers responded that it is out of sync (decided using snowball),
// the node will attempt to sync its state to the latest block by downloading the AVL tree
// diff from its peers and applying the diff to its local AVL tree.
func (l *Ledger) SyncToLatestBlock() {
	logger := log.Sync("sync")
	syncTimeoutMultiplier := 0

	for {
		if !l.isOutOfSync() {
			l.applySync(synced)
			l.syncer.Reset()

			if syncTimeoutMultiplier < 60 {
				syncTimeoutMultiplier++
			}

			//logger.Debug().Msgf("Not out of sync, sleeping %d seconds", syncTimeoutMultiplier)

			time.Sleep(time.Duration(syncTimeoutMultiplier) * time.Second)

			continue
		}

		l.setSync(outOfSync)

		current := l.blocks.Latest()
		syncTimeoutMultiplier = 0

		close(l.sync)
		l.consensus.Wait() // Wait for all consensus-related workers to shutdown.

		l.finalizer.Reset() // Reset consensus Snowball sampler.
		l.syncer.Reset()    // Reset syncing Snowball sampler.

		logger.Info().
			Uint64("current_block_index", current.Index).
			Msg("Noticed that we are out of sync; downloading latest state Snapshot from our peer(s).")

		synced := false
		for !synced {
			synced = l.performSync(current)
		}

		l.transactions.ReshufflePending(*l.blocks.Latest())

		l.sync = make(chan struct{})
		l.PerformConsensus()
	}
}

func (l *Ledger) performSync(current *Block) bool { // nolint:gocyclo,gocognit
	logger := log.Sync("sync")

	peers, err := SelectPeers(l.client.ClosestPeers(), conf.GetSnowballK())
	if err != nil {
		logger.Warn().Msg("It looks like there are no peers for us to sync with. Retrying...")
		time.Sleep(1 * time.Second)

		return false
	}

	req := &SyncRequest{Data: &SyncRequest_BlockId{BlockId: current.Index}}

	type response struct {
		peer   skademlia.ClosestPeer
		header *SyncInfo
		latest Block
		stream Wavelet_SyncClient
	}

	responseChan := make(chan response)

	for _, p := range peers {
		go func(p skademlia.ClosestPeer) {
			var resp response

			defer func() {
				responseChan <- resp
			}()

			stream, err := NewWaveletClient(p.Conn()).Sync(context.Background())
			if err != nil {
				return
			}

			if err := stream.Send(req); err != nil {
				return
			}

			res, err := stream.Recv()
			if err != nil {
				return
			}

			header := res.GetHeader()
			if header == nil {
				return
			}

			latest, err := UnmarshalBlock(bytes.NewReader(header.Block))
			if err != nil {
				return
			}

			if latest.Index == 0 || len(header.Checksums) == 0 {
				return
			}

			resp = response{
				peer:   p,
				header: header,
				latest: latest,
				stream: stream,
			}
		}(p)
	}

	responses := make([]response, 0, len(peers))

	for i := 0; i < len(peers); i++ {
		if res := <-responseChan; res.header != nil {
			responses = append(responses, res)
		}
	}

	close(responseChan)

	if len(responses) == 0 {
		return false
	}

	defer func() {
		for _, res := range responses {
			_ = res.stream.CloseSend()
		}
	}()

	set := make(map[uint64][]response)

	for _, v := range responses {
		set[v.latest.Index] = append(set[v.latest.Index], v)
	}

	// Select a block to sync to which the majority of peers are on.

	var (
		latest      *Block
		latestVoter skademlia.ClosestPeer
		majority    []response
	)

	for _, votes := range set {
		if len(votes) >= len(set)*2/3 {
			majorityVote := votes[0]
			latest = &majorityVote.latest
			latestVoter = majorityVote.peer
			majority = votes

			break
		}
	}

	// If there is no majority or a tie, dispose all streams and try again.
	if majority == nil || latest == nil {
		logger.Warn().Msg("It looks like our peers could not decide on what the latest block currently is. Retrying...")

		return false
	}

	logger.Debug().
		Uint64("block_id", latest.Index).
		Hex("merkle_root", latest.Merkle[:]).
		Msg("Discovered the latest block the majority of our peers ar eon.")

	type source struct {
		idx      int
		checksum [blake2b.Size256]byte
		streams  []Wavelet_SyncClient
		size     int
	}

	var sources []source

	idx := 0

	// For each chunk checksum from the set of checksums provided by each
	// peer, pick the majority checksum.

	for {
		set := make(map[[blake2b.Size256]byte][]Wavelet_SyncClient)

		for _, response := range majority {
			if idx >= len(response.header.Checksums) {
				continue
			}

			var checksum [blake2b.Size256]byte

			copy(checksum[:], response.header.Checksums[idx])

			set[checksum] = append(set[checksum], response.stream)
		}

		if len(set) == 0 {
			break // We have finished going through all responses. Engage in syncing.
		}

		// Figure out what the majority of peers believe the checksum is for a chunk
		// at index idx. If a majority is found, mark the peers as a viable source
		// for grabbing the chunks contents to then reassemble together an AVL+ tree
		// diff to apply to our ledger state to complete syncing.

		consistent := false

		for checksum, voters := range set {
			if len(voters) == 0 || len(voters) < len(majority)*2/3 {
				continue
			}

			sources = append(sources, source{idx: idx, checksum: checksum, streams: voters})
			consistent = true

			break
		}

		// If peers could not come up with a consistent checksum for some
		// chunk at a consistent idx, dispose all streams and try again.

		if !consistent {
			return false
		}

		idx++
	}

	// Streams may not concurrently send and receive messages at once.
	streamLocks := make(map[Wavelet_SyncClient]*sync.Mutex)

	var (
		streamLock       sync.Mutex
		chunkWG          sync.WaitGroup
		chunksBufferLock sync.Mutex
	)

	logger.Debug().
		Int("num_chunks", len(sources)).
		Msg("Starting up workers to downloaded all chunks of data needed to sync to the latest block...")

	chunksBuffer, err := l.filePool.GetBounded(int64(len(sources)) * sys.SyncChunkSize)
	if err != nil {
		logger.Error().
			Err(err).
			Msg("Could not create paged buffer! Retrying...")

		return false
	}

	diffBuffer := l.filePool.GetUnbounded()

	defer func() {
		l.filePool.Put(chunksBuffer)
		l.filePool.Put(diffBuffer)
	}()

	chunkWG.Add(len(sources))

	for _, src := range sources {
		go func(src source) {
			defer chunkWG.Done()

			req := &SyncRequest{Data: &SyncRequest_Checksum{Checksum: src.checksum[:]}}

			for range src.streams {
				stream := src.streams[rand.Intn(len(src.streams))]

				// Lock the stream so that other workers may not concurrently interact
				// with the exact same stream at once.

				streamLock.Lock()
				if _, exists := streamLocks[stream]; !exists {
					streamLocks[stream] = new(sync.Mutex)
				}
				lock := streamLocks[stream]
				streamLock.Unlock()

				lock.Lock()

				if err := stream.Send(req); err != nil {
					lock.Unlock()
					continue
				}

				res, err := stream.Recv()
				if err != nil {
					lock.Unlock()
					continue
				}

				lock.Unlock()

				chunk := res.GetChunk()
				if chunk == nil {
					continue
				}

				if len(chunk) > conf.GetSyncChunkSize() {
					continue
				}

				if blake2b.Sum256(chunk) != src.checksum {
					continue
				}

				// We found the chunk! Store the chunks contents.
				chunksBufferLock.Lock()
				_, err = chunksBuffer.WriteAt(chunk, int64(src.idx)*sys.SyncChunkSize)
				chunksBufferLock.Unlock()

				if err != nil {
					continue
				}

				sources[src.idx].size = len(chunk)

				break
			}
		}(src)
	}

	chunkWG.Wait() // Wait until all chunks have been received.

	logger.Debug().
		Int("num_chunks", len(sources)).
		Msg(
			"Downloaded whatever chunks were available to sync to the latest block, and shutted down all " +
				"workers. Checking validity of chunks...",
		)

	// Check all chunks has been received
	var diffSize int64

	for i, src := range sources {
		if src.size == 0 {
			logger.Error().
				Uint64("target_block_id", latest.Index).
				Hex("chunk_checksum", sources[i].checksum[:]).
				Msg("Could not download one of the chunks necessary to sync to the latest block! Retrying...")

			return false
		}

		diffSize += int64(src.size)
	}

	if _, err := io.CopyN(diffBuffer, chunksBuffer, diffSize); err != nil {
		logger.Error().
			Uint64("target_block_id", latest.Index).
			Err(err).
			Msg("Failed to write chunks to bounded memory buffer. Restarting sync...")

		return false
	}

	logger.Info().
		Int("num_chunks", len(sources)).
		Uint64("target_block", latest.Index).
		Msg("All chunks have been successfully verified and re-assembled into a diff. Applying diff...")

	snapshot := l.accounts.Snapshot()
	if err := snapshot.ApplyDiff(diffBuffer); err != nil {
		logger.Error().
			Uint64("target_block_id", latest.Index).
			Err(err).
			Msg("Failed to apply re-assembled diff to our ledger state. Restarting sync...")

		return false
	}

	if checksum := snapshot.Checksum(); checksum != latest.Merkle {
		logger.Error().
			Uint64("target_block_id", latest.Index).
			Hex("expected_merkle_root", latest.Merkle[:]).
			Hex("yielded_merkle_root", checksum[:]).
			Msg("Failed to apply re-assembled diff to our ledger state. Restarting sync...")

		return false
	}

	// get all the blocks up until latest one starting either from latest saved one or oldest possible
	oldestBlockIx := current.Index
	if pl := uint64(conf.GetPruningLimit()); latest.Index > pl && latest.Index-pl > oldestBlockIx {
		oldestBlockIx = latest.Index - pl
	}

	var blocks []*Block

	client := NewWaveletClient(latestVoter.Conn())

	for i := oldestBlockIx; i < latest.Index; i++ {
		if _, err := l.blocks.GetByIndex(i); err == nil {
			continue
		}

		resp, err := client.GetBlock(context.Background(), &QueryRequest{BlockIndex: i})
		if err != nil {
			if strings.Contains(err.Error(), ErrRoundPruned.Error()) {
				logger.Debug().Uint64("block_index", i).Msg("Skipping pruned block.")
				continue
			}

			logger.Error().
				Err(err).
				Msg("Failed to save finalized block to our database")

			return false
		}

		block, err := UnmarshalBlock(bytes.NewReader(resp.Block))
		if err != nil {
			logger.Error().
				Err(err).
				Msg("Failed to save finalized block to our database")

			return false
		}

		blocks = append(blocks, &block)
	}

	blocks = append(blocks, latest)

	logger.Info().
		Int("blocks", len(blocks)).
		Msg("Going to save downloaded blocks")

	for _, b := range blocks {
		evicted, err := l.blocks.Save(b)
		if err != nil {
			logger.Error().
				Err(err).
				Msg("Failed to save finalized block to our database")

			return false
		}

		if err := l.storeTransactions(b, evicted); err != nil {
			logger.Error().
				Err(err).
				Msg("Failed to save transactions from finalized block to our database")

			return false
		}
	}

	if err := l.accounts.Commit(snapshot); err != nil {
		logger := log.Node()
		logger.Fatal().Err(err).Msg("failed to commit collapsed state to our database")
	}

	l.resetTransactionsSyncIndex()

	logger = log.Sync("apply")
	logger.Info().
		Int("num_chunks", len(sources)).
		Uint64("old_block_index", current.Index).
		Uint64("new_block_index", latest.Index).
		Hex("new_block_id", latest.ID[:]).
		Hex("old_block_id", current.ID[:]).
		Hex("new_merkle_root", latest.Merkle[:]).
		Hex("old_merkle_root", current.Merkle[:]).
		Msg("Successfully built a new state snapshot out of chunk(s) we have received from peers.")

	return true
}

func (l *Ledger) isOutOfSync() bool {
	snowballK := conf.GetSnowballK()
	syncVotes := make(chan Vote, snowballK)
	logger := log.Sync("sync")
	voteWG := new(sync.WaitGroup)

	voteWG.Add(1)

	go CollectVotesForSync(l.accounts, l.syncer, syncVotes, voteWG, snowballK)

	defer func() {
		close(syncVotes)
		voteWG.Wait()
	}()

	for {
		time.Sleep(5 * time.Millisecond)

		peers, err := SelectPeers(l.client.ClosestPeers(), snowballK)
		if err != nil {
			<-time.After(1 * time.Second)

			continue
		}

		current := l.blocks.Latest()

		var wg sync.WaitGroup

		wg.Add(len(peers))

		for _, p := range peers {
			go func(conn *grpc.ClientConn) {
				client := NewWaveletClient(conn)

				ctx, cancel := context.WithTimeout(context.Background(), conf.GetCheckOutOfSyncTimeout())

				p := &peer.Peer{}

				res, err := client.CheckOutOfSync(
					ctx,
					&OutOfSyncRequest{BlockIndex: current.Index},
					grpc.Peer(p),
				)
				if err != nil {
					logger.Error().
						Err(err).
						Msgf("error while checking out of sync with %v", p.Addr)

					cancel()
					wg.Done()
					return
				}

				cancel()

				info := noise.InfoFromPeer(p)
				if info == nil {
					wg.Done()
					return
				}

				voter, ok := info.Get(skademlia.KeyID).(*skademlia.ID)
				if !ok {
					wg.Done()
					return
				}

				syncVotes <- &syncVote{voter: voter, outOfSync: res.OutOfSync}

				wg.Done()
			}(p.Conn())
		}

		wg.Wait()

		if l.syncer.Decided() {
			break
		}
	}

	preferred := l.syncer.Preferred()

	return *preferred.Value().(*bool)
}

// collapseResults is what returned by calling collapseTransactions. Refer to collapseTransactions
// to understand what counts of accepted, rejected, or otherwise ignored transactions truly represent
// after calling collapseTransactions.
type collapseResults struct {
	applied        []*Transaction
	rejected       []*Transaction
	rejectedErrors []error

	appliedCount  int
	rejectedCount int

	snapshot *avl.Tree
}

type CollapseState struct {
	once    sync.Once
	results *collapseResults
	err     error
}

// collapseTransactions takes all transactions recorded within a block, and applies all valid
// and available ones to a snapshot of all accounts stored in the ledger. It returns an updated
// snapshot with all finalized transactions applied, alongside count summaries of the number of
// applied, rejected, or otherwise ignored transactions.
func (l *Ledger) collapseTransactions(
	height uint64, current *Block, proposed []TransactionID, logging bool,
) (*collapseResults, error) {
	var ids []byte

	sh := (*reflect.SliceHeader)(unsafe.Pointer(&ids))
	sh.Data = uintptr(unsafe.Pointer(&proposed[0]))
	sh.Len = len(proposed)
	sh.Cap = len(proposed)

	cacheKey := highwayhash.Sum64(ids, CacheKey)
	state, _ := l.queryStateCache.LoadOrPut(cacheKey, &CollapseState{})

	state.once.Do(func() {
		transactions, err := l.transactions.BatchFind(proposed)
		if err != nil {
			state.err = err
			return
		}

		state.results, state.err = collapseTransactions(height, transactions, current, l.accounts)
	})

	if logging && state.results != nil {
		l.collapseResultsLogger.Log(state.results)
	}

	return state.results, state.err
}

// LogChanges logs all changes made to an AVL tree state snapshot for the purposes
// of logging out changes to account state to Wavelet's HTTP API.
func (l *Ledger) LogChanges(snapshot *avl.Tree, lastBlockIndex uint64) {
	balanceLogger := log.Accounts("balance_updated")
	gasBalanceLogger := log.Accounts("gas_balance_updated")
	stakeLogger := log.Accounts("stake_updated")
	rewardLogger := log.Accounts("reward_updated")
	numPagesLogger := log.Accounts("num_pages_updated")

	balanceKey := append(keyAccounts[:], keyAccountBalance[:]...)
	gasBalanceKey := append(keyAccounts[:], keyAccountContractGasBalance[:]...)
	stakeKey := append(keyAccounts[:], keyAccountStake[:]...)
	rewardKey := append(keyAccounts[:], keyAccountReward[:]...)
	numPagesKey := append(keyAccounts[:], keyAccountContractNumPages[:]...)

	var id AccountID

	snapshot.IterateLeafDiff(lastBlockIndex, func(key, value []byte) bool {
		switch {
		case bytes.HasPrefix(key, balanceKey):
			copy(id[:], key[len(balanceKey):])

			balanceLogger.Log().
				Hex("account_id", id[:]).
				Uint64("balance", binary.LittleEndian.Uint64(value)).
				Msg("")
		case bytes.HasPrefix(key, gasBalanceKey):
			copy(id[:], key[len(gasBalanceKey):])

			gasBalanceLogger.Log().
				Hex("account_id", id[:]).
				Uint64("gas_balance", binary.LittleEndian.Uint64(value)).
				Msg("")
		case bytes.HasPrefix(key, stakeKey):
			copy(id[:], key[len(stakeKey):])

			stakeLogger.Log().
				Hex("account_id", id[:]).
				Uint64("stake", binary.LittleEndian.Uint64(value)).
				Msg("")
		case bytes.HasPrefix(key, rewardKey):
			copy(id[:], key[len(rewardKey):])

			rewardLogger.Log().
				Hex("account_id", id[:]).
				Uint64("reward", binary.LittleEndian.Uint64(value)).
				Msg("")
		case bytes.HasPrefix(key, numPagesKey):
			copy(id[:], key[len(numPagesKey):])

			numPagesLogger.Log().
				Hex("account_id", id[:]).
				Uint64("num_pages", binary.LittleEndian.Uint64(value)).
				Msg("")
		}

		return true
	})
}

func (l *Ledger) SyncStatus() string {
	l.syncStatusLock.RLock()
	defer l.syncStatusLock.RUnlock()

	switch l.syncStatus {
	case outOfSync:
		return "Node is out of sync"
	case synced:
		return "Node is synced, but not taking part in consensus process yet"
	case fullySynced:
		return "Node is fully synced"
	case finalized:
		return "Node is taking part in consensus process"
	default:
		return "Sync status unknown"
	}
}

func (l *Ledger) applySync(flag bitset) {
	l.syncStatusLock.Lock()
	l.syncStatus |= flag
	l.syncStatusLock.Unlock()
}

func (l *Ledger) setSync(flag bitset) {
	l.syncStatusLock.Lock()
	l.syncStatus = flag
	l.syncStatusLock.Unlock()
}

func (l *Ledger) resetTransactionsSyncIndex() {
	l.transactionFilterLock.Lock()
	defer l.transactionFilterLock.Unlock()

	l.transactionFilter.Reset()

	l.transactions.Iterate(func(tx *Transaction) bool {
		l.transactionFilter.Insert(tx.ID)
		return true
	})
}

func (l *Ledger) loadTransactions() error {
	count := 0

	blocks := l.blocks.Clone()
	for _, b := range blocks {
		transactions, err := LoadTransactions(l.db, b.Transactions)
		if err != nil {
			return err
		}

		l.transactions.BatchUnsafeAdd(transactions)

		l.transactionFilterLock.Lock()
		for _, tx := range transactions {
			l.transactionFilter.Insert(tx.ID)
		}
		l.transactionFilterLock.Unlock()

		count += len(transactions)
	}

	if len(blocks) > 0 && count > 0 {
		logger := log.Node()
		logger.Info().
			Int("num_blocks", len(blocks)).
			Int("num_transactions", count).
			Msg("Loaded transactions from db.")
	}

	return nil
}

func (l *Ledger) storeTransactions(stored *Block, evicted *Block) error {
	l.transactions.BatchMarkFinalized(stored.Transactions...)

	txs, _ := l.transactions.BatchFind(stored.Transactions)
	if txs == nil {
		return nil
	}

	if err := StoreTransactions(l.db, txs); err != nil {
		return err
	}

	if evicted != nil {
		return DeleteTransactions(l.db, evicted.Transactions)
	}

	return nil
}

// filterInvalidVotes takes a slice of (*finalizationVote)'s and filters away
// ones that are invalid with respect to the current nodes state.
func (l *Ledger) filterInvalidVotes(current *Block, votes []Vote) {
ValidateVotes:
	for _, vote := range votes {
		vote := vote.(*finalizationVote)

		// Ignore nil block proposals.
		if vote.block == nil {
			continue ValidateVotes
		}

		// Skip validating the block if it has already been validated before.
		if _, exists := l.queryBlockValidCache[vote.block.ID]; exists {
			continue ValidateVotes
		}

		// Ignore block proposals at an unexpected height.
		if vote.block.Index != current.Index+1 {
			vote.block = nil
			continue ValidateVotes
		}

		// Ignore block proposals containing transactions which our node has not
		// locally archived, and mark them as missing.
		if l.transactions.BatchMarkMissing(vote.block.Transactions...) {
			vote.block = nil
			continue ValidateVotes
		}

		transactions, err := l.transactions.BatchFind(vote.block.Transactions)
		if err != nil {
			vote.block = nil
			continue ValidateVotes
		}

		for i := range transactions {
			// Validate the height recorded on transactions inside the block proposal.
			if vote.block.Index >= transactions[i].Block+uint64(conf.GetPruningLimit()) {
				vote.block = nil
				continue ValidateVotes
			}

			if i > 0 { // Filter away block proposals with transaction IDs that are not properly sorted.
				if bytes.Compare(transactions[i-1].ComputeIndex(current.ID), transactions[i].ComputeIndex(current.ID)) >= 0 {
					vote.block = nil
					continue ValidateVotes
				}
			}
		}

		// Derive the Merkle root of the block by cloning the current ledger state, and applying
		// all transactions in the block into the ledger state.
		results, err := l.collapseTransactions(vote.block.Index, current, vote.block.Transactions, false)
		if err != nil {
			logger := log.Node()
			logger.Error().
				Err(err).
				Msg("error collapsing transactions during query")

			vote.block = nil
			continue ValidateVotes
		}

		// Validate the Merkle root recorded on the block with the resultant Merkle root we got
		// from applying all transactions in the block.
		if results.snapshot.Checksum() != vote.block.Merkle {
			vote.block = nil
			continue ValidateVotes
		}

		l.queryBlockValidCache[vote.block.ID] = struct{}{}
	}
}
