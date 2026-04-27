package discovery

import "time"

type Config struct {
	ScanResultTTL time.Duration // default 24h — purge old scan results
}

func NewConfig() Config {
	return Config{
		ScanResultTTL: 24 * time.Hour,
	}
}
