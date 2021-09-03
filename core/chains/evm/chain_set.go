package evm

import (
	"math"
	"math/big"

	"github.com/pkg/errors"
	"github.com/smartcontractkit/sqlx"
	"go.uber.org/multierr"
	"gorm.io/gorm"

	"github.com/smartcontractkit/chainlink/core/chains/evm/types"
	"github.com/smartcontractkit/chainlink/core/logger"
	"github.com/smartcontractkit/chainlink/core/service"
	"github.com/smartcontractkit/chainlink/core/services/bulletprooftxmanager"
	"github.com/smartcontractkit/chainlink/core/services/eth"
	httypes "github.com/smartcontractkit/chainlink/core/services/headtracker/types"
	"github.com/smartcontractkit/chainlink/core/services/keystore"
	"github.com/smartcontractkit/chainlink/core/services/log"
	"github.com/smartcontractkit/chainlink/core/services/postgres"
	"github.com/smartcontractkit/chainlink/core/store/config"
	"github.com/smartcontractkit/chainlink/core/utils"
)

var ErrNoChains = errors.New("no chains loaded, are you running with EVM_DISABLED=true ?")

var _ ChainSet = &chainSet{}

//go:generate mockery --name ChainSet --output ./mocks/ --case=underscore
type ChainSet interface {
	service.Service
	Get(id *big.Int) (Chain, error)
	Default() (Chain, error)
	Configure(id *big.Int, enabled bool, config types.ChainCfg) (types.Chain, error)
	Chains() []Chain
	ChainCount() int
	ORM() types.ORM
}

type chainSet struct {
	defaultID *big.Int
	chains    map[string]*chain
	logger    *logger.Logger
	orm       types.ORM
	opts      ChainSetOpts
}

func (cll *chainSet) Start() (err error) {
	for _, c := range cll.Chains() {
		err = multierr.Combine(err, c.Start())
	}
	cll.logger.Infof("EVM: Started %d chains, default chain ID is %d", len(cll.chains), cll.defaultID)
	return
}
func (cll *chainSet) Close() (err error) {
	cll.logger.Debug("EVM: stopping")
	for _, c := range cll.Chains() {
		err = multierr.Combine(err, c.Close())
	}
	return
}
func (cll *chainSet) Healthy() (err error) {
	for _, c := range cll.Chains() {
		err = multierr.Combine(err, c.Healthy())
	}
	return
}
func (cll *chainSet) Ready() (err error) {
	for _, c := range cll.Chains() {
		err = multierr.Combine(err, c.Ready())
	}
	return
}

func (cll *chainSet) Get(id *big.Int) (Chain, error) {
	if id == nil {
		cll.logger.Debugf("Chain ID not specified, using default: %s", cll.defaultID.String())
		return cll.Default()
	}
	c, exists := cll.chains[id.String()]
	if exists {
		return c, nil
	}
	return nil, errors.Errorf("chain not found with id %d", id)
}

func (cll *chainSet) Default() (Chain, error) {
	if len(cll.chains) == 0 {
		return nil, ErrNoChains
	}
	if cll.defaultID == nil {
		return nil, errors.New("no default chain ID specified")
	}

	return cll.Get(cll.defaultID)
}

func (cll *chainSet) Configure(id *big.Int, enabled bool, config types.ChainCfg) (types.Chain, error) {
	// Update configuration stored in the database
	bid := utils.NewBig(id)
	dbchain, err := cll.orm.UpdateChain(*bid, enabled, config)
	if err != nil {
		return types.Chain{}, err
	}
	// TODO: replace with math.MaxInt once we make go 1.17 mandatory
	nodes, _, err := cll.orm.NodesForChain(*bid, 0, math.MaxInt16)
	if err != nil {
		return types.Chain{}, err
	}
	dbchain.Nodes = nodes

	// TODO: the rest of this call likely needs to be synchronized?
	chain, err := cll.Get(id)
	exists := err == nil
	cid := id.String()

	switch {
	case exists && !enabled:
		// Chain was toggled to disabled
		delete(cll.chains, cid)
		return types.Chain{}, chain.Close()
	case !exists && enabled:
		// Chain was toggled to enabled
		chain, err := newChain(dbchain, cll.opts)
		if errors.Cause(err) == ErrNoPrimaryNode {
			cll.logger.Warnf("EVM: No primary node found for chain %s; this chain will be ignored", cid)
		} else if err != nil {
			return types.Chain{}, err
		}
		if err = chain.Start(); err != nil {
			return types.Chain{}, err
		}
		cll.chains[cid] = chain
		return dbchain, nil
	}

	return dbchain, nil
}

func (cll *chainSet) Chains() (c []Chain) {
	for _, chain := range cll.chains {
		c = append(c, chain)
	}
	return c
}

func (cll *chainSet) ChainCount() int {
	return len(cll.chains)
}

func (cll *chainSet) ORM() types.ORM {
	return cll.orm
}

type ChainSetOpts struct {
	Config           config.GeneralConfig
	Logger           *logger.Logger
	GormDB           *gorm.DB
	SQLxDB           *sqlx.DB
	KeyStore         keystore.Eth
	AdvisoryLocker   postgres.AdvisoryLocker
	EventBroadcaster postgres.EventBroadcaster
	ORM              types.ORM

	// Gen-functions are useful for dependency injection by tests
	GenEthClient      func(types.Chain) eth.Client
	GenLogBroadcaster func(types.Chain) log.Broadcaster
	GenHeadTracker    func(types.Chain) httypes.Tracker
	GenTxManager      func(types.Chain) bulletprooftxmanager.TxManager
}

func LoadChainSet(opts ChainSetOpts) (ChainSet, error) {
	if err := checkOpts(&opts); err != nil {
		return nil, err
	}
	if opts.Config.EVMDisabled() {
		opts.Logger.Info("EVM is disabled, no chains will be loaded")
		return &chainSet{orm: opts.ORM, logger: opts.Logger}, nil
	}
	dbchains, err := opts.ORM.EnabledChainsWithNodes()
	if err != nil {
		return nil, errors.Wrap(err, "error loading chains")
	}
	return NewChainSet(opts, dbchains)
}

func NewChainSet(opts ChainSetOpts, dbchains []types.Chain) (ChainSet, error) {
	if err := checkOpts(&opts); err != nil {
		return nil, err
	}
	opts.Logger.Infof("Creating ChainSet with default chain id: %v and number of chains: %v", opts.Config.DefaultChainID(), len(dbchains))
	var err error
	cll := &chainSet{opts.Config.DefaultChainID(), make(map[string]*chain), opts.Logger, opts.ORM, opts}
	for i := range dbchains {
		cid := dbchains[i].ID.String()
		opts.Logger.Infof("EVM: Loading chain %s", cid)
		chain, err2 := newChain(dbchains[i], opts)
		if err2 != nil {
			if errors.Cause(err2) == ErrNoPrimaryNode {
				opts.Logger.Warnf("EVM: No primary node found for chain %s; this chain will be ignored", cid)
			} else {
				err = multierr.Combine(err, err2)
			}
			continue
		}
		if _, exists := cll.chains[cid]; exists {
			return nil, errors.Errorf("duplicate chain with ID %s", cid)
		}
		cll.chains[cid] = chain
	}
	return cll, err
}

func checkOpts(opts *ChainSetOpts) error {
	if opts.Logger == nil {
		return errors.New("logger must be non-nil")
	}
	if opts.Config == nil {
		return errors.New("config must be non-nil")
	}
	if opts.ORM == nil {
		opts.ORM = NewORM(opts.SQLxDB)
	}
	return nil
}
