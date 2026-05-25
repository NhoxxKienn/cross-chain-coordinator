package coordinator

import (
	"context"
	"testing"

	gocrypto "github.com/ethereum/go-ethereum/crypto"
	ethwallet "github.com/perun-network/perun-eth-backend/wallet"
	_ "github.com/perun-network/perun-eth-backend/wallet" // registers ETH wallet backend
	swallet "github.com/perun-network/perun-eth-backend/wallet/simple"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"perun.network/go-perun/channel"
	"perun.network/go-perun/channel/multi"
	"perun.network/go-perun/wallet"
)

// newTestHost builds a minimal CoordinatorHost with no registered chain coordinators.
// The empty multi.Coordinator means Subscribe returns a subscription that blocks forever
// on events but unblocks cleanly when Close is called.
func newTestHost(acc map[wallet.BackendID]wallet.Account) *CoordinatorHost {
	return &CoordinatorHost{
		acc:         acc,
		registry:    newRegistry(),
		coordinator: multi.NewCoordinator(),
	}
}

// makeEthAcc generates a fresh ECDSA key and returns the unlocked wallet account and address.
func makeEthAcc(t *testing.T) (wallet.Account, wallet.Address) {
	t.Helper()
	key, err := gocrypto.GenerateKey()
	require.NoError(t, err)
	w := swallet.NewWallet(key)
	addr := ethwallet.AsWalletAddr(gocrypto.PubkeyToAddress(key.PublicKey))
	acc, err := w.Unlock(addr)
	require.NoError(t, err)
	return acc, addr
}

func TestValidateCoordinatorDesignation(t *testing.T) {
	acc, addr := makeEthAcc(t)
	host := newTestHost(map[wallet.BackendID]wallet.Account{1: acc})

	t.Run("matching address", func(t *testing.T) {
		params := &channel.Params{Coordinator: map[wallet.BackendID]wallet.Address{1: addr}}
		assert.NoError(t, host.validateCoordinatorDesignation(params))
	})

	t.Run("wrong address", func(t *testing.T) {
		_, wrongAddr := makeEthAcc(t)
		params := &channel.Params{Coordinator: map[wallet.BackendID]wallet.Address{1: wrongAddr}}
		assert.Error(t, host.validateCoordinatorDesignation(params))
	})

	t.Run("wrong backend ID", func(t *testing.T) {
		params := &channel.Params{Coordinator: map[wallet.BackendID]wallet.Address{2: addr}}
		assert.Error(t, host.validateCoordinatorDesignation(params))
	})

	t.Run("nil coordinator map", func(t *testing.T) {
		params := &channel.Params{Coordinator: nil}
		assert.Error(t, host.validateCoordinatorDesignation(params))
	})
}

// signedState builds a minimal SignedState with the given ID — enough for startWatching.
func signedState(id channel.ID, coordMap map[wallet.BackendID]wallet.Address) channel.SignedState {
	return channel.SignedState{
		Params: &channel.Params{Coordinator: coordMap},
		State:  &channel.State{ID: id},
	}
}

func TestStopWatching_CascadesSubs(t *testing.T) {
	acc, addr := makeEthAcc(t)
	host := newTestHost(map[wallet.BackendID]wallet.Account{1: acc})
	ctx := context.Background()
	coordMap := map[wallet.BackendID]wallet.Address{1: addr}

	parentID := channel.ID{1}
	subID := channel.ID{2}

	require.NoError(t, host.startWatchingLedger(ctx, NotifyWatchLedgerChannelRequest{
		SignedState: signedState(parentID, coordMap),
	}))
	require.NoError(t, host.startWatchingSub(ctx, NotifyWatchSubChannelRequest{
		ParentID:    parentID,
		SignedState: signedState(subID, coordMap),
	}))

	_, ok := host.registry.retrieve(parentID)
	require.True(t, ok, "parent must be in registry before stop")
	_, ok = host.registry.retrieve(subID)
	require.True(t, ok, "sub must be in registry before stop")

	require.NoError(t, host.stopWatching(parentID))

	_, ok = host.registry.retrieve(parentID)
	assert.False(t, ok, "parent should be removed from registry")
	_, ok = host.registry.retrieve(subID)
	assert.False(t, ok, "sub should be removed by cascade")
}

func TestStopWatching_SubOnly(t *testing.T) {
	acc, addr := makeEthAcc(t)
	host := newTestHost(map[wallet.BackendID]wallet.Account{1: acc})
	ctx := context.Background()
	coordMap := map[wallet.BackendID]wallet.Address{1: addr}

	parentID := channel.ID{1}
	subID := channel.ID{2}

	require.NoError(t, host.startWatchingLedger(ctx, NotifyWatchLedgerChannelRequest{
		SignedState: signedState(parentID, coordMap),
	}))
	require.NoError(t, host.startWatchingSub(ctx, NotifyWatchSubChannelRequest{
		ParentID:    parentID,
		SignedState: signedState(subID, coordMap),
	}))

	// Stop only the sub-channel — parent should remain.
	require.NoError(t, host.stopWatching(subID))

	_, ok := host.registry.retrieve(parentID)
	assert.True(t, ok, "parent should remain in registry")
	_, ok = host.registry.retrieve(subID)
	assert.False(t, ok, "sub should be removed")
}

func TestStopWatching_NotRegistered(t *testing.T) {
	host := newTestHost(nil)
	err := host.stopWatching(channel.ID{99})
	assert.Error(t, err)
}

func TestStartWatching_RejectsDuplicateID(t *testing.T) {
	acc, addr := makeEthAcc(t)
	host := newTestHost(map[wallet.BackendID]wallet.Account{1: acc})
	ctx := context.Background()
	coordMap := map[wallet.BackendID]wallet.Address{1: addr}
	id := channel.ID{5}

	require.NoError(t, host.startWatchingLedger(ctx, NotifyWatchLedgerChannelRequest{
		SignedState: signedState(id, coordMap),
	}))
	err := host.startWatchingLedger(ctx, NotifyWatchLedgerChannelRequest{
		SignedState: signedState(id, coordMap),
	})
	assert.Error(t, err, "registering same ID twice should fail")
}

func TestStartWatching_RejectsWrongCoordinator(t *testing.T) {
	acc, addr := makeEthAcc(t)
	_, wrongAddr := makeEthAcc(t)
	host := newTestHost(map[wallet.BackendID]wallet.Account{1: acc})
	ctx := context.Background()

	// Channel designates a coordinator address we don't hold.
	err := host.startWatchingLedger(ctx, NotifyWatchLedgerChannelRequest{
		SignedState: signedState(channel.ID{7}, map[wallet.BackendID]wallet.Address{1: wrongAddr}),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not designated")

	_ = addr // suppress unused-variable warning
}
