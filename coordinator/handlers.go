package coordinator

import (
	"encoding/json"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/pkg/errors"
)

func (c *CoordinatorHost) handleNotifyWatchLedger(s network.Stream) {
	defer s.Close()

	var req NotifyWatchLedgerChannelRequest
	if err := json.NewDecoder(s).Decode(&req); err != nil {
		writeResponse(s, responseStatusError, errors.WithMessage(err, "decoding request").Error())
		return
	}

	if err := c.startWatchingLedger(c.ctx, req); err != nil {
		writeResponse(s, responseStatusError, err.Error())
		return
	}

	writeResponse(s, responseStatusOK, "")
}

func (c *CoordinatorHost) handleNotifyWatchSub(s network.Stream) {
	defer s.Close()

	var req NotifyWatchSubChannelRequest
	if err := json.NewDecoder(s).Decode(&req); err != nil {
		writeResponse(s, responseStatusError, errors.WithMessage(err, "decoding request").Error())
		return
	}

	if err := c.startWatchingSub(c.ctx, req); err != nil {
		writeResponse(s, responseStatusError, err.Error())
		return
	}

	writeResponse(s, responseStatusOK, "")
}

func (c *CoordinatorHost) handleNotifyStopWatch(s network.Stream) {
	defer s.Close()

	var req NotifyStopWatchRequest
	if err := json.NewDecoder(s).Decode(&req); err != nil {
		writeResponse(s, responseStatusError, errors.WithMessage(err, "decoding request").Error())
		return
	}

	if err := c.stopWatching(req.ID); err != nil {
		writeResponse(s, responseStatusError, err.Error())
		return
	}

	writeResponse(s, responseStatusOK, "")
}

func writeResponse(s network.Stream, status, reason string) {
	resp := Response{Status: status, Reason: reason}
	_ = json.NewEncoder(s).Encode(resp)
}
