// Copyright 2014 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

// Package eth implements the Ethereum protocol.
package eth

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/bor"
	"github.com/ethereum/go-ethereum/consensus/clique"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/bloombits"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state/pruner"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/eth/downloader"
	"github.com/ethereum/go-ethereum/eth/downloader/whitelist"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/eth/filters"
	"github.com/ethereum/go-ethereum/eth/gasprice"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/eth/protocols/snap"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/internal/shutdowncheck"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/miner"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/dnsdisc"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/ethereum/go-ethereum/trie"
)

// Config contains the configuration options of the ETH protocol.
// Deprecated: use ethconfig.Config instead.
type Config = ethconfig.Config

// Ethereum implements the Ethereum full node service.
type Ethereum struct {
	config *ethconfig.Config

	// Handlers
	txPool             *txpool.TxPool
	blockchain         *core.BlockChain
	handler            *handler
	ethDialCandidates  enode.Iterator
	snapDialCandidates enode.Iterator
	merger             *consensus.Merger

	// DB interfaces
	chainDb ethdb.Database // Block chain database

	eventMux       *event.TypeMux
	engine         consensus.Engine
	accountManager *accounts.Manager
	authorized     bool // If consensus engine is authorized with keystore

	bloomRequests     chan chan *bloombits.Retrieval // Channel receiving bloom data retrieval requests
	bloomIndexer      *core.ChainIndexer             // Bloom indexer operating during block imports
	closeBloomHandler chan struct{}

	APIBackend *EthAPIBackend

	miner     *miner.Miner
	gasPrice  *big.Int
	etherbase common.Address

	networkID     uint64
	netRPCService *ethapi.NetAPI

	p2pServer *p2p.Server

	lock sync.RWMutex // Protects the variadic fields (e.g. gas price and etherbase)

	closeCh chan struct{} // Channel to signal the background processes to exit

	shutdownTracker *shutdowncheck.ShutdownTracker // Tracks if and when the node has shutdown ungracefully
}

