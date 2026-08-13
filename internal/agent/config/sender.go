package config

import "time"

type TickOrBatchSenderParams struct {
	BatchSize     int           `yaml:"batch_size"`
	FlushInterval time.Duration `yaml:"flush_interval"`
}
