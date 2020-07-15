package connmanager

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/kaspanet/kaspad/dnsseed"
	"github.com/kaspanet/kaspad/wire"

	"github.com/kaspanet/kaspad/addrmgr"
	"github.com/kaspanet/kaspad/netadapter"

	"github.com/kaspanet/kaspad/config"
)

// connectionRequest represents a user request (either through CLI or RPC) to connect to a certain node
type connectionRequest struct {
	address       string
	isPermanent   bool
	nextAttempt   time.Time
	retryDuration time.Duration
}

// ConnectionManager monitors that the current active connections satisfy the requirements of
// outgoing, requested and incoming connections
type ConnectionManager struct {
	netAdapter     *netadapter.NetAdapter
	addressManager *addrmgr.AddrManager

	activeRequested  map[string]*connectionRequest
	pendingRequested map[string]*connectionRequest
	activeOutgoing   map[string]struct{}
	targetOutgoing   int
	activeIncoming   map[string]struct{}
	maxIncoming      int

	stop                   uint32
	connectionRequestsLock sync.Mutex
}

// New instantiates a new instance of a ConnectionManager
func New(netAdapter *netadapter.NetAdapter, addressManager *addrmgr.AddrManager) (*ConnectionManager, error) {
	c := &ConnectionManager{
		netAdapter:       netAdapter,
		addressManager:   addressManager,
		activeRequested:  map[string]*connectionRequest{},
		pendingRequested: map[string]*connectionRequest{},
		activeOutgoing:   map[string]struct{}{},
		activeIncoming:   map[string]struct{}{},
	}

	cfg := config.ActiveConfig()
	connectPeers := cfg.AddPeers
	if len(cfg.ConnectPeers) > 0 {
		connectPeers = cfg.ConnectPeers
	}

	c.maxIncoming = cfg.MaxInboundPeers
	c.targetOutgoing = cfg.TargetOutboundPeers

	for _, connectPeer := range connectPeers {
		c.pendingRequested[connectPeer] = &connectionRequest{
			address:     connectPeer,
			isPermanent: true,
		}
	}

	return c, nil
}

// Start begins the operation of the ConnectionManager
func (c *ConnectionManager) Start() {
	cfg := config.ActiveConfig()
	if !cfg.DisableDNSSeed {
		dnsseed.SeedFromDNS(cfg.NetParams(), wire.SFNodeNetwork, false, nil,
			config.ActiveConfig().Lookup, func(addrs []*wire.NetAddress) {
				// Kaspad uses a lookup of the dns seeder here. Since seeder returns
				// IPs of nodes and not its own IP, we can not know real IP of
				// source. So we'll take first returned address as source.
				c.addressManager.AddAddresses(addrs, addrs[0], nil)
			})
	}

	spawn(c.connectionsLoop)
}

// Stop halts the operation of the ConnectionManager
func (c *ConnectionManager) Stop() {
	atomic.StoreUint32(&c.stop, 1)

	for _, connection := range c.netAdapter.Connections() {
		_ = connection.Disconnect() // Ignore errors since connection might be in the midst of disconnecting
	}
}

const connectionsLoopInterval = 30 * time.Second

func (c *ConnectionManager) initiateConnection(address string) error {
	log.Infof("Connecting to %s", address)
	_, err := c.netAdapter.Connect(address)
	if err != nil {
		log.Infof("Couldn't connect to %s: %s", address, err)
	}
	return err
}

func (c *ConnectionManager) connectionsLoop() {
	for atomic.LoadUint32(&c.stop) == 0 {
		connections := c.netAdapter.Connections()

		// We convert the connections list to a set, so that connections can be found quickly
		// Then we go over the set, classifying connection by category: requested, outgoing or incoming
		// Every step removes all matching connections so that once we get to checkIncomingConnections -
		// the only connections left are the incoming ones
		connSet := convertToSet(connections)

		c.checkRequestedConnections(connSet)

		c.checkOutgoingConnections(connSet)

		c.checkIncomingConnections(connSet)

		<-time.Tick(connectionsLoopInterval)
	}
}

// checkIncomingConnections makes sure there's no more then maxIncoming incoming connections
// if there are - it randomly disconnects enough to go below that number
func (c *ConnectionManager) checkIncomingConnections(connSet connectionSet) {
	if len(connSet) <= c.maxIncoming {
		return
	}

	// randomly disconnect nodes until the number of incoming connections is smaller the maxIncoming
	for address, connection := range connSet {
		err := connection.Disconnect()
		if err != nil {
			log.Errorf("Error disconnecting from %s: %+v", address, err)
		}

		connSet.remove(connection)
	}
}
