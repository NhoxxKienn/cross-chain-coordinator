package backends

import (
	"crypto/ecdsa"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/crypto"
	ethchannel "github.com/perun-network/perun-eth-backend/channel"
	ethwallet "github.com/perun-network/perun-eth-backend/wallet"
	swallet "github.com/perun-network/perun-eth-backend/wallet/simple"
	"perun.network/go-perun/channel/multi"
	"perun.network/go-perun/wallet"
)

func SetupMultiCoordinator(key *ecdsa.PrivateKey, coordinators []BackendCoordinatorConfig) (*multi.Coordinator, map[wallet.BackendID]wallet.Account, error) {
	eWallet := swallet.NewWallet(key)
	eacc := accounts.Account{Address: crypto.PubkeyToAddress(key.PublicKey)}
	addr := ethwallet.AsWalletAddr(eacc.Address)

	coordAcc := make(map[wallet.BackendID]wallet.Account)
	var err error
	coordAcc[1], err = eWallet.Unlock(addr)
	if err != nil {
		return nil, nil, fmt.Errorf("setup multi coordinator: unlock account %s: %w", addr, err)
	}

	coords := multi.NewCoordinator()
	for _, coordCfg := range coordinators {
		switch coordCfg.BackendID {
		case 1:
			ethLedgerID := ethchannel.MakeLedgerBackendID(big.NewInt(int64(coordCfg.LedgerID)))
			coord, err := newETHCoordinator(eWallet, coordCfg, eacc)
			if err != nil {
				return nil, nil, fmt.Errorf("setup multi coordinator: create ETH coordinator: %w", err)
			}
			coords.RegisterCoordinator(ethLedgerID, coord)
		default:
			panic(fmt.Sprintf("unsupported backend ID: %d", coordCfg.BackendID))
		}
	}

	return coords, coordAcc, nil
}
