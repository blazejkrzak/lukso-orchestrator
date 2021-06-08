package node

import (
	"context"
	"github.com/ethereum/go-ethereum/common/math"
	ethRpc "github.com/ethereum/go-ethereum/rpc"
	"github.com/lukso-network/lukso-orchestrator/orchestrator/cache"
	"github.com/lukso-network/lukso-orchestrator/orchestrator/consensus"
	"github.com/lukso-network/lukso-orchestrator/orchestrator/db"
	"github.com/lukso-network/lukso-orchestrator/orchestrator/db/kv"
	"github.com/lukso-network/lukso-orchestrator/orchestrator/pandorachain"
	"github.com/lukso-network/lukso-orchestrator/orchestrator/rpc"
	"github.com/lukso-network/lukso-orchestrator/orchestrator/vanguardchain"
	"github.com/lukso-network/lukso-orchestrator/orchestrator/vanguardchain/client"
	"github.com/lukso-network/lukso-orchestrator/shared"
	"github.com/lukso-network/lukso-orchestrator/shared/cmd"
	"github.com/lukso-network/lukso-orchestrator/shared/fileutil"
	"github.com/lukso-network/lukso-orchestrator/shared/types"
	"github.com/lukso-network/lukso-orchestrator/shared/version"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// OrchestratorNode
type OrchestratorNode struct {
	// basic configuration
	cliCtx *cli.Context
	ctx    context.Context
	cancel context.CancelFunc

	// service storage
	services *shared.ServiceRegistry
	lock     sync.RWMutex
	stop     chan struct{} // Channel to wait for termination notifications.

	//kv database with cache
	db db.Database
}

// New creates a new node instance, sets up configuration options, and registers
// every required service to the node.
func New(cliCtx *cli.Context) (*OrchestratorNode, error) {
	registry := shared.NewServiceRegistry()
	ctx, cancel := context.WithCancel(cliCtx.Context)
	orchestrator := &OrchestratorNode{
		cliCtx:   cliCtx,
		ctx:      ctx,
		cancel:   cancel,
		services: registry,
		stop:     make(chan struct{}),
	}

	if err := orchestrator.startDB(orchestrator.cliCtx); err != nil {
		return nil, err
	}

	if err := orchestrator.registerVanguardChainService(cliCtx); err != nil {
		return nil, err
	}

	if err := orchestrator.registerPandoraChainService(cliCtx); err != nil {
		return nil, err
	}

	if err := orchestrator.registerRPCService(cliCtx); err != nil {
		return nil, err
	}

	// Register consensus service only after vanguard notified about consensus info and blocks
	go orchestrator.registerConsensusService(cliCtx)

	return orchestrator, nil
}

// startDB initialize KV db and cache
func (o *OrchestratorNode) startDB(cliCtx *cli.Context) error {
	baseDir := cliCtx.String(cmd.DataDirFlag.Name)
	dbPath := filepath.Join(baseDir, kv.OrchestratorNodeDbDirName)
	clearDB := cliCtx.Bool(cmd.ClearDB.Name)
	forceClearDB := cliCtx.Bool(cmd.ForceClearDB.Name)

	log.WithField("database-path", dbPath).Info("Checking DB")

	d, err := db.NewDB(o.ctx, dbPath, &kv.Config{
		InitialMMapSize: cliCtx.Int(cmd.BoltMMapInitialSizeFlag.Name),
	})
	if err != nil {
		return err
	}

	clearDBConfirmed := false
	if clearDB && !forceClearDB {
		actionText := "This will delete your orchestrator database stored in your data directory. " +
			"Your database backups will not be removed - do you want to proceed? (Y/N)"
		deniedText := "Database will not be deleted. No changes have been made."
		clearDBConfirmed, err = cmd.ConfirmAction(actionText, deniedText)
		if err != nil {
			return err
		}
	}

	if clearDBConfirmed || forceClearDB {
		log.Warning("Removing database")
		if err := d.Close(); err != nil {
			return errors.Wrap(err, "could not close db prior to clearing")
		}
		if err := d.ClearDB(); err != nil {
			return errors.Wrap(err, "could not clear database")
		}
		d, err = db.NewDB(o.ctx, dbPath, &kv.Config{
			InitialMMapSize: cliCtx.Int(cmd.BoltMMapInitialSizeFlag.Name),
		})
		if err != nil {
			return errors.Wrap(err, "could not create new database")
		}
	}

	o.db = d
	return nil
}

// registerVanguardChainService
func (o *OrchestratorNode) registerVanguardChainService(cliCtx *cli.Context) error {
	vanguardGRPCUrl := cliCtx.String(cmd.VanguardGRPCEndpoint.Name)
	dialGRPCClient := vanguardchain.DIALGRPCFn(func(endpoint string) (client.VanguardClient, error) {
		return client.Dial(o.ctx, endpoint, time.Minute*6, 32, math.MaxInt32)
	})
	svc, err := vanguardchain.NewService(
		o.ctx,
		vanguardGRPCUrl,
		o.db,
		dialGRPCClient,
	)
	if err != nil {
		return nil
	}
	log.WithField("vanguardGRPCUrl", vanguardGRPCUrl).Info("Registered vanguard chain service")
	return o.services.RegisterService(svc)
}

// registerPandoraChainService
func (o *OrchestratorNode) registerPandoraChainService(cliCtx *cli.Context) error {
	pandoraRPCUrl := cliCtx.String(cmd.PandoraRPCEndpoint.Name)
	dialRPCClient := func(endpoint string) (*ethRpc.Client, error) {
		rpcClient, err := ethRpc.Dial(endpoint)
		if err != nil {
			return nil, err
		}
		return rpcClient, nil
	}
	namespace := "eth"
	panCache := cache.NewPanHeaderCache()
	svc, err := pandorachain.NewService(o.ctx, pandoraRPCUrl, namespace, o.db, panCache, dialRPCClient)
	if err != nil {
		return nil
	}
	log.WithField("pandoraHttpUrl", pandoraRPCUrl).Info("Registered pandora chain service")
	return o.services.RegisterService(svc)
}

func (o *OrchestratorNode) registerConsensusService(
	cliCtx *cli.Context,
) {
	var (
		vanguardChain             *vanguardchain.Service
		pandoraChain              *pandorachain.Service
		vanguardHeadersChan       chan *types.HeaderHash
		vanguardConsensusInfoChan chan *types.MinimalEpochConsensusInfo
		//pandoraHeadersChan        chan *types.HeaderHash
	)

	vanguardHeadersChan = make(chan *types.HeaderHash, 100000)
	//pandoraHeadersChan = make(chan *types.HeaderHash, 100000)
	vanguardConsensusInfoChan = make(chan *types.MinimalEpochConsensusInfo, 100000)

	services := o.services
	err := services.FetchService(&vanguardChain)

	if err != nil {
		log.WithField("err", err).Error("Consensus service not registered")

		return
	}

	err = services.FetchService(&pandoraChain)

	if err != nil {
		log.WithField("err", err).Error("Consensus service not registered")

		return
	}

	vanguardChain.SubscribeVanNewPendingBlockHash(vanguardHeadersChan)
	vanguardChain.SubscribeMinConsensusInfoEvent(vanguardConsensusInfoChan)

	// TODO: add 3 instead of 2 (also pandora chain should notify about availability)
	vanguardHeaderLock := true
	vanguardConsensusLock := true

	// TODO: resolve synchronization. Do not start when still receiving data.
	//  This should prevent data availability failure
	for {
		select {
		case <-vanguardHeadersChan:
			if !vanguardHeaderLock {
				continue
			}

			vanguardHeaderLock = false
			log.WithField("context", "vanguardHeadersChan").
				Info("received signal about vanguard headers")
		case <-vanguardConsensusInfoChan:
			if !vanguardConsensusLock {
				continue
			}

			vanguardConsensusLock = false
			log.WithField("context", "vanguardHeadersChan").
				Info("received signal about vanguard consensus info")
		}

		// Leave the loop
		if !vanguardHeaderLock && !vanguardConsensusLock {
			log.WithField("context", "consensus service lock").
				Info("unchained all locks")

			break
		}
	}

	svc := consensus.New(cliCtx.Context, o.db)
	err = o.services.RegisterService(svc)

	if nil != err {
		log.WithField("err", err).Error("Consensus service not registered")
	}
}

// register RPC server
func (o *OrchestratorNode) registerRPCService(cliCtx *cli.Context) error {
	var consensusInfoFeed *vanguardchain.Service
	if err := o.services.FetchService(&consensusInfoFeed); err != nil {
		return err
	}

	var ipcapiURL string
	if cliCtx.String(cmd.IPCPathFlag.Name) != "" {
		ipcFilePath := cliCtx.String(cmd.IPCPathFlag.Name)
		ipcapiURL = fileutil.IpcEndpoint(filepath.Join(ipcFilePath, cmd.DefaultIpcPath), "")

		log.WithField("ipcFilePath", ipcFilePath).WithField(
			"ipcPath", ipcapiURL).Info("ipc file path")
	}

	httpEnable := cliCtx.Bool(cmd.HTTPEnabledFlag.Name)
	httpListenAddr := cliCtx.String(cmd.HTTPListenAddrFlag.Name)
	httpPort := cliCtx.Int(cmd.HTTPPortFlag.Name)
	wsEnable := cliCtx.Bool(cmd.WSEnabledFlag.Name)
	wsListenerAddr := cliCtx.String(cmd.WSListenAddrFlag.Name)
	wsPort := cliCtx.Int(cmd.WSPortFlag.Name)

	log.WithField("httpEnable", httpEnable).WithField("httpListenAddr", httpListenAddr).WithField(
		"httpPort", httpPort).WithField("wsEnable", wsEnable).WithField(
		"wsListenerAddr", wsListenerAddr).WithField("wsPort", wsPort).Debug("rpc server configuration")

	svc, err := rpc.NewService(o.ctx, &rpc.Config{
		ConsensusInfoFeed: consensusInfoFeed,
		Db:                o.db,
		IPCPath:           ipcapiURL,
		HTTPEnable:        httpEnable,
		HTTPHost:          httpListenAddr,
		HTTPPort:          httpPort,
		WSEnable:          wsEnable,
		WSHost:            wsListenerAddr,
		WSPort:            wsPort,
	})
	if err != nil {
		return nil
	}

	log.Info("Registered RPC service")
	return o.services.RegisterService(svc)
}

// Start the OrchestratorNode and kicks off every registered service.
func (o *OrchestratorNode) Start() {
	o.lock.Lock()

	log.WithFields(logrus.Fields{
		"version": version.Version(),
	}).Info("Starting orchestrator node")

	o.services.StartAll()

	stop := o.stop
	o.lock.Unlock()

	go func() {
		sigc := make(chan os.Signal, 1)
		signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
		defer signal.Stop(sigc)
		<-sigc
		log.Info("Got interrupt, shutting down...")
		go o.Close()
		for i := 10; i > 0; i-- {
			<-sigc
			if i > 1 {
				log.WithField("times", i-1).Info("Already shutting down, interrupt more to panic")
			}
		}
		panic("Panic closing the orchestrator node")
	}()

	// Wait for stop channel to be closed.
	<-stop
}

// Close handles graceful shutdown of the system.
func (b *OrchestratorNode) Close() {
	b.lock.Lock()
	defer b.lock.Unlock()

	log.Info("Stopping orchestrator node")
	b.services.StopAll()
	b.cancel()
	close(b.stop)
}

// This will trigger handler only after certain timeout
func Debounce(
	ctx cli.Context,
	interval time.Duration,
	eventsChan <-chan interface{},
	handler func(interface{}),
) {
	for event := range eventsChan {
	loop:
		for {
			timer := time.NewTimer(interval)

			select {
			case event = <-eventsChan:
			case <-timer.C:
				handler(event)
				break loop
			case <-ctx.Done():
				timer.Stop()
				return
			}

			timer.Stop()
		}
	}
}