// New creates a new Ethereum object (including the
// initialisation of the common Ethereum object)
func New(stack *node.Node, config *ethconfig.Config) (*Ethereum, error) {
	// Ensure configuration values are compatible and sane
	if config.SyncMode == downloader.LightSync {
		return nil, errors.New("can't run ethereum.Ethereum in light sync mode, use les.LightEthereum")
	}

	if !config.SyncMode.IsValid() {
		return nil, fmt.Errorf("invalid sync mode %d", config.SyncMode)
	}

	if config.Miner.GasPrice == nil || config.Miner.GasPrice.Cmp(common.Big0) <= 0 {
		log.Warn("Sanitizing invalid miner gas price", "provided", config.Miner.GasPrice, "updated", ethconfig.Defaults.Miner.GasPrice)
		config.Miner.GasPrice = new(big.Int).Set(ethconfig.Defaults.Miner.GasPrice)
	}

	if config.NoPruning && config.TrieDirtyCache > 0 {
		if config.SnapshotCache > 0 {
			config.TrieCleanCache += config.TrieDirtyCache * 3 / 5
			config.SnapshotCache += config.TrieDirtyCache * 2 / 5
		} else {
			config.TrieCleanCache += config.TrieDirtyCache
		}

		config.TrieDirtyCache = 0
	}

	log.Info("Allocated trie memory caches", "clean", common.StorageSize(config.TrieCleanCache)*1024*1024, "dirty", common.StorageSize(config.TrieDirtyCache)*1024*1024)

	// Assemble the Ethereum object
	chainDb, err := stack.OpenDatabaseWithFreezer("chaindata", config.DatabaseCache, config.DatabaseHandles, config.DatabaseFreezer, "ethereum/db/chaindata/", false)
	if err != nil {
		return nil, err
	}

	if err := pruner.RecoverPruning(stack.ResolvePath(""), chainDb, stack.ResolvePath(config.TrieCleanCacheJournal)); err != nil {
		log.Error("Failed to recover state", "error", err)
	}
	// Transfer mining-related config to the ethash config.
	ethashConfig := config.Ethash
	ethashConfig.NotifyFull = config.Miner.NotifyFull
	cliqueConfig, err := core.LoadCliqueConfig(chainDb, config.Genesis)

	if err != nil {
		return nil, err
	}

	// START: Bor changes
	ethereum := &Ethereum{
		config:            config,
		merger:            consensus.NewMerger(chainDb),
		chainDb:           chainDb,
		eventMux:          stack.EventMux(),
		accountManager:    stack.AccountManager(),
		authorized:        false,
		closeBloomHandler: make(chan struct{}),
		networkID:         config.NetworkId,
		gasPrice:          config.Miner.GasPrice,
		etherbase:         config.Miner.Etherbase,
		bloomRequests:     make(chan chan *bloombits.Retrieval),
		bloomIndexer:      core.NewBloomIndexer(chainDb, params.BloomBitsBlocks, params.BloomConfirms),
		p2pServer:         stack.Server(),
		closeCh:           make(chan struct{}),
		shutdownTracker:   shutdowncheck.NewShutdownTracker(chainDb),
	}

	ethereum.APIBackend = &EthAPIBackend{stack.Config().ExtRPCEnabled(), stack.Config().AllowUnprotectedTxs, ethereum, nil}
	if ethereum.APIBackend.allowUnprotectedTxs {
		log.Debug(" ###########", "Unprotected transactions allowed")

		config.TxPool.AllowUnprotectedTxs = true
	}

	gpoParams := config.GPO
	if gpoParams.Default == nil {
		gpoParams.Default = config.Miner.GasPrice
	}

	// Override the chain config with provided settings.
	var overrides core.ChainOverrides
	if config.OverrideShanghai != nil {
		overrides.OverrideShanghai = config.OverrideShanghai
	}

	chainConfig, _, genesisErr := core.SetupGenesisBlockWithOverride(chainDb, trie.NewDatabase(chainDb), config.Genesis, &overrides)
	if _, isCompat := genesisErr.(*params.ConfigCompatError); genesisErr != nil && !isCompat {
		return nil, genesisErr
	}

	blockChainAPI := ethapi.NewBlockChainAPI(ethereum.APIBackend)
	engine := ethconfig.CreateConsensusEngine(stack, chainConfig, config, &ethashConfig, cliqueConfig, config.Miner.Notify, config.Miner.Noverify, chainDb, blockChainAPI)
	ethereum.engine = engine
	// END: Bor changes

	bcVersion := rawdb.ReadDatabaseVersion(chainDb)

	var dbVer = "<nil>"
	if bcVersion != nil {
		dbVer = fmt.Sprintf("%d", *bcVersion)
	}

	log.Info("Initialising Ethereum protocol", "network", config.NetworkId, "dbversion", dbVer)

	if !config.SkipBcVersionCheck {
		if bcVersion != nil && *bcVersion > core.BlockChainVersion {
			return nil, fmt.Errorf("database version is v%d, Geth %s only supports v%d", *bcVersion, params.VersionWithMeta, core.BlockChainVersion)
		} else if bcVersion == nil || *bcVersion < core.BlockChainVersion {
			if bcVersion != nil { // only print warning on upgrade, not on init
				log.Warn("Upgrade blockchain database version", "from", dbVer, "to", core.BlockChainVersion)
			}

			rawdb.WriteDatabaseVersion(chainDb, core.BlockChainVersion)
		}
	}

	var (
		vmConfig = vm.Config{
			EnablePreimageRecording:      config.EnablePreimageRecording,
			ParallelEnable:               config.ParallelEVM.Enable,
			ParallelSpeculativeProcesses: config.ParallelEVM.SpeculativeProcesses,
		}
		cacheConfig = &core.CacheConfig{
			TrieCleanLimit:      config.TrieCleanCache,
			TrieCleanJournal:    stack.ResolvePath(config.TrieCleanCacheJournal),
			TrieCleanRejournal:  config.TrieCleanCacheRejournal,
			TrieCleanNoPrefetch: config.NoPrefetch,
			TrieDirtyLimit:      config.TrieDirtyCache,
			TrieDirtyDisabled:   config.NoPruning,
			TrieTimeLimit:       config.TrieTimeout,
			SnapshotLimit:       config.SnapshotCache,
			Preimages:           config.Preimages,
			TriesInMemory:       config.TriesInMemory,
		}
	)

	checker := whitelist.NewService(10)

	// check if Parallel EVM is enabled
	// if enabled, use parallel state processor
	if config.ParallelEVM.Enable {
		ethereum.blockchain, err = core.NewParallelBlockChain(chainDb, cacheConfig, config.Genesis, &overrides, ethereum.engine, vmConfig, ethereum.shouldPreserve, &config.TxLookupLimit, checker)
	} else {
		ethereum.blockchain, err = core.NewBlockChain(chainDb, cacheConfig, config.Genesis, &overrides, ethereum.engine, vmConfig, ethereum.shouldPreserve, &config.TxLookupLimit, checker)
	}

	ethereum.APIBackend.gpo = gasprice.NewOracle(ethereum.APIBackend, gpoParams)

	if err != nil {
		return nil, err
	}

	_ = ethereum.engine.VerifyHeader(ethereum.blockchain, ethereum.blockchain.CurrentHeader(), true) // TODO think on it

	// BOR changes
	ethereum.APIBackend.gpo.ProcessCache()
	// BOR changes

	ethereum.bloomIndexer.Start(ethereum.blockchain)

	if config.TxPool.Journal != "" {
		config.TxPool.Journal = stack.ResolvePath(config.TxPool.Journal)
	}

	ethereum.txPool = txpool.NewTxPool(config.TxPool, ethereum.blockchain.Config(), ethereum.blockchain)

	// Permit the downloader to use the trie cache allowance during fast sync
	cacheLimit := cacheConfig.TrieCleanLimit + cacheConfig.TrieDirtyLimit + cacheConfig.SnapshotLimit

	checkpoint := config.Checkpoint
	if checkpoint == nil {
		checkpoint = params.TrustedCheckpoints[ethereum.blockchain.Genesis().Hash()]
	}

	if ethereum.handler, err = newHandler(&handlerConfig{
		Database:       chainDb,
		Chain:          ethereum.blockchain,
		TxPool:         ethereum.txPool,
		Merger:         ethereum.merger,
		Network:        config.NetworkId,
		Sync:           config.SyncMode,
		BloomCache:     uint64(cacheLimit),
		EventMux:       ethereum.eventMux,
		Checkpoint:     checkpoint,
		RequiredBlocks: config.RequiredBlocks,
		EthAPI:         blockChainAPI,
		checker:        checker,
		txArrivalWait:  ethereum.p2pServer.TxArrivalWait,
	}); err != nil {
		return nil, err
	}

	ethereum.miner = miner.New(ethereum, &config.Miner, ethereum.blockchain.Config(), ethereum.EventMux(), ethereum.engine, ethereum.isLocalBlock)
	_ = ethereum.miner.SetExtra(makeExtraData(config.Miner.ExtraData))

	// Setup DNS discovery iterators.
	dnsclient := dnsdisc.NewClient(dnsdisc.Config{})

	ethereum.ethDialCandidates, err = dnsclient.NewIterator(ethereum.config.EthDiscoveryURLs...)
	if err != nil {
		return nil, err
	}

	ethereum.snapDialCandidates, err = dnsclient.NewIterator(ethereum.config.SnapDiscoveryURLs...)
	if err != nil {
		return nil, err
	}

	// Start the RPC service
	ethereum.netRPCService = ethapi.NewNetAPI(ethereum.p2pServer, config.NetworkId)

	// Register the backend on the node
	stack.RegisterAPIs(ethereum.APIs())
	stack.RegisterProtocols(ethereum.Protocols())
	stack.RegisterLifecycle(ethereum)

	// Successful startup; push a marker and check previous unclean shutdowns.
	ethereum.shutdownTracker.MarkStartup()

	return ethereum, nil
}

