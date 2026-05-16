package coordinator

import "perun.network/go-perun/channel"

const (
	relayID = "QmcxeYpYpYPX4J3478YZUaxFytYfUDbNe1jUWVYeZjL3gY"

	keySize = 2048

	NotifyWatchLedgerProtocolID = "/coordinator/notify-watch-ledger/1.0.0"
	NotifyWatchSubProtocolID    = "/coordinator/notify-watch-sub/1.0.0"
	NotifyStopWatchProtocolID   = "/coordinator/notify-stop-watch/1.0.0"

	responseStatusOK    = "ok"
	responseStatusError = "error"
)

type Response struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

type NotifyWatchLedgerChannelRequest struct {
	SignedState channel.SignedState `json:"signed_state"`
}

type NotifyWatchSubChannelRequest struct {
	ParentID    channel.ID          `json:"parent_id"`
	SignedState channel.SignedState `json:"signed_state"`
}

type NotifyStopWatchRequest struct {
	ID channel.ID `json:"id"`
}
