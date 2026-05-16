package coordinator

import (
	"errors"
	"time"

	"polycry.pt/poly-go/sync"

	"perun.network/go-perun/channel"
	"perun.network/go-perun/channel/multi"
)

type CoordPhase uint8

const (
	PhaseWatching      CoordPhase = iota
	PhaseReadyForCoord            // all chains finalised
	PhaseCoordinated              // coordSigs submitted
)

type chainDispute struct {
	version   uint64
	state     *channel.SignedState
	timeout   time.Time
	finalised bool
}

type coordCh struct {
	id          channel.ID
	params      *channel.Params
	isClosed    bool
	done        chan struct{}
	parent      *coordCh
	multiLedger bool

	// Sub-channel tree in DFS order — maps to coordSigs indices.
	subChs              map[channel.ID]struct{}
	archivedSubChStates map[channel.ID]channel.SignedState

	// For keeping track of the version registered on the blockchain for
	// this channel. This is used to prevent registering the same state
	// more than once.
	registered        map[multi.LedgerBackendKey]bool
	registeredVersion map[multi.LedgerBackendKey]uint64

	// Per-chain dispute state keyed by chainID.
	chainDisputes  map[multi.LedgerBackendKey]*chainDispute
	canonicalState *channel.SignedState
	phase          CoordPhase
	registeredAt   time.Time

	// eventsFromChainSub holds one subscription per chain.
	eventsFromChainSub *multi.AdjudicatorSubscription

	// subChsAccess serialises cross-chain event processing,
	// identical role to the watcher's subChsAccess.
	subChsAccess sync.Mutex
	wg           sync.WaitGroup
}

type chInitializer func() (*coordCh, error)

// isReadyForCoordination implements ReadyForCoordination(cid) from the thesis:
// every chain must have a confirmed finalised dispute window.
func (c *coordCh) isReadyForCoordination() bool {
	if len(c.chainDisputes) == 0 {
		return false
	}
	for _, d := range c.chainDisputes {
		if !d.finalised {
			return false
		}
	}
	return true
}

// selectCanonicalState picks the highest-version state across all chains.
// Conflict resolution rule: highest version with valid participant witness wins.
func (c *coordCh) selectCanonicalSignedState() *channel.SignedState {
	var best *channel.SignedState
	for _, d := range c.chainDisputes {
		if d.state == nil {
			continue
		}
		if best == nil || d.version > best.State.Version {
			best = d.state
		}
	}
	return best
}

// Go executes a function in a goroutine, updating the channel's wait group
// before and after execution.
func (ch *coordCh) Go(fn func()) {
	ch.wg.Add(1)

	go func() {
		defer ch.wg.Done()

		fn()
	}()
}

func (ch *coordCh) isSubChannel() bool { return ch.parent != nil }

// registry mirrors local/registry.go exactly.
type registry struct {
	mtx sync.Mutex
	chs map[channel.ID]*coordCh
}

func newRegistry() *registry {
	return &registry{
		chs: make(map[channel.ID]*coordCh),
	}
}

func (r *registry) addIfSucceeds(id channel.ID, init chInitializer) (*coordCh, error) {
	r.mtx.Lock()
	defer r.mtx.Unlock()

	if _, ok := r.chs[id]; ok {
		return nil, errors.New("already watching this channel")
	}
	ch, err := init()
	if err != nil {
		return nil, err
	}
	r.chs[ch.id] = ch
	return ch, nil
}

func (r *registry) retrieve(id channel.ID) (*coordCh, bool) {
	r.mtx.Lock()
	ch, ok := r.chs[id]
	r.mtx.Unlock()
	return ch, ok
}

func (r *registry) remove(id channel.ID) {
	r.mtx.Lock()
	delete(r.chs, id)
	r.mtx.Unlock()
}