func makeExtraData(extra []byte) []byte {
	if len(extra) == 0 {
		// create default extradata
		extra, _ = rlp.EncodeToBytes([]interface{}{
			uint(params.VersionMajor<<16 | params.VersionMinor<<8 | params.VersionPatch),
			"bor",
			runtime.Version(),
			runtime.GOOS,
		})
	}

	if uint64(len(extra)) > params.MaximumExtraDataSize {
		log.Warn("Miner extra data exceed limit", "extra", hexutil.Bytes(extra), "limit", params.MaximumExtraDataSize)
		extra = nil
	}

	return extra
}

// APIs return the collection of RPC services the ethereum package offers.
// NOTE, some of these services probably need to be moved to somewhere else.
func (s *Ethereum) APIs() []rpc.API {
	apis := ethapi.GetAPIs(s.APIBackend)

	// Append any APIs exposed explicitly by the consensus engine
	apis = append(apis, s.engine.APIs(s.BlockChain())...)

	// BOR change starts
	filterSystem := filters.NewFilterSystem(s.APIBackend, filters.Config{})
	// set genesis to public filter api
	publicFilterAPI := filters.NewFilterAPI(filterSystem, false, s.config.BorLogs)
	// avoiding constructor changed by introducing new method to set genesis
	publicFilterAPI.SetChainConfig(s.blockchain.Config())
	// BOR change ends

	// Append all the local APIs and return
	return append(apis, []rpc.API{
		{
			Namespace: "eth",
			Service:   NewEthereumAPI(s),
		}, {
			Namespace: "miner",
			Service:   NewMinerAPI(s),
		}, {
			Namespace: "eth",
			Service:   publicFilterAPI, // BOR related change
		}, {
			Namespace: "admin",
			Service:   NewAdminAPI(s),
		}, {
			Namespace: "debug",
			Service:   NewDebugAPI(s),
		}, {
			Namespace: "net",
			Service:   s.netRPCService,
		},
	}...)
}

