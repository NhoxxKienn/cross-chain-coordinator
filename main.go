// Package main implements a relay server.
package main

import (
	"context"
	"cross-chain-coordinator/coordinator"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/libp2p/go-libp2p-core/crypto"
	"perun.network/go-perun/channel/multi"
)

func main() {
	mode := flag.String("mode", "relay", "Mode to run: relay | keygen")
	flushInterval := flag.Duration("flush", 30*24*time.Hour, "Flush interval")
	keyFile := flag.String("keyfile", "test_private.key", "Key file to use or generate")
	flag.Parse()

	switch *mode {
	case "relay":
		if err := runRelay(*flushInterval, *keyFile); err != nil {
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

func runRelay(flushInterval time.Duration, keyFile string) error {

	coord := multi.NewCoordinator()
	//TODO: setup coordinator with ETH-backend
	// coord.RegisterCoordinator()

	host, err := coordinator.SetupRelayCoordinator(keyFile, nil, coord)
	if err != nil {
		return err
	}
	log.Printf("Relay server started with ID: %s", host.ID())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	<-ctx.Done()

	log.Println("Shutting down server...")
	if err := host.Close(); err != nil {
		log.Printf("Failed to close host: %v", err)
		return err
	}
	log.Println("Server shut down successfully")
	return nil
}

func runKeygen(filename string) {
	priv, _, err := crypto.GenerateKeyPair(crypto.RSA, 2048)
	if err != nil {
		log.Fatalf("Key generation failed: %v", err)
	}

	privBytes, err := crypto.MarshalPrivateKey(priv)
	if err != nil {
		log.Fatalf("Failed to marshal private key: %v", err)
	}

	err = os.WriteFile(filename, privBytes, 0600)
	if err != nil {
		log.Fatalf("Failed to write private key to file: %v", err)
	}

	fmt.Printf("Key saved to %s\n", filename)
}
