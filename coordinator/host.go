package coordinator

import (
	"context"
	"log"
	"net"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/net/swarm"
	libp2pclient "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/pkg/errors"
	"perun.network/go-perun/channel/multi"
	"perun.network/go-perun/wallet"
)

// defaultCoordinateTimeout caps a single coordinate() call (subscribe + wait +
// on-chain coordinate tx + confirmation). It must accommodate the longest
// expected ChallengeDuration plus concludeWaitSlack (12 s on the ETH backend)
// plus a generous block-mining margin.
const defaultCoordinateTimeout = 5 * time.Minute

type CoordinatorHost struct {
	acc         map[wallet.BackendID]wallet.Account
	host        host.Host
	reservation *libp2pclient.Reservation
	closer      context.CancelFunc
	ctx         context.Context

	registry    *registry
	coordinator *multi.Coordinator

	// coordinateTimeout caps each in-flight coordinate() call.
	// Defaults to defaultCoordinateTimeout; tests may override before use.
	coordinateTimeout time.Duration
	// coordWg tracks every in-flight coordinate() goroutine so Wait can drain
	// them cleanly before backend shutdown (otherwise SubscribeNewHead inside
	// ConfirmTransaction can be torn down mid-call, producing a nil receipt
	// dereference in the ETH backend).
	coordWg sync.WaitGroup

	relayMu   sync.Mutex
	relayInfo *peer.AddrInfo
}

// Wait blocks until all in-flight coordinate() calls finish or timeout elapses.
// Callers (tests, graceful-shutdown paths) should invoke this before tearing
// down the underlying chain backend.
func (c *CoordinatorHost) Wait(timeout time.Duration) error {
	done := make(chan struct{})
	go func() {
		c.coordWg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-time.After(timeout):
		return errors.New("CoordinatorHost.Wait: timeout waiting for in-flight coordinate calls")
	}
}

// coordinateDeadline returns the configured per-coordinate timeout, falling
// back to the package default for zero-valued hosts (e.g. tests constructing
// CoordinatorHost directly without SetupRelayCoordinator).
func (c *CoordinatorHost) coordinateDeadline() time.Duration {
	if c.coordinateTimeout > 0 {
		return c.coordinateTimeout
	}
	return defaultCoordinateTimeout
}

//	SetupRelayCoordinator initializes a libp2p host with relay capabilities and an address book.
//
// It listens on the specified port and uses the provided key file for identity.
func SetupRelayCoordinator(privKey crypto.PrivKey, acc map[wallet.BackendID]wallet.Account, coordinator *multi.Coordinator) (*CoordinatorHost, error) {
	relayInfo, _, err := getRelayServerInfo()
	if err != nil {
		return nil, errors.WithMessage(err, "getting relay server info")
	}

	h, err := libp2p.New(
		libp2p.NoListenAddrs,
		libp2p.Identity(privKey),
		libp2p.EnableRelay(),
	)
	if err != nil {
		return nil, err
	}

	// Redialing hacked
	if sw, ok := h.Network().(*swarm.Swarm); ok {
		sw.Backoff().Clear(relayInfo.ID)
	}

	ctx, cancel := context.WithCancel(context.Background())

	err = h.Connect(ctx, *relayInfo)
	if err != nil {
		cancel()
		_ = h.Close()
		return nil, errors.WithMessage(err, "connecting to relay server")
	}

	// Reserve connection
	// Hosts that want to have messages relayed on their behalf need to reserve a slot
	// with the circuit relay service host
	resv, err := libp2pclient.Reserve(ctx, h, *relayInfo)
	if err != nil {
		cancel()
		_ = h.Close()
		return nil, errors.WithMessage(err, "reserving relay slot")
	}

	c := &CoordinatorHost{
		acc:               acc,
		host:              h,
		reservation:       resv,
		closer:            cancel,
		ctx:               ctx,
		relayInfo:         relayInfo,
		registry:          newRegistry(),
		coordinator:       coordinator,
		coordinateTimeout: defaultCoordinateTimeout,
	}

	// Register stream handler — clients dial this protocol ID to register channels
	h.SetStreamHandler(NotifyWatchLedgerProtocolID, c.handleNotifyWatchLedger)
	h.SetStreamHandler(NotifyWatchSubProtocolID, c.handleNotifyWatchSub)
	h.SetStreamHandler(NotifyStopWatchProtocolID, c.handleNotifyStopWatch)

	go c.keepReservationAlive(ctx, *relayInfo)

	return c, nil
}

func getRelayServerInfo() (*peer.AddrInfo, string, error) {
	id, err := peer.Decode(relayID)
	if err != nil {
		err = errors.WithMessage(err, "decoding peer id of relay server")
		return nil, "", err
	}

	// Get the IP address of the relay server.
	ip, err := net.LookupIP("relay.perun.network")
	if err != nil {
		err = errors.WithMessage(err, "looking up IP address of relay.perun.network")
		return nil, "", err
	}
	relayAddr := "/ip4/" + ip[0].String() + "/tcp/5574"

	relayMultiaddr, err := ma.NewMultiaddr(relayAddr)
	if err != nil {
		err = errors.WithMessage(err, "parsing relay multiadress")
		return nil, "", err
	}

	relayInfo := &peer.AddrInfo{
		ID:    id,
		Addrs: []ma.Multiaddr{relayMultiaddr},
	}

	return relayInfo, relayAddr, nil
}

// Close shuts down the coordinator host and cancels all background goroutines.
func (c *CoordinatorHost) Close() error {
	c.closer()
	return c.host.Close()
}

func (c *CoordinatorHost) keepReservationAlive(ctx context.Context, relay peer.AddrInfo) {
	const (
		renewInterval  = 4 * time.Minute
		initialBackoff = 2 * time.Second
		maxBackoff     = 1 * time.Minute
	)

	ticker := time.NewTicker(renewInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.renewWithBackoff(ctx, relay, initialBackoff, maxBackoff)
		}
	}
}

func (c *CoordinatorHost) renewWithBackoff(ctx context.Context, relay peer.AddrInfo, initialBackoff, maxBackoff time.Duration) {
	backoff := initialBackoff
	for {
		if sw, ok := c.host.Network().(*swarm.Swarm); ok {
			sw.Backoff().Clear(relay.ID)
		}

		newReservation, err := libp2pclient.Reserve(ctx, c.host, relay)
		if err == nil {
			c.relayMu.Lock()
			c.reservation = newReservation
			c.relayMu.Unlock()
			return
		}

		if ctx.Err() != nil {
			return
		}

		log.Printf("keepReservationAlive: renewal failed: %v; retrying in %s", err, backoff)

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		if backoff*2 < maxBackoff {
			backoff *= 2
		} else {
			backoff = maxBackoff
		}
	}
}

func (c *CoordinatorHost) ID() peer.ID {
	return c.host.ID()
}
