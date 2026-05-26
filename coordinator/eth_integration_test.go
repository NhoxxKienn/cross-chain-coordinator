//go:build integration

package coordinator

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	gocrypto "github.com/ethereum/go-ethereum/crypto"
	ethchannel "github.com/perun-network/perun-eth-backend/channel"
	ethtest "github.com/perun-network/perun-eth-backend/channel/test"
	ethwallet "github.com/perun-network/perun-eth-backend/wallet"
	swallet "github.com/perun-network/perun-eth-backend/wallet/simple"
	"github.com/stretchr/testify/require"
	"perun.network/go-perun/channel"
	"perun.network/go-perun/channel/multi"
	"perun.network/go-perun/wallet"
)

// TestEthIntegration_FullFlow exercises the full on-chain coordination path:
//
//  1. Alice registers a non-final multi-ledger state on a simulated Ethereum chain.
//  2. The coordinator detects the RegisteredEvent, waits the wall-clock dispute
//     duration (ChallengeDuration=2 s), then calls on-chain coordinate().
//  3. The test asserts that a CoordinatedEvent is received on Alice's
//     adjudicator subscription within 30 seconds.
//
// The state must be multi-ledger (assets on ≥2 distinct chains) because the
// Solidity Adjudicator.coordinateSingle() requires isMultiLedgerState(state).
// We use chain 1337 (simulated, real Adjudicator) + chain 1338 (mock-only).
//
// Auto-mining via sb.StartMining(50ms) advances blockchain time so that
// waitCoordinable's on-chain BlockTimeout (dispute.timeout + 12 s slack)
// elapses naturally — this is the same pattern used by perun-eth-backend's
// own TestCoordinate_Basic and avoids the subscription-overflow / nil-receipt
// failure mode caused by hand-rolled rapid Commit() loops.
//
// Run with: go test -tags integration ./coordinator/... -run TestEthIntegration -v
func TestEthIntegration_FullFlow(t *testing.T) {
	const (
		challengeDuration = uint64(2)
		coordinateTimeout = 25 * time.Second
		eventWaitTimeout  = 30 * time.Second
		blockInterval     = 50 * time.Millisecond
		txFinalityDepth   = uint64(1)
	)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// ---- simulated chain (chain 1337) with auto-mining ----
	sb := ethtest.NewSimulatedBackend()
	sb.StartMining(blockInterval)
	// Stop mining BEFORE the deferred sb.Close so the mining goroutine
	// doesn't race with the backend shutdown. t.Cleanup runs LIFO; the
	// StopMining cleanup is registered after sb.Close so it runs first.
	t.Cleanup(func() { _ = sb.Close() })
	t.Cleanup(sb.StopMining)

	// ---- ECDSA keys ----
	coordKey, err := gocrypto.GenerateKey()
	require.NoError(t, err)
	aliceKey, err := gocrypto.GenerateKey()
	require.NoError(t, err)
	bobKey, err := gocrypto.GenerateKey()
	require.NoError(t, err)

	coordEthAddr := gocrypto.PubkeyToAddress(coordKey.PublicKey)
	aliceEthAddr := gocrypto.PubkeyToAddress(aliceKey.PublicKey)

	sb.FundAddress(ctx, coordEthAddr)
	sb.FundAddress(ctx, aliceEthAddr)

	// ---- simple wallets and contract backends ----
	coordWallet := swallet.NewWallet(coordKey)
	aliceWallet := swallet.NewWallet(aliceKey)
	bobWallet := swallet.NewWallet(bobKey)

	chainID := sb.ChainID() // 1337
	coordCB := ethchannel.NewContractBackend(sb, ethchannel.MakeChainID(chainID), swallet.NewTransactor(coordWallet, sb.Signer), txFinalityDepth)
	aliceCB := ethchannel.NewContractBackend(sb, ethchannel.MakeChainID(chainID), swallet.NewTransactor(aliceWallet, sb.Signer), txFinalityDepth)

	coordEthAcc := accounts.Account{Address: coordEthAddr}
	aliceEthAcc := accounts.Account{Address: aliceEthAddr}

	// ---- deploy adjudicator on chain 1337 ----
	adjAddr, err := ethchannel.DeployAdjudicator(ctx, coordCB, coordEthAcc)
	require.NoError(t, err)

	// ---- build SimCoordinator (chain 1337) and SimAdjudicator (Alice) ----
	simCoord := ethtest.NewSimCoordinator(coordCB, adjAddr, coordEthAddr, coordEthAcc)
	simAdj := ethtest.NewSimAdjudicator(aliceCB, adjAddr, aliceEthAddr, aliceEthAcc)

	// ---- multi-coordinator: real SimCoordinator for chain 1337, mock for 1338 ----
	// The Solidity coordinateSingle() requires isMultiLedgerState(state), which is
	// true only when assets span ≥2 distinct chains. We include a chain-1338 asset
	// to satisfy this, backed by a mock coordinator that accepts Coordinate() calls.
	mock1338 := newMockCoordSub()
	mc := multi.NewCoordinator()
	mc.RegisterCoordinator(ethchannel.MakeLedgerBackendID(chainID), simCoord)
	mc.RegisterCoordinator(ethchannel.MakeLedgerBackendID(big.NewInt(1338)), mock1338)

	// ---- go-perun wallet accounts (for Perun-level state signing) ----
	coordWalletAddr := ethwallet.AsWalletAddr(coordEthAddr)
	aliceWalletAddr := ethwallet.AsWalletAddr(aliceEthAddr)
	bobWalletAddr := ethwallet.AsWalletAddr(gocrypto.PubkeyToAddress(bobKey.PublicKey))

	coordWalletAcc, err := coordWallet.Unlock(coordWalletAddr)
	require.NoError(t, err)
	aliceWalletAcc, err := aliceWallet.Unlock(aliceWalletAddr)
	require.NoError(t, err)
	bobWalletAcc, err := bobWallet.Unlock(bobWalletAddr)
	require.NoError(t, err)

	// ---- CoordinatorHost ----
	host := &CoordinatorHost{
		acc:               map[wallet.BackendID]wallet.Account{1: coordWalletAcc},
		registry:          newRegistry(),
		coordinator:       mc,
		coordinateTimeout: coordinateTimeout,
	}

	// ---- channel params and state ----
	// ChallengeDuration must satisfy two timers:
	//   1. coordinator's wall-clock wait (ChallengeDuration seconds)
	//   2. on-chain BlockTimeout in waitCoordinable (dispute.timeout + 12 s slack)
	// With auto-mining at 50 ms and default +1 s block-time increments, the on-chain
	// clock advances ~20 s per second of real time, so a ChallengeDuration of 2 s
	// gives the wall-clock timer breathing room and the on-chain timeout elapses
	// well within eventWaitTimeout.
	//
	// IsFinal=false routes to registerNonFinal (not concludeFinal); LedgerChannel=true
	// is required by the on-chain register() check.
	parts := []map[wallet.BackendID]wallet.Address{
		{1: aliceWalletAddr},
		{1: bobWalletAddr},
	}
	coordMap := map[wallet.BackendID]wallet.Address{1: coordWalletAddr}
	params := channel.NewParamsUnsafe(challengeDuration, parts, channel.NoApp(), big.NewInt(0xCAFE1337), true, false, channel.Aux{}, coordMap)

	// Two assets on different chains → isMultiLedgerState returns true →
	// coordinateSingle() canEnterCoordinated check passes.
	asset1337 := ethchannel.NewAsset(chainID, common.Address{})
	asset1338 := ethchannel.NewAsset(big.NewInt(1338), common.Address{})
	state := &channel.State{
		ID: params.ID(),
		Allocation: channel.Allocation{
			Balances: channel.Balances{
				{big.NewInt(0), big.NewInt(0)}, // asset1337: alice=0, bob=0
				{big.NewInt(0), big.NewInt(0)}, // asset1338: alice=0, bob=0
			},
			Backends: []wallet.BackendID{1, 1},
			Assets:   []channel.Asset{asset1337, asset1338},
		},
		App:     channel.NoApp(),
		Data:    channel.NoData(),
		Version: 0,
		IsFinal: false,
	}

	// Participant signatures are validated on-chain by the adjudicator.
	aliceSig, err := channel.Sign(aliceWalletAcc, state, 1)
	require.NoError(t, err)
	bobSig, err := channel.Sign(bobWalletAcc, state, 1)
	require.NoError(t, err)

	signedState := channel.SignedState{
		Params: params,
		State:  state,
		Sigs:   []wallet.Sig{aliceSig, bobSig},
	}

	// ---- start watching ----
	// CRITICAL ordering: host.Wait MUST run before sb.Close so any in-flight
	// coordinate() ConfirmTransaction subscription is drained before the
	// simulated backend tears down its RPC client (otherwise the subscription
	// fires nil on hsub.Err() → confirmNTimes returns (nil, nil) → panic).
	id := params.ID()
	require.NoError(t, host.startWatchingLedger(ctx, NotifyWatchLedgerChannelRequest{
		SignedState: signedState,
	}))
	t.Cleanup(func() { _ = host.Wait(coordinateTimeout) })
	t.Cleanup(func() { _ = host.stopWatching(id) })

	// Alice subscribes before registering so she receives all subsequent events
	// (RegisteredEvent, then CoordinatedEvent on the same subscription).
	aliceSub, err := simAdj.Subscribe(ctx, id)
	require.NoError(t, err)
	defer aliceSub.Close()

	// ---- Alice registers the channel on chain 1337 ----
	adjReq := channel.AdjudicatorReq{
		Params: params,
		Acc:    map[wallet.BackendID]wallet.Account{1: aliceWalletAcc},
		Tx:     channel.Transaction{State: state, Sigs: []wallet.Sig{aliceSig, bobSig}},
		Idx:    0,
	}
	require.NoError(t, simAdj.Register(ctx, adjReq, nil))

	// ---- wait for CoordinatedEvent ----
	// Flow: RegisteredEvent → wall-clock wait (ChallengeDuration s) →
	// coordinator.Coordinate → on-chain coordinate() tx → CoordinatedEvent.
	coordinatedCh := make(chan struct{}, 1)
	go func() {
		for {
			ev := aliceSub.Next()
			if ev == nil {
				return
			}
			if _, ok := ev.(*channel.CoordinatedEvent); ok {
				coordinatedCh <- struct{}{}
				return
			}
		}
	}()

	select {
	case <-coordinatedCh:
		// coordinator issued coordSigs and submitted coordinate() on-chain ✓
	case <-time.After(eventWaitTimeout):
		t.Fatalf("CoordinatedEvent not received within %s", eventWaitTimeout)
	}
}