func (s *Ethereum) ResetWithGenesisBlock(gb *types.Block) {
	s.blockchain.ResetWithGenesisBlock(gb)
}

func (s *Ethereum) PublicBlockChainAPI() *ethapi.BlockChainAPI {
	return s.handler.ethAPI
}

func (s *Ethereum) Etherbase() (eb common.Address, err error) {
	s.lock.RLock()
	etherbase := s.etherbase
	s.lock.RUnlock()

	if etherbase != (common.Address{}) {
		return etherbase, nil
	}

	return common.Address{}, fmt.Errorf("etherbase must be explicitly specified")
}

// isLocalBlock checks whether the specified block is mined
// by local miner accounts.
//
// We regard two types of accounts as local miner account: etherbase
// and accounts specified via `txpool.locals` flag.
func (s *Ethereum) isLocalBlock(header *types.Header) bool {
	author, err := s.engine.Author(header)
	if err != nil {
		log.Warn("Failed to retrieve block author", "number", header.Number.Uint64(), "hash", header.Hash(), "err", err)
		return false
	}
	// Check whether the given address is etherbase.
	s.lock.RLock()
	etherbase := s.etherbase
	s.lock.RUnlock()

	if author == etherbase {
		return true
	}
	// Check whether the given address is specified by `txpool.local`
	// CLI flag.
	for _, account := range s.config.TxPool.Locals {
		if account == author {
			return true
		}
	}

	return false
}

// shouldPreserve checks whether we should preserve the given block
// during the chain reorg depending on whether the author of block
// is a local account.
func (s *Ethereum) shouldPreserve(header *types.Header) bool {
	// The reason we need to disable the self-reorg preserving for clique
	// is it can be probable to introduce a deadlock.
	//
	// e.g. If there are 7 available signers
	//
	// r1   A
	// r2     B
	// r3       C
	// r4         D
	// r5   A      [X] F G
	// r6    [X]
	//
	// In the round5, the inturn signer E is offline, so the worst case
	// is A, F and G sign the block of round5 and reject the block of opponents
	// and in the round6, the last available signer B is offline, the whole
	// network is stuck.
	if _, ok := s.engine.(*clique.Clique); ok {
		return false
	}

	return s.isLocalBlock(header)
}

// SetEtherbase sets the mining reward address.
func (s *Ethereum) SetEtherbase(etherbase common.Address) {
	s.lock.Lock()
	s.etherbase = etherbase
	s.lock.Unlock()

	s.miner.SetEtherbase(etherbase)
}

