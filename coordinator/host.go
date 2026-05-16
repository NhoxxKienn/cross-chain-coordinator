package coordinator

import (
	"context"
	"log"
	"net"
	"os"
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

type CoordinatorHost struct {
	acc         wallet.Account
	host        host.Host
	reservation *libp2pclient.Reservation
	closer      context.CancelFunc
	ctx         context.Context

	registry    *registry
	coordinator *multi.Coordinator

	relayMu   sync.Mutex
	relayInfo *peer.AddrInfo
}

//	SetupRelayCoordinator initializes a libp2p host with relay capabilities and an address book.
//
// It listens on the specified port and uses the provided key file for identity.
func SetupRelayCoordinator(keyFile string, acc wallet.Account, coordinator *multi.Coordinator) (*CoordinatorHost, error) {
	relayInfo, _, err := getRelayServerInfo()
	if err != nil {
		return nil, errors.WithMessage(err, "getting relay server info")
	}

	keyBytes, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, err
	}
	privKey, err := crypto.UnmarshalPrivateKey(keyBytes)
	if err != nil {
		return nil, err
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
		acc:         acc,
		host:        h,
		reservation: resv,
		closer:      cancel,
		ctx:         ctx,
		relayInfo:   relayInfo,
		registry:    newRegistry(),
		coordinator: coordinator,
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
