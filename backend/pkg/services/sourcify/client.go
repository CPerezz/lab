package sourcify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

const (
	// DefaultAPIBaseURL is the default Sourcify API base URL
	DefaultAPIBaseURL = "https://sourcify.dev/server"
	// DefaultTimeout for HTTP requests
	DefaultTimeout = 10 * time.Second
	// DefaultCacheTTL is how long to cache contract metadata
	DefaultCacheTTL = 24 * time.Hour
)

// Client provides methods to interact with the Sourcify API
type Client struct {
	baseURL    string
	httpClient *http.Client
	cache      *contractCache
	log        logrus.FieldLogger
}

// Config holds configuration for the Sourcify client
type Config struct {
	BaseURL  string
	Timeout  time.Duration
	CacheTTL time.Duration
}

// NewClient creates a new Sourcify API client
func NewClient(log logrus.FieldLogger, cfg *Config) *Client {
	if cfg == nil {
		cfg = &Config{}
	}

	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultAPIBaseURL
	}

	if cfg.Timeout == 0 {
		cfg.Timeout = DefaultTimeout
	}

	if cfg.CacheTTL == 0 {
		cfg.CacheTTL = DefaultCacheTTL
	}

	return &Client{
		baseURL: cfg.BaseURL,
		httpClient: &http.Client{
			Timeout: cfg.Timeout,
		},
		cache: newContractCache(cfg.CacheTTL),
		log:   log.WithField("component", "sourcify_client"),
	}
}

// ContractMetadata represents the metadata returned from Sourcify API
type ContractMetadata struct {
	Metadata struct {
		Settings struct {
			CompilationTarget map[string]string `json:"compilationTarget"`
		} `json:"settings"`
	} `json:"metadata"`
	ChainID string `json:"chainId"`
	Address string `json:"address"`
}

// GetContractName retrieves the contract name for a given address and chain ID
// Returns empty string if contract is not verified or name cannot be determined
func (c *Client) GetContractName(ctx context.Context, chainID int64, address string) string {
	// Check cache first
	if name, found := c.cache.get(chainID, address); found {
		return name
	}

	// Fetch from Sourcify API
	name := c.fetchContractName(ctx, chainID, address)

	// Cache the result (even if empty, to avoid repeated failed lookups)
	c.cache.set(chainID, address, name)

	return name
}

// fetchContractName makes the actual API call to Sourcify
func (c *Client) fetchContractName(ctx context.Context, chainID int64, address string) string {
	url := fmt.Sprintf("%s/v2/contract/%d/%s?fields=metadata", c.baseURL, chainID, address)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		c.log.WithError(err).WithFields(logrus.Fields{
			"chain_id": chainID,
			"address":  address,
		}).Debug("Failed to create Sourcify request")
		return ""
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.log.WithError(err).WithFields(logrus.Fields{
			"chain_id": chainID,
			"address":  address,
		}).Debug("Failed to fetch contract from Sourcify")
		return ""
	}
	defer resp.Body.Close()

	// If contract not found (404) or any error, return empty string
	if resp.StatusCode != http.StatusOK {
		c.log.WithFields(logrus.Fields{
			"chain_id":    chainID,
			"address":     address,
			"status_code": resp.StatusCode,
		}).Debug("Contract not found or not verified on Sourcify")
		return ""
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		c.log.WithError(err).WithFields(logrus.Fields{
			"chain_id": chainID,
			"address":  address,
		}).Debug("Failed to read Sourcify response")
		return ""
	}

	var metadata ContractMetadata
	if err := json.Unmarshal(body, &metadata); err != nil {
		c.log.WithError(err).WithFields(logrus.Fields{
			"chain_id": chainID,
			"address":  address,
		}).Debug("Failed to parse Sourcify metadata")
		return ""
	}

	// Extract contract name from compilationTarget
	// The compilationTarget is a map like {"TetherToken.sol": "TetherToken"}
	// where the value is the contract name
	for _, contractName := range metadata.Metadata.Settings.CompilationTarget {
		if contractName != "" {
			c.log.WithFields(logrus.Fields{
				"chain_id":      chainID,
				"address":       address,
				"contract_name": contractName,
			}).Debug("Successfully resolved contract name from Sourcify")
			return contractName
		}
	}

	c.log.WithFields(logrus.Fields{
		"chain_id": chainID,
		"address":  address,
	}).Debug("Contract found on Sourcify but no contract name in compilationTarget")

	return ""
}

// contractCache provides thread-safe caching for contract names
type contractCache struct {
	mu      sync.RWMutex
	data    map[string]cacheEntry
	ttl     time.Duration
	cleanup *time.Ticker
	done    chan bool
}

type cacheEntry struct {
	name      string
	expiresAt time.Time
}

func newContractCache(ttl time.Duration) *contractCache {
	c := &contractCache{
		data:    make(map[string]cacheEntry),
		ttl:     ttl,
		cleanup: time.NewTicker(1 * time.Hour),
		done:    make(chan bool),
	}

	// Start cleanup goroutine
	go c.cleanupLoop()

	return c
}

func (c *contractCache) get(chainID int64, address string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := fmt.Sprintf("%d:%s", chainID, address)
	entry, found := c.data[key]
	if !found {
		return "", false
	}

	// Check if expired
	if time.Now().After(entry.expiresAt) {
		return "", false
	}

	return entry.name, true
}

func (c *contractCache) set(chainID int64, address string, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := fmt.Sprintf("%d:%s", chainID, address)
	c.data[key] = cacheEntry{
		name:      name,
		expiresAt: time.Now().Add(c.ttl),
	}
}

func (c *contractCache) cleanupLoop() {
	for {
		select {
		case <-c.cleanup.C:
			c.removeExpired()
		case <-c.done:
			return
		}
	}
}

func (c *contractCache) removeExpired() {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	for key, entry := range c.data {
		if now.After(entry.expiresAt) {
			delete(c.data, key)
		}
	}
}

func (c *contractCache) Stop() {
	c.cleanup.Stop()
	c.done <- true
}
