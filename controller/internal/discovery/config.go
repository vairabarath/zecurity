package discovery

import "time"

const ScanResultTTL = 24 * time.Hour

type DiscoveryConfig struct {
	ScanResultTTL time.Duration
}

func DefaultConfig() DiscoveryConfig {
	return DiscoveryConfig{ScanResultTTL: ScanResultTTL}
}
