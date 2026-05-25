// Package main implements a relay server.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"time"

	"cross-chain-coordinator/backends"
	"cross-chain-coordinator/service"

	gocrypto "github.com/ethereum/go-ethereum/crypto"
	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
)

func main() {
	mode := flag.String("mode", "relay", "Mode to run: relay | keygen")
	flushInterval := flag.Duration("flush", 30*24*time.Hour, "Flush interval")
	keyFile := flag.String("keyfile", "test_private.key", "libp2p identity key file")
	configFile := flag.String("config", "devnet_config.yaml", "Coordinator config file")
	flag.Parse()

	switch *mode {
	case "relay":
		if err := runRelay(*flushInterval, *keyFile, *configFile); err != nil {
			log.Printf("Relay failed: %v", err)
			os.Exit(1)
		}
	case "keygen":
		runKeygen(*keyFile)
	default:
		fmt.Fprintf(os.Stderr, "unknown mode: %s\n", *mode)
		os.Exit(1)
	}
}

func runRelay(flushInterval time.Duration, keyFile, configFile string) error {
	_ = flushInterval

	cfg, err := backends.LoadConfig(configFile)
	if err != nil {
		return err
	}

	// Load libp2p identity key.
	keyBytes, err := os.ReadFile(keyFile)
	if err != nil {
		return fmt.Errorf("read libp2p key %s: %w", keyFile, err)
	}
	libp2pKey, err := libp2pcrypto.UnmarshalPrivateKey(keyBytes)
	if err != nil {
		return fmt.Errorf("parse libp2p key: %w", err)
	}

	// Load ECDSA signing key from the path in config.
	hexBytes, err := os.ReadFile(cfg.PrivateKeyPath)
	if err != nil {
		return fmt.Errorf("read signing key %s: %w", cfg.PrivateKeyPath, err)
	}
	ecdsaKey, err := gocrypto.HexToECDSA(strings.TrimSpace(string(hexBytes)))
	if err != nil {
		return fmt.Errorf("parse signing key: %w", err)
	}

	svc, err := service.New(cfg.Coordinators, ecdsaKey, libp2pKey)
	if err != nil {
		return err
	}
	log.Printf("Relay server started with ID: %s", svc.ID())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	<-ctx.Done()

	log.Println("Shutting down server...")
	if err := svc.Close(); err != nil {
		log.Printf("Failed to close host: %v", err)
		return err
	}
	log.Println("Server shut down successfully")
	return nil
}

func runKeygen(filename string) {
	priv, _, err := libp2pcrypto.GenerateKeyPair(libp2pcrypto.RSA, 2048)
	if err != nil {
		log.Fatalf("Key generation failed: %v", err)
	}

	privBytes, err := libp2pcrypto.MarshalPrivateKey(priv)
	if err != nil {
		log.Fatalf("Failed to marshal private key: %v", err)
	}

	err = os.WriteFile(filename, privBytes, 0600)
	if err != nil {
		log.Fatalf("Failed to write private key to file: %v", err)
	}

	fmt.Printf("Key saved to %s\n", filename)
}
