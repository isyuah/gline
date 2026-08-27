package config

type GlineDestinationParams struct {
	URL          string `yaml:"url"`
	HeartbeatURL string `yaml:"heartbeat_url"`
	Token        string `yaml:"token"`
}
