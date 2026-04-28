package discovery

import "time"

const ScanResultTTL = 24 * time.Hour

type Config struct {
	ScanResultTTL time.Duration
}

func NewConfig() Config {
	return Config{ScanResultTTL: ScanResultTTL}
}
