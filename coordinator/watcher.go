package coordinator

import (
	"context"
	"time"

	"github.com/pkg/errors"

	"perun.network/go-perun/channel"
	"perun.network/go-perun/channel/multi"
	"perun.network/go-perun/log"
)

const statesFromClientWaitTime = 1 * time.Millisecond

// startWatchingLedger mirrors Watcher.StartWatchingLedgerChannel.
// Called from handleNotifyWatchLedger after decoding the stream message.
func (c *CoordinatorHost) startWatchingLedger(
	ctx context.Context,
	req NotifyWatchLedgerChannelRequest,
) error {
	return c.startWatching(ctx, nil, req.SignedState)
}

// startWatchingSub mirrors Watcher.StartWatchingSubChannel.
// Called from handleNotifyWatchSub after decoding the stream message.
func (c *CoordinatorHost) startWatchingSub(
	ctx context.Context,
	req NotifyWatchSubChannelRequest,
) error {
	parentCh, ok := c.registry.retrieve(req.ParentID)
	if !ok {
		return errors.New("parent channel not registered with the coordinator")
	}
	if parentCh.isSubChannel() {
		return errors.New("parent must be a ledger channel")
	}

	parentCh.subChsAccess.Lock()
	defer parentCh.subChsAccess.Unlock()

	err := c.startWatching(ctx, parentCh, req.SignedState) // sub uses parent's chain subs
	if err != nil {
		return err
	}
	parentCh.subChs[req.SignedState.State.ID] = struct{}{}
	return nil
}

// stopWatching mirrors Watcher.StopWatching.
func (c *CoordinatorHost) stopWatching(id channel.ID) error {
	ch, ok := c.registry.retrieve(id)
	if !ok {
		return errors.New("channel not registered with the coordinator")
	}

	parent := ch
	if ch.isSubChannel() {
		parent = ch.parent
	}

	parent.subChsAccess.Lock()
	defer parent.subChsAccess.Unlock()

	if ch.isClosed {
		return errors.New("channel not registered with the coordinator")
	}
	close(ch.done)

	if ch.isSubChannel() {
		delete(parent.subChs, id)
	} else {
		// Cascade: close all sub-channels before removing the parent.
		// Closing the subscription terminates the sub-channel event loop goroutine.
		for subID := range ch.subChs {
			if subCh, ok := c.registry.retrieve(subID); ok {
				close(subCh.done)
				closeChain(subCh)
				c.registry.remove(subID)
				subCh.isClosed = true
			}
		}
	}

	closeChain(ch)
	c.registry.remove(ch.id)
	ch.isClosed = true
	return nil
}

// validateCoordinatorDesignation rejects channels that are not designated for
// this coordinator. Checks that Params.Coordinator contains an address matching
// one of our accounts — if wrong, CalcID will mismatch on the client side.
func (c *CoordinatorHost) validateCoordinatorDesignation(params *channel.Params) error {
	for bID, addr := range params.Coordinator {
		if acc, ok := c.acc[bID]; ok && acc.Address().Equal(addr) {
			return nil
		}
	}
	return errors.New("channel not designated for this coordinator")
}

// startWatching is the internal shared implementation.
// Mirrors Watcher.startWatching exactly, adapted for multi-chain subscriptions.
func (c *CoordinatorHost) startWatching(
	ctx context.Context,
	parent *coordCh,
	signedState channel.SignedState,
) error {
	if err := c.validateCoordinatorDesignation(signedState.Params); err != nil {
		return err
	}

	id := signedState.State.ID

	chInitializer := func() (*coordCh, error) {
		eventsFromChainSub, err := c.coordinator.Subscribe(ctx, id)
		if err != nil {
			return nil, errors.WithMessage(err, "subscribing to adjudicator events from blockchain")
		}

		multiSubs, ok := eventsFromChainSub.(*multi.AdjudicatorSubscription)
		if !ok {
			return nil, errors.New("unexpected subscription type")
		}

		multiledger := multi.IsMultiLedgerAssets(signedState.State.Assets)
		return newCoordCh(id, parent, signedState.Params, multiSubs, multiledger), nil
	}

	ch, err := c.registry.addIfSucceeds(id, chInitializer)
	if err != nil {
		return err
	}

	ch.Go(func() { ch.handleEventsFromChain(c, c.registry) }) //nolint:contextcheck

	return nil
}

