package qq

import (
	"encoding/json"
	"fmt"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/channels"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// qqCreds holds the secret credentials for a QQ channel instance.
type qqCreds struct {
	AppID     string `json:"app_id"`
	AppSecret string `json:"app_secret"`
}

// qqConfig holds the non-secret configuration for a QQ channel instance.
type qqConfig struct {
	AllowFrom []string `json:"allow_from"`
}

// Factory creates a QQ Channel from DB instance data.
// Implements channels.ChannelFactory.
func Factory(name string, creds json.RawMessage, cfg json.RawMessage,
	msgBus *bus.MessageBus, _ store.PairingStore) (channels.Channel, error) {

	var c qqCreds
	if err := json.Unmarshal(creds, &c); err != nil {
		return nil, fmt.Errorf("qq: decode credentials: %w", err)
	}

	var ic qqConfig
	if len(cfg) > 0 {
		if err := json.Unmarshal(cfg, &ic); err != nil {
			return nil, fmt.Errorf("qq: decode config: %w", err)
		}
	}

	ch, err := newChannel(name, ic, c, msgBus)
	if err != nil {
		return nil, err
	}
	return ch, nil
}
