package coordinator

import (
	"context"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	ethchannel "github.com/perun-network/perun-eth-backend/channel"
	_ "github.com/perun-network/perun-eth-backend/channel" // registers ETH channel backend (BackendID=1)
	"github.com/stretchr/testify/require"
	"perun.network/go-perun/channel"
	"perun.network/go-perun/channel/multi"
	"perun.network/go-perun/wallet"
)

// ---- injectable mock CoordinatorSubscriber ----

// mockSub is a buffered, injectable AdjudicatorSubscription.
type mockSub struct {
	events chan channel.AdjudicatorEvent
	done   chan struct{}
	once   sync.Once
}

func newMockSub() *mockSub {
	return &mockSub{
		events: make(chan channel.AdjudicatorEvent, 8),
		done:   make(chan struct{}),
	}
}

func (m *mockSub) Next() channel.AdjudicatorEvent {
	select {
	case e := <-m.events:
		return e
	case <-m.done:
		return nil
	}
}

func (m *mockSub) Err() error { return nil }

func (m *mockSub) Close() error {
	m.once.Do(func() { close(m.done) })
	return nil
}

func (m *mockSub) inject(e channel.AdjudicatorEvent) { m.events <- e }

// mockCoordSub implements channel.CoordinatorSubscriber.
// Subscriptions are keyed by channel ID so tests can inject events per channel.
type mockCoordSub struct {
	mu           sync.Mutex
	subs         map[channel.ID]*mockSub
	coordinateCh chan struct{}
}

func newMockCoordSub() *mockCoordSub {
	return &mockCoordSub{
		subs:         make(map[channel.ID]*mockSub),
		coordinateCh: make(chan struct{}, 1),
	}
}

func (m *mockCoordSub) Subscribe(_ context.Context, id channel.ID) (channel.AdjudicatorSubscription, error) {
	sub := newMockSub()
	m.mu.Lock()
	m.subs[id] = sub
	m.mu.Unlock()
	return sub, nil
}

func (m *mockCoordSub) Coordinate(_ context.Context, _ channel.AdjudicatorReq, _ []channel.SignedState, _ []wallet.Sig) error {
	select {
	case m.coordinateCh <- struct{}{}:
	default:
	}
	return nil
}

func (m *mockCoordSub) inject(id channel.ID, e channel.AdjudicatorEvent) {
	m.mu.Lock()
	sub := m.subs[id]
	m.mu.Unlock()
	if sub != nil {
		sub.inject(e)
	}
}

// ---- test helpers ----

// makeMockHost builds a CoordinatorHost with one mockCoordSub registered
// under Ethereum ledger 1337. Returns the host, mock, and coordinator address.
func makeMockHost(t *testing.T) (*CoordinatorHost, *mockCoordSub, wallet.Address) {
	t.Helper()
	coordAcc, coordAddr := makeEthAcc(t)

	mock := newMockCoordSub()
	mc := multi.NewCoordinator()
	mc.RegisterCoordinator(ethchannel.MakeLedgerBackendID(big.NewInt(1337)), mock)

	host := &CoordinatorHost{
		acc:         map[wallet.BackendID]wallet.Account{1: coordAcc},
		registry:    newRegistry(),
		coordinator: mc,
	}
	return host, mock, coordAddr
}

// makeETHParamsAndState returns ETH-compatible Params (ChallengeDuration=0,
// so the dispute window is immediately elapsed) and a matching State for ledger 1337.
func makeETHParamsAndState(t *testing.T, coordAddr wallet.Address) (*channel.Params, *channel.State) {
	t.Helper()
	_, aliceAddr := makeEthAcc(t)
	_, bobAddr := makeEthAcc(t)

	parts := []map[wallet.BackendID]wallet.Address{
		{1: aliceAddr},
		{1: bobAddr},
	}
	coordMap := map[wallet.BackendID]wallet.Address{1: coordAddr}
	// NewParamsUnsafe bypasses challengeDuration!=0 validation; CalcID still runs.
	params := channel.NewParamsUnsafe(
		0,
		parts,
		channel.NoApp(),
		big.NewInt(0xC0FFEE),
		true,
		false,
		channel.Aux{},
		coordMap,
	)

	ethAsset := ethchannel.NewAsset(big.NewInt(1337), common.Address{})
	state := &channel.State{
		ID: params.ID(),
		Allocation: channel.Allocation{
			Balances: channel.Balances{{big.NewInt(0), big.NewInt(0)}},
			Backends: []wallet.BackendID{1},
			Assets:   []channel.Asset{ethAsset},
		},
		App:     channel.NoApp(),
		Data:    channel.NoData(),
		Version: 0,
		IsFinal: true,
	}
	return params, state
}

// ---- integration tests ----

