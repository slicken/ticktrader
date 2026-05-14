package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"ticktrader/config"
	"ticktrader/exchange"
	"ticktrader/exchange/hyperliquid"
	"ticktrader/exchange/lighter"
	"ticktrader/model"
)

var (
	configFile    = "config.json"
	dashboardAddr = ":8080"
	exch          exchange.I
	modl          *model.Marketmaker
	dashboard     *model.Dashboard
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if len(os.Args) > 1 {
		configFile = os.Args[1]
	}

	// load config
	log.Printf("Loading %s...\n", configFile)
	cfg, err := config.LoadConfig(configFile, ctx)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	cfg.Model.Print()

	// Load exactly one enabled exchange (the first one in config order).
	for i := range cfg.Exchanges {
		if !cfg.Exchanges[i].Enabled {
			continue
		}
		exchangeCfg := &cfg.Exchanges[i]
		log.Printf("Loading exchange %s...\n", exchangeCfg.Name)
		if exch, err = loadExchange(exchangeCfg); err != nil {
			log.Fatalf("Failed to load exchange %s: %v", exchangeCfg.Name, err)
		}
		if err := exch.Start(ctx); err != nil {
			log.Fatalf("Failed to start exchange %s: %v", exchangeCfg.Name, err)
		}
		break
	}
	if exch == nil {
		log.Fatal("No enabled exchange found in config")
	}

	log.Println("Loading model...")
	modl = model.Initialize(exch, &cfg.Model)
	modl.Cfg = cfg
	go modl.Start(ctx)

	log.Printf("Starting dashboard [http://localhost%s]", dashboardAddr)
	dashboard = model.NewDashboard(modl, dashboardAddr)
	go dashboard.Start(ctx)

	// handle graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	fmt.Println(<-sigChan)
	log.Println("Shutting down...")

	cancel()

	if exch != nil {
		if err := exch.Stop(); err != nil {
			log.Printf("Failed to stop exchange %s: %v", exch.Name(), err)
		}
	}

	time.Sleep(2 * time.Second)
}

func loadExchange(cfg *config.ExchangeConfig) (exchange.I, error) {
	switch strings.ToLower(cfg.Name) {
	case "hyperliquid":
		return hyperliquid.NewHyperliquid(cfg), nil
	case "lighter", "lighter.xyz":
		return lighter.NewLighter(cfg), nil
	default:
		return nil, fmt.Errorf("unsupported exchange %q", cfg.Name)
	}
}