// StartMining starts the miner with the given number of CPU threads. If mining
// is already running, this method adjust the number of threads allowed to use
// and updates the minimum price required by the transaction pool.
func (s *Ethereum) StartMining(threads int) error {
	// Update the thread count within the consensus engine
	type threaded interface {
		SetThreads(threads int)
	}

	if th, ok := s.engine.(threaded); ok {
		log.Info("Updated mining threads", "threads", threads)

		if threads == 0 {
			threads = -1 // Disable the miner from within
		}

		th.SetThreads(threads)
	}
	// If the miner was not running, initialize it
	if !s.IsMining() {
		// Propagate the initial price point to the transaction pool
		s.lock.RLock()
		price := s.gasPrice
		s.lock.RUnlock()
		s.txPool.SetGasPrice(price)

		// Configure the local mining address
		eb, err := s.Etherbase()
		if err != nil {
			log.Error("Cannot start mining without etherbase", "err", err)
			return fmt.Errorf("etherbase missing: %v", err)
		}
		// If personal endpoints are disabled, the server creating
		// this Ethereum instance has already Authorized consensus.
		if !s.authorized {
			var cli *clique.Clique
			if c, ok := s.engine.(*clique.Clique); ok {
				cli = c
			} else if cl, ok := s.engine.(*beacon.Beacon); ok {
				if c, ok := cl.InnerEngine().(*clique.Clique); ok {
					cli = c
				}
			}

			if cli != nil {
				wallet, err := s.accountManager.Find(accounts.Account{Address: eb})
				if wallet == nil || err != nil {
					log.Error("Etherbase account unavailable locally", "err", err)
					return fmt.Errorf("signer missing: %v", err)
				}

				cli.Authorize(eb, wallet.SignData)
			}

			if bor, ok := s.engine.(*bor.Bor); ok {
				wallet, err := s.accountManager.Find(accounts.Account{Address: eb})
				if wallet == nil || err != nil {
					log.Error("Etherbase account unavailable locally", "err", err)

					return fmt.Errorf("signer missing: %v", err)
				}

				bor.Authorize(eb, wallet.SignData)
			}
		}

		// If mining is started, we can disable the transaction rejection mechanism
		// introduced to speed sync times.
		atomic.StoreUint32(&s.handler.acceptTxs, 1)

		go s.miner.Start()
	}

	return nil
}

// StopMining terminates the miner, both at the consensus engine level as well as
// at the block creation level.
func (s *Ethereum) StopMining() {
	// Update the thread count within the consensus engine
	type threaded interface {
		SetThreads(threads int)
	}

	if th, ok := s.engine.(threaded); ok {
		th.SetThreads(-1)
	}
	// Stop the block creating itself
	s.miner.Stop()
}

func (s *Ethereum) IsMining() bool      { return s.miner.Mining() }
func (s *Ethereum) Miner() *miner.Miner { return s.miner }

func (s *Ethereum) AccountManager() *accounts.Manager  { return s.accountManager }
func (s *Ethereum) BlockChain() *core.BlockChain       { return s.blockchain }
func (s *Ethereum) TxPool() *txpool.TxPool             { return s.txPool }
func (s *Ethereum) EventMux() *event.TypeMux           { return s.eventMux }
func (s *Ethereum) Engine() consensus.Engine           { return s.engine }
func (s *Ethereum) ChainDb() ethdb.Database            { return s.chainDb }
func (s *Ethereum) IsListening() bool                  { return true } // Always listening
func (s *Ethereum) Downloader() *downloader.Downloader { return s.handler.downloader }
func (s *Ethereum) Synced() bool                       { return atomic.LoadUint32(&s.handler.acceptTxs) == 1 }
func (s *Ethereum) SetSynced()                         { atomic.StoreUint32(&s.handler.acceptTxs, 1) }
func (s *Ethereum) ArchiveMode() bool                  { return s.config.NoPruning }
func (s *Ethereum) BloomIndexer() *core.ChainIndexer   { return s.bloomIndexer }
func (s *Ethereum) Merger() *consensus.Merger          { return s.merger }
func (s *Ethereum) SyncMode() downloader.SyncMode {
	mode, _ := s.handler.chainSync.modeAndLocalHead()
	return mode
}

// SetAuthorized sets the authorized bool variable
// denoting that consensus has been authorized while creation
func (s *Ethereum) SetAuthorized(authorized bool) {
	s.lock.Lock()
	s.authorized = authorized
	s.lock.Unlock()
}

// Protocols returns all the currently configured
// network protocols to start.
func (s *Ethereum) Protocols() []p2p.Protocol {
	protos := eth.MakeProtocols((*ethHandler)(s.handler), s.networkID, s.ethDialCandidates)
	if s.config.SnapshotCache > 0 {
		protos = append(protos, snap.MakeProtocols((*snapHandler)(s.handler), s.snapDialCandidates)...)
	}

	return protos
}