// TestMockIntegration_SingleLedger exercises the full event→coordinate pipeline:
// startWatchingLedger → inject RegisteredEvent → assert Coordinate is called.
func TestMockIntegration_SingleLedger(t *testing.T) {
	host, mock, coordAddr := makeMockHost(t)
	params, state := makeETHParamsAndState(t, coordAddr)
	id := params.ID()
	ctx := context.Background()

	require.NoError(t, host.startWatchingLedger(ctx, NotifyWatchLedgerChannelRequest{
		SignedState: channel.SignedState{Params: params, State: state},
	}))
	t.Cleanup(func() { _ = host.stopWatching(id) })

	// Simulate on-chain dispute: elapsed timeout → coordinator should act immediately.
	mock.inject(id, channel.NewRegisteredEvent(id, &channel.ElapsedTimeout{}, 0, state, nil))

	select {
	case <-mock.coordinateCh:
		// Coordinate was called — coordinator issued coordSigs.
	case <-time.After(3 * time.Second):
		t.Fatal("Coordinate was not called within 3 seconds")
	}
}

// TestMockIntegration_SubChannel registers a ledger channel plus one sub-channel.
// After the RegisteredEvent for the parent, all sub-channel states must be
// collected and passed to Coordinate.
func TestMockIntegration_SubChannel(t *testing.T) {
	host, mock, coordAddr := makeMockHost(t)
	params, state := makeETHParamsAndState(t, coordAddr)
	parentID := params.ID()
	ctx := context.Background()

	// Sub-channel gets its own params/state (different nonce → different ID).
	_, subState := makeETHParamsAndStateWithNonce(t, coordAddr, big.NewInt(0xDEAD))
	subParams := makeSubParams(t, coordAddr)
	subID := subParams.ID()

	require.NoError(t, host.startWatchingLedger(ctx, NotifyWatchLedgerChannelRequest{
		SignedState: channel.SignedState{Params: params, State: state},
	}))
	require.NoError(t, host.startWatchingSub(ctx, NotifyWatchSubChannelRequest{
		ParentID:    parentID,
		SignedState: channel.SignedState{Params: subParams, State: subState},
	}))
	t.Cleanup(func() { _ = host.stopWatching(parentID) })

	mock.inject(parentID, channel.NewRegisteredEvent(parentID, &channel.ElapsedTimeout{}, 0, state, nil))

	select {
	case <-mock.coordinateCh:
	case <-time.After(3 * time.Second):
		t.Fatal("Coordinate was not called within 3 seconds")
	}

	_ = subID
}

// TestMockIntegration_DuplicateEvent verifies that a second RegisteredEvent with
// the same version does not trigger a second Coordinate call.
func TestMockIntegration_DuplicateEvent(t *testing.T) {
	host, mock, coordAddr := makeMockHost(t)
	params, state := makeETHParamsAndState(t, coordAddr)
	id := params.ID()
	ctx := context.Background()

	require.NoError(t, host.startWatchingLedger(ctx, NotifyWatchLedgerChannelRequest{
		SignedState: channel.SignedState{Params: params, State: state},
	}))
	t.Cleanup(func() { _ = host.stopWatching(id) })

	e := channel.NewRegisteredEvent(id, &channel.ElapsedTimeout{}, 0, state, nil)
	mock.inject(id, e)

	// First Coordinate call.
	select {
	case <-mock.coordinateCh:
	case <-time.After(3 * time.Second):
		t.Fatal("first Coordinate not called")
	}

	// Second identical event — awaitFinalisationAndCoordinate is already done,
	// so no second Coordinate call should arrive quickly.
	mock.inject(id, e)
	select {
	case <-mock.coordinateCh:
		t.Error("unexpected second Coordinate call")
	case <-time.After(200 * time.Millisecond):
		// expected: no second call
	}
}

// makeETHParamsAndStateWithNonce is like makeETHParamsAndState but accepts a custom nonce.
func makeETHParamsAndStateWithNonce(t *testing.T, coordAddr wallet.Address, nonce *big.Int) (*channel.Params, *channel.State) {
	t.Helper()
	_, aliceAddr := makeEthAcc(t)
	_, bobAddr := makeEthAcc(t)

	parts := []map[wallet.BackendID]wallet.Address{{1: aliceAddr}, {1: bobAddr}}
	coordMap := map[wallet.BackendID]wallet.Address{1: coordAddr}
	params := channel.NewParamsUnsafe(0, parts, channel.NoApp(), nonce, false, true, channel.Aux{}, coordMap)

	ethAsset := ethchannel.NewAsset(big.NewInt(1337), common.Address{})
	state := &channel.State{
		ID: params.ID(),
		Allocation: channel.Allocation{
			Balances: channel.Balances{{big.NewInt(0), big.NewInt(0)}},
			Backends: []wallet.BackendID{1},
			Assets:   []channel.Asset{ethAsset},
		},
		App:     channel.NoApp(),
		Data:    channel.NoData(),
		IsFinal: true,
	}
	return params, state
}

// makeSubParams builds a minimal Params that can be used as a sub-channel
// (VirtualChannel=true, distinct nonce so the ID differs from the parent).
func makeSubParams(t *testing.T, coordAddr wallet.Address) *channel.Params {
	t.Helper()
	_, aliceAddr := makeEthAcc(t)
	_, bobAddr := makeEthAcc(t)
	parts := []map[wallet.BackendID]wallet.Address{{1: aliceAddr}, {1: bobAddr}}
	coordMap := map[wallet.BackendID]wallet.Address{1: coordAddr}
	return channel.NewParamsUnsafe(0, parts, channel.NoApp(), big.NewInt(0xABCD), false, true, channel.Aux{}, coordMap)
}