func newCoordCh(
	id channel.ID,
	parent *coordCh,
	params *channel.Params,
	eventsFromChainSub *multi.AdjudicatorSubscription,
	multiLedger bool,
) *coordCh {
	return &coordCh{
		id:          id,
		params:      params,
		parent:      parent,
		multiLedger: multiLedger,

		subChs:              make(map[channel.ID]struct{}),
		archivedSubChStates: make(map[channel.ID]channel.SignedState),

		registered:        make(map[multi.LedgerBackendKey]bool),
		registeredVersion: make(map[multi.LedgerBackendKey]uint64),

		chainDisputes: map[multi.LedgerBackendKey]*chainDispute{},

		eventsFromChainSub: eventsFromChainSub,
		done:               make(chan struct{}),
	}
}

func (ch *coordCh) handleEventsFromChain(coordinator *CoordinatorHost, chRegistry *registry) {
	// Create a context that is canceled when the watcher is stopped.
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		<-ch.done
		cancel()
	}()

	// NOTE: this loop must call NextWithKey() every iteration. The previous
	// `for init; cond; post` form only updated `e` in the post statement,
	// leaving `ledgerKey` frozen at the first event's value — every
	// subsequent RegisteredEvent / ProgressedEvent was attributed to the
	// first ledger, silently corrupting per-chain dispute tracking.
	for {
		e, ledgerKey, ok := ch.eventsFromChainSub.NextWithKey()
		if !ok || e == nil {
			break
		}
		switch e := e.(type) {
		case *channel.RegisteredEvent:
			log.WithFields(log.Fields{
				"ID":      e.ID(),
				"Version": e.Version(),
				"ledger":  ledgerKey.LedgerID,
				"backend": ledgerKey.BackendID,
			}).Debug("Coordinator received registered event")
			ch.handleRegisteredEvent(ctx, e, coordinator, ledgerKey)
		case *channel.ProgressedEvent:
			log.WithFields(log.Fields{
				"ID":      e.ID(),
				"Version": e.Version(),
				"ledger":  ledgerKey.LedgerID,
				"backend": ledgerKey.BackendID,
			}).Debug("Received progressed event — resetting dispute window")
			// Progressed resets the challenge window on this ledger.
			// Do NOT publish — the coordinator only observes.
			ch.handleProgressedEvent(ledgerKey)

		case *channel.CoordinatedEvent:
			log.WithFields(log.Fields{
				"ID":      e.ID(),
				"Version": e.Version(),
				"ledger":  ledgerKey.LedgerID,
				"backend": ledgerKey.BackendID,
			}).Info("Received coordinated event — channel is now fully finalised")
			// The channel is now fully finalised. We can stop watching and clean up.
			chRegistry.remove(ch.id)
			return
		case *channel.ConcludedEvent:
			log.WithFields(log.Fields{
				"ID":      e.ID(),
				"Version": e.Version(),
				"ledger":  ledgerKey.LedgerID,
				"backend": ledgerKey.BackendID,
			}).Debug("Received concluded event from chain")
			// The channel is concluded on this ledger. We can stop watching and clean up.
			// Note that for sub-channels, this may be received before the parent is coordinated.
			chRegistry.remove(ch.id)
			return
		default:
			// This should never happen.
			log.Error("Received adjudicator event of unknown type (%T) from chain: %v", e)
		}
	}
	err := ch.eventsFromChainSub.Err()
	if err != nil {
		log.Errorf("Subscription to adjudicator events from chain was closed with error: %v", err)
	}
}

// handleRegisteredEvent records the dispute for a specific ledger and,
// on the first registration, spawns awaitFinalisationAndCoordinate which
// watches ALL ledger dispute windows and triggers coordinate() once all close.
//
// Mirrors the watcher's handleRegisteredEvent with subChsAccess serialisation,
// but drives coordination instead of refutation.
func (ch *coordCh) handleRegisteredEvent(
	ctx context.Context,
	e *channel.RegisteredEvent,
	coordinator *CoordinatorHost,
	ledgerKey multi.LedgerBackendKey,
) {
	parent := ch
	if ch.isSubChannel() {
		parent = ch.parent
	}

	// Serialise across ledgers — identical to watcher's subChsAccess pattern.
	// Ensures concurrent RegisteredEvents from different chains are processed
	// one after the other on the same channel tree.
	if !parent.subChsAccess.TryLockCtx(ctx) {
		// Watching has been stopped. Return.
		return
	}
	defer parent.subChsAccess.Unlock()

	// Track first registration per ledger — mirrors watcher's registered/registeredVersion.
	firstRegistration := !parent.registered[ledgerKey]
	if firstRegistration {
		parent.registered[ledgerKey] = true
		parent.registeredVersion[ledgerKey] = e.Version()
	}

	// Update or create the per-ledger dispute record.
	// Only advance — never overwrite with a stale version.
	d := parent.chainDisputes[ledgerKey]
	if d == nil {
		d = &chainDispute{}
		parent.chainDisputes[ledgerKey] = d
	}

	if e.Version() >= d.version {
		d.version = e.Version()
		d.state = &channel.SignedState{
			Params: parent.params,
			State:  e.State,
			Sigs:   e.Sigs,
		}
		d.timeout = time.Now().Add(
			time.Duration(ch.params.ChallengeDuration) * time.Second,
		)
		d.finalised = false
	}

	// On first registration of any ledger, spawn the single goroutine that
	// watches ALL ledger timeouts and issues coordinate() once all close.
	// Subsequent RegisteredEvents on other ledgers update chainDisputes in-place;
	// awaitFinalisationAndCoordinate polls them all.
	if firstRegistration && len(parent.chainDisputes) == 1 {
		go parent.awaitFinalisationAndCoordinate(ctx, coordinator)
	}
}

