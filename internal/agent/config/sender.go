package config

import "time"

type TickOrBatchSenderParams struct {
	BatchSize     int           `yaml:"batch_size"`
	FlushInterval time.Duration `yaml:"flush_interval"`
}

type ReliableSenderParams struct {
	SpoolPath         string              `yaml:"spool_path"`
	MaxSpoolBytes     int64               `yaml:"max_spool_bytes"`
	MaxRecordBytes    int64               `yaml:"max_record_bytes"`
	BatchSize         int                 `yaml:"batch_size"`
	FlushInterval     time.Duration       `yaml:"flush_interval"`
	HeartbeatInterval time.Duration       `yaml:"heartbeat_interval"`
	Retry             ReliableRetryParams `yaml:"retry"`
}

type ReliableRetryParams struct {
	BaseDelay time.Duration `yaml:"base_delay"`
	MaxDelay  time.Duration `yaml:"max_delay"`
	Jitter    float64       `yaml:"jitter"`
}
