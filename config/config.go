package config

import (
	"context"
	"encoding/json"

	"fmt"
	"log"
	"os"
	"time"
)

// Config holds all configuration parameters
type Config struct {
	DynamicConfig bool             `json:"enable_dynamic_config,omitempty"`
	Exchanges     []ExchangeConfig `json:"exchanges"` // Required
	Model         ModelConfig      `json:"model,omitempty"`
}

// ExchangeConfig holds exchange-specific configuration
type ExchangeConfig struct {
	Name        string `json:"name"`                 // Exchange name (e.g., "hyperliquid", "lighter")
	Enabled     bool   `json:"enabled"`              // Enable or disable the exchange
	Testnet     bool   `json:"testnet,omitempty"`    // Use testnet or mainnet
	BaseURL     string `json:"base_url,omitempty"`   // Optional exchange REST endpoint override
	APIKey      string `json:"api_key,omitempty"`    // Exchange API key or account address, depending on venue
	APISecret   string `json:"api_secret,omitempty"` // Exchange API secret/private key
	APIKeyIndex uint8  `json:"api_key_index,omitempty"`
	Debug       bool   `json:"debug,omitempty"` // Enable debug logging for orders and positions
}

// ModelConfig holds all model parameters
type ModelConfig struct {
	// Trading Pair Parameters
	Pairs         []string `json:"pairs"`          // Trading pairs (e.g., ["BTC", "ETH", "SOL"])
	DisabledPairs []string `json:"pairs_disabled"` // Pairs to exclude from trading (e.g., ["USDT", "USDC"])
}

// LoadConfig loads configuration from a file
func LoadConfig(path string, ctx context.Context) (*Config, error) {
	cfg := &Config{}

	// Read config file
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	// Parse JSON
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// Start config reloader goroutine
	if cfg.DynamicConfig {
		log.Println("Dynamic config is enabled. (reloading every minute)")

		go func() {
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()

			var lastModTime time.Time
			stat, err := os.Stat(path)
			if err == nil {
				lastModTime = stat.ModTime()
			}

			for {
				select {
				case <-ticker.C:
					// Check if file has been modified
					stat, err := os.Stat(path)
					if err != nil {
						continue
					}

					if stat.ModTime().After(lastModTime) {
						// Reload config
						newData, err := os.ReadFile(path)
						if err != nil {
							log.Printf("Failed to read config file: %v\n", err)
							continue
						}

						if err := json.Unmarshal(newData, cfg); err != nil {
							log.Printf("Failed to parse config file: %v\n", err)
							continue
						}

						lastModTime = stat.ModTime()
						log.Println("Config reloaded successfully")
					}
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	return cfg, nil
}

func (sc *ModelConfig) Print() {
	log.Println()
	log.Printf("MODEL CONFIGURATION:")
	log.Println("Trader Settings:")
	log.Printf("  Pairs                : %v", sc.Pairs)
	log.Printf("  Disabled Pairs       : %v", sc.DisabledPairs)
	log.Println()
}