// awaitFinalisationAndCoordinate is spawned ONCE per channel (not per ledger).
// It loops over ALL chainDisputes, waits for each timeout to expire,
// marks each as finalised, and calls coordinate() once all are finalised.
//
// This implements ReadyForCoordination(cid) from the thesis:
//
//	"W waits until every chain has reached its final pre-withdraw state
//	 and no further local dispute-side progression is possible."
func (ch *coordCh) awaitFinalisationAndCoordinate(
	ctx context.Context,
	coordinator *CoordinatorHost,
) {
	for {
		// Snapshot current dispute state under the lock.
		ch.subChsAccess.Lock()
		disputes := make(map[multi.LedgerBackendKey]time.Time, len(ch.chainDisputes))
		for key, d := range ch.chainDisputes {
			if !d.finalised {
				disputes[key] = d.timeout
			}
		}
		ch.subChsAccess.Unlock()

		if len(disputes) == 0 {
			// All known ledgers are already finalised — check if ready.
			break
		}

		// Wait for the nearest timeout among unfinalised ledgers.
		for key, timeout := range disputes {
			delay := time.Until(timeout)
			if delay > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(delay):
				}
			}

			// Mark this ledger as finalised.
			ch.subChsAccess.Lock()
			if d, ok := ch.chainDisputes[key]; ok {
				d.finalised = true
			}
			ch.subChsAccess.Unlock()

			log.WithFields(log.Fields{
				"channelID": ch.id,
				"ledger":    key.LedgerID,
			}).Debug("Ledger dispute window closed — marked finalised")
		}

		// Re-check: a ProgressedEvent may have reset a timeout while we were
		// waiting, so loop again to catch any newly unfinalised entries.
	}

	ch.subChsAccess.Lock()
	ready := ch.isReadyForCoordination()
	ch.subChsAccess.Unlock()

	if !ready {
		// Can only happen if chainDisputes is empty (no RegisteredEvent seen yet).
		log.WithField("channelID", ch.id).
			Warn("awaitFinalisationAndCoordinate: not ready despite empty pending set")
		return
	}

	log.WithField("channelID", ch.id).
		Info("ReadyForCoordination — all ledger windows closed, issuing coordinator certificate")

	if err := coordinator.coordinate(ctx, ch); err != nil {
		log.WithField("channelID", ch.id).Errorf("coordinate failed: %v", err)
	}
}

// handleProgressedEvent resets the dispute window for a specific ledger.
// Called when a ProgressedEvent arrives — the challenge window restarts.
func (ch *coordCh) handleProgressedEvent(ledgerKey multi.LedgerBackendKey) {
	ch.subChsAccess.Lock()
	defer ch.subChsAccess.Unlock()

	if d, ok := ch.chainDisputes[ledgerKey]; ok {
		d.timeout = time.Now().Add(
			time.Duration(ch.params.ChallengeDuration) * time.Second,
		)
		d.finalised = false
	}
}

func makeSignedState(params *channel.Params, tx channel.Transaction) channel.SignedState {
	return channel.SignedState{
		Params: params,
		State:  tx.State,
		Sigs:   tx.Sigs,
	}
}

// closeChain closes all chain subscriptions for a channel and waits
// for all goroutines to finish. Mirrors closePubSubs in the watcher.
func closeChain(ch *coordCh) {

	if err := ch.eventsFromChainSub.Close(); err != nil {
		err := errors.WithMessage(err, "closing events from chain sub")
		log.WithField("id", ch.id).Error(err.Error())
	}
	ch.wg.Wait()
}