// Start implements node.Lifecycle, starting all internal goroutines needed by the
// Ethereum protocol implementation.
func (s *Ethereum) Start() error {
	eth.StartENRUpdater(s.blockchain, s.p2pServer.LocalNode())

	// Start the bloom bits servicing goroutines
	s.startBloomHandlers(params.BloomBitsBlocks)

	// Regularly update shutdown marker
	s.shutdownTracker.Start()

	// Figure out a max peers count based on the server limits
	maxPeers := s.p2pServer.MaxPeers

	if s.config.LightServ > 0 {
		if s.config.LightPeers >= s.p2pServer.MaxPeers {
			return fmt.Errorf("invalid peer config: light peer count (%d) >= total peer count (%d)", s.config.LightPeers, s.p2pServer.MaxPeers)
		}

		maxPeers -= s.config.LightPeers
	}

	// Start the networking layer and the light server if requested
	s.handler.Start(maxPeers)

	go s.startCheckpointWhitelistService()

	return nil
}

var (
	ErrNotBorConsensus             = errors.New("not bor consensus was given")
	ErrBorConsensusWithoutHeimdall = errors.New("bor consensus without heimdall")

	whitelistTimeout = 30 * time.Second
)

// StartCheckpointWhitelistService starts the goroutine to fetch checkpoints and update the
// checkpoint whitelist map.
func (s *Ethereum) startCheckpointWhitelistService() {
	// a shortcut helps with tests and early exit
	select {
	case <-s.closeCh:
		return
	default:
	}

	// first run the checkpoint whitelist
	firstCtx, cancel := context.WithTimeout(context.Background(), whitelistTimeout)
	err := s.handleWhitelistCheckpoint(firstCtx, true)

	cancel()

	if err != nil {
		if errors.Is(err, ErrBorConsensusWithoutHeimdall) || errors.Is(err, ErrNotBorConsensus) {
			return
		}

		log.Warn("unable to whitelist checkpoint - first run", "err", err)
	}

	ticker := time.NewTicker(100 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), whitelistTimeout)
			err := s.handleWhitelistCheckpoint(ctx, false)

			cancel()

			if err != nil {
				log.Warn("unable to whitelist checkpoint", "err", err)
			}
		case <-s.closeCh:
			return
		}
	}
}

// handleWhitelistCheckpoint handles the checkpoint whitelist mechanism.
func (s *Ethereum) handleWhitelistCheckpoint(ctx context.Context, first bool) error {
	ethHandler := (*ethHandler)(s.handler)

	bor, ok := ethHandler.chain.Engine().(*bor.Bor)
	if !ok {
		return ErrNotBorConsensus
	}

	if bor.HeimdallClient == nil {
		return ErrBorConsensusWithoutHeimdall
	}

	// Create a new checkpoint verifier
	verifier := newCheckpointVerifier(nil)
	blockNums, blockHashes, err := ethHandler.fetchWhitelistCheckpoints(ctx, bor, verifier, first)
	// If the array is empty, we're bound to receive an error. Non-nill error and non-empty array
	// means that array has partial elements and it failed for some block. We'll add those partial
	// elements anyway.
	if len(blockNums) == 0 {
		return err
	}

	// Update the checkpoint whitelist map.
	for i := 0; i < len(blockNums); i++ {
		ethHandler.downloader.ProcessCheckpoint(blockNums[i], blockHashes[i])
	}

	return nil
}

// Stop implements node.Lifecycle, terminating all internal goroutines used by the
// Ethereum protocol.
func (s *Ethereum) Stop() error {
	// Stop all the peer-related stuff first.
	s.ethDialCandidates.Close()
	s.snapDialCandidates.Close()
	s.handler.Stop()

	// Then stop everything else.
	s.bloomIndexer.Close()
	close(s.closeBloomHandler)

	// Close all bg processes
	close(s.closeCh)

	// closing consensus engine first, as miner has deps on it
	s.engine.Close()
	s.txPool.Stop()
	s.miner.Close()
	s.blockchain.Stop()
	s.engine.Close()

	// Clean shutdown marker as the last thing before closing db
	s.shutdownTracker.Stop()

	s.chainDb.Close()
	s.eventMux.Stop()

	return nil
}

//
// Bor related methods
//

// SetBlockchain set blockchain while testing
func (s *Ethereum) SetBlockchain(blockchain *core.BlockChain) {
	s.blockchain = blockchain
}
