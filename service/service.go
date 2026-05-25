package service

import (
	"crypto/ecdsa"

	"cross-chain-coordinator/backends"
	"cross-chain-coordinator/coordinator"

	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
)

// Service wraps a running CoordinatorHost. Call Close to shut it down.
type Service struct {
	*coordinator.CoordinatorHost
}

// New wires the ETH backend and starts the libp2p relay coordinator.
// coordinators is the per-chain config (chain URL, adjudicator address, etc.).
// signingKey is the ECDSA key used to sign coordinator certificates on-chain.
// libp2pKey is the stable identity key for this coordinator's peer.ID.
func New(
	coordinators []backends.BackendCoordinatorConfig,
	signingKey *ecdsa.PrivateKey,
	libp2pKey libp2pcrypto.PrivKey,
) (*Service, error) {
	coord, acc, err := backends.SetupMultiCoordinator(signingKey, coordinators)
	if err != nil {
		return nil, err
	}

	host, err := coordinator.SetupRelayCoordinator(libp2pKey, acc, coord)
	if err != nil {
		return nil, err
	}

	return &Service{CoordinatorHost: host}, nil
}
