package qq

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/channels"
	"github.com/tencent-connect/botgo"
	"github.com/tencent-connect/botgo/dto"
	"github.com/tencent-connect/botgo/log"
	"github.com/tencent-connect/botgo/openapi"
	"github.com/tencent-connect/botgo/token"
	"golang.org/x/oauth2"
)

// filteredLogger silences botgo SDK logs.
type filteredLogger struct{}

func (f *filteredLogger) Debug(v ...interface{})                 {}
func (f *filteredLogger) Info(v ...interface{})                  {}
func (f *filteredLogger) Warn(v ...interface{})                  {}
func (f *filteredLogger) Error(v ...interface{})                 {}
func (f *filteredLogger) Debugf(format string, v ...interface{}) {}
func (f *filteredLogger) Infof(format string, v ...interface{})  {}
func (f *filteredLogger) Warnf(format string, v ...interface{})  {}
func (f *filteredLogger) Errorf(format string, v ...interface{}) {}
func (f *filteredLogger) Sync() error                            { return nil }

// wsPayload represents a WebSocket message payload.
type wsPayload struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
	S  uint32          `json:"s"`
	T  string          `json:"t"`
}

// helloData represents the Hello event data.
type helloData struct {
	HeartbeatInterval int `json:"heartbeat_interval"`
}

// readyData represents the Ready event data.
type readyData struct {
	SessionID string `json:"session_id"`
	Version   int    `json:"version"`
	User      struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Bot      bool   `json:"bot"`
	} `json:"user"`
}

// c2cMessageEventData represents a C2C message event.
type c2cMessageEventData struct {
	ID        string `json:"id"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp"`
	Author    struct {
		UserOpenID string `json:"user_openid"`
	} `json:"author"`
}

// groupATMessageEventData represents a group @message event.
type groupATMessageEventData struct {
	ID        string `json:"id"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp"`
	Author    struct {
		MemberOpenID string `json:"member_openid"`
	} `json:"author"`
	GroupOpenID string `json:"group_openid"`
}

// channelATMessageEventData represents a channel @message event.
type channelATMessageEventData struct {
	ID        string `json:"id"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp"`
	Author    struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"author"`
	ChannelID string `json:"channel_id"`
	GuildID   string `json:"guild_id"`
}

// Channel implements the QQ Official Bot API channel.
type Channel struct {
	*channels.BaseChannel
	appID        string
	appSecret    string
	api          openapi.OpenAPI
	tokenSource  oauth2.TokenSource
	tokenCancel  context.CancelFunc
	session      *dto.WebsocketAP
	ctx          context.Context
	cancel       context.CancelFunc
	conn         *websocket.Conn
	connMu       sync.Mutex
	mu           sync.RWMutex
	sessionID    string
	lastSeq      uint32
	heartbeatInt int
	accessToken  string
	msgSeqMap    map[string]int64
}

// newChannel creates a new QQ channel.
func newChannel(name string, cfg qqConfig, creds qqCreds, msgBus *bus.MessageBus) (*Channel, error) {
	if creds.AppID == "" || creds.AppSecret == "" {
		return nil, fmt.Errorf("qq: app_id and app_secret are required")
	}

	base := channels.NewBaseChannel(name, msgBus, cfg.AllowFrom)
	ch := &Channel{
		BaseChannel: base,
		appID:       creds.AppID,
		appSecret:   creds.AppSecret,
		msgSeqMap:   make(map[string]int64),
	}
	ch.SetType(channels.TypeQQ)
	ch.MarkRegistered("configured")
	return ch, nil
}

// Start starts the QQ channel.
func (c *Channel) Start(ctx context.Context) error {
	c.MarkStarting("connecting to QQ gateway")

	// Silence botgo SDK logs.
	log.DefaultLogger = &filteredLogger{}

	credentials := &token.QQBotCredentials{
		AppID:     c.appID,
		AppSecret: c.appSecret,
	}
	c.tokenSource = token.NewQQBotTokenSource(credentials)

	tokenCtx, cancel := context.WithCancel(context.Background())
	c.tokenCancel = cancel
	if err := token.StartRefreshAccessToken(tokenCtx, c.tokenSource); err != nil {
		c.MarkFailed("failed to start token refresh", err.Error(), channels.ChannelFailureKindAuth, false)
		return fmt.Errorf("qq: failed to start token refresh: %w", err)
	}

	c.api = botgo.NewOpenAPI(c.appID, c.tokenSource).WithTimeout(10 * time.Second).SetDebug(false)

	c.ctx, c.cancel = context.WithCancel(ctx)
	go c.connectWebSocket(c.ctx)

	c.MarkHealthy("connected to QQ gateway")
	c.SetRunning(true)
	return nil
}

// connectWebSocket manages the WebSocket connection with auto-reconnect.
func (c *Channel) connectWebSocket(ctx context.Context) {
	reconnectDelay := 1000 * time.Millisecond
	maxDelay := 60 * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		default:
			if err := c.doConnect(ctx); err != nil {
				c.MarkDegraded("connection failed, retrying", err.Error(), channels.ChannelFailureKindNetwork, true)
				time.Sleep(reconnectDelay)
				reconnectDelay *= 2
				if reconnectDelay > maxDelay {
					reconnectDelay = maxDelay
				}
			} else {
				reconnectDelay = 1000 * time.Millisecond
				c.MarkHealthy("connected to QQ gateway")
				c.waitForConnection(ctx)
			}
		}
	}
}

// doConnect performs a single connection attempt.
func (c *Channel) doConnect(ctx context.Context) error {
	tok, err := c.tokenSource.Token()
	if err != nil {
		return fmt.Errorf("failed to get access token: %w", err)
	}
	c.accessToken = tok.AccessToken

	wsResp, err := c.api.WS(ctx, map[string]string{}, "")
	if err != nil {
		return fmt.Errorf("failed to get websocket URL: %w", err)
	}

	c.mu.Lock()
	c.session = wsResp
	c.mu.Unlock()

	dialer := websocket.DefaultDialer
	conn, _, err := dialer.DialContext(ctx, wsResp.URL, nil)
	if err != nil {
		return fmt.Errorf("failed to dial websocket: %w", err)
	}

	c.connMu.Lock()
	c.conn = conn
	c.connMu.Unlock()

	return c.waitForHello(ctx)
}

// waitForHello waits for and processes the Hello message.
func (c *Channel) waitForHello(ctx context.Context) error {
	c.connMu.Lock()
	conn := c.conn
	c.connMu.Unlock()

	if conn == nil {
		return fmt.Errorf("connection is nil")
	}

	_, message, err := conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("failed to read Hello message: %w", err)
	}

	var payload wsPayload
	if err := json.Unmarshal(message, &payload); err != nil {
		return fmt.Errorf("failed to parse Hello message: %w", err)
	}

	if payload.Op != 10 {
		return fmt.Errorf("expected Hello (op=10), got op=%d", payload.Op)
	}

	var hd helloData
	if err := json.Unmarshal(payload.D, &hd); err != nil {
		return fmt.Errorf("failed to parse Hello data: %w", err)
	}

	c.heartbeatInt = hd.HeartbeatInterval

	if c.sessionID != "" {
		return c.sendResume()
	}
	return c.sendIdentify()
}

// sendIdentify sends the Identify payload.
func (c *Channel) sendIdentify() error {
	// Intents: group chat + DM + guild + guild members + guild messages
	intents := (1 << 25) | (1 << 12) | (1 << 30) | (1 << 0) | (1 << 1)

	payload := map[string]interface{}{
		"op": 2,
		"d": map[string]interface{}{
			"token":   fmt.Sprintf("QQBot %s", c.accessToken),
			"intents": intents,
			"shard":   []uint32{0, 1},
		},
	}

	c.connMu.Lock()
	defer c.connMu.Unlock()

	if c.conn == nil {
		return fmt.Errorf("connection is nil")
	}
	return c.conn.WriteJSON(payload)
}

// sendResume sends the Resume payload.
func (c *Channel) sendResume() error {
	payload := map[string]interface{}{
		"op": 6,
		"d": map[string]interface{}{
			"token":      fmt.Sprintf("QQBot %s", c.accessToken),
			"session_id": c.sessionID,
			"seq":        c.lastSeq,
		},
	}

	c.connMu.Lock()
	defer c.connMu.Unlock()

	if c.conn == nil {
		return fmt.Errorf("connection is nil")
	}
	return c.conn.WriteJSON(payload)
}

// waitForConnection keeps the connection alive and processes messages.
func (c *Channel) waitForConnection(ctx context.Context) {
	c.connMu.Lock()
	conn := c.conn
	c.connMu.Unlock()

	if conn == nil {
		return
	}

	heartbeatTicker := time.NewTicker(time.Duration(c.heartbeatInt) * time.Millisecond)
	defer heartbeatTicker.Stop()

	messageChan := make(chan []byte, 100)
	errorChan := make(chan error, 1)

	go func() {
		for {
			c.connMu.Lock()
			currentConn := c.conn
			c.connMu.Unlock()

			if currentConn == nil {
				errorChan <- fmt.Errorf("connection closed")
				return
			}

			_, message, err := currentConn.ReadMessage()
			if err != nil {
				errorChan <- err
				return
			}
			messageChan <- message
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeatTicker.C:
			c.sendHeartbeat()
		case message := <-messageChan:
			c.handleMessage(message)
		case <-errorChan:
			return
		}
	}
}

// sendHeartbeat sends a heartbeat message.
func (c *Channel) sendHeartbeat() {
	c.connMu.Lock()
	defer c.connMu.Unlock()

	if c.conn == nil {
		return
	}
	payload := map[string]interface{}{
		"op": 1,
		"d":  c.lastSeq,
	}
	_ = c.conn.WriteJSON(payload)
}

// handleMessage processes a WebSocket message.
func (c *Channel) handleMessage(message []byte) {
	var payload wsPayload
	if err := json.Unmarshal(message, &payload); err != nil {
		return
	}

	if payload.S > 0 {
		c.lastSeq = payload.S
	}

	switch payload.Op {
	case 0: // Dispatch
		c.handleDispatch(payload.T, payload.D)
	case 1: // Heartbeat ACK
	case 7: // Reconnect
	default:
	}
}

// handleDispatch routes dispatch events.
func (c *Channel) handleDispatch(eventType string, data json.RawMessage) {
	switch eventType {
	case "READY":
		c.handleReady(data)
	case "RESUMED":
	case "C2C_MESSAGE_CREATE":
		c.handleC2CMessage(data)
	case "GROUP_AT_MESSAGE_CREATE":
		c.handleGroupATMessage(data)
	case "AT_MESSAGE_CREATE":
		c.handleChannelATMessage(data)
	case "DIRECT_MESSAGE_CREATE":
		// Channel DM: not handled for now.
	default:
	}
}

// handleReady processes the Ready event.
func (c *Channel) handleReady(data json.RawMessage) {
	var rd readyData
	if err := json.Unmarshal(data, &rd); err != nil {
		return
	}
	c.sessionID = rd.SessionID
}

// handleC2CMessage processes C2C direct messages.
func (c *Channel) handleC2CMessage(data json.RawMessage) {
	var event c2cMessageEventData
	if err := json.Unmarshal(data, &event); err != nil {
		return
	}

	senderID := event.Author.UserOpenID
	if !c.IsAllowed(senderID) {
		return
	}

	metadata := map[string]string{
		"chat_type": "c2c",
		"msg_id":    event.ID,
	}

	c.HandleMessage(senderID, senderID, event.Content, nil, metadata, "direct")
}

// handleGroupATMessage processes group @messages.
func (c *Channel) handleGroupATMessage(data json.RawMessage) {
	var event groupATMessageEventData
	if err := json.Unmarshal(data, &event); err != nil {
		return
	}

	senderID := event.Author.MemberOpenID
	if !c.IsAllowed(senderID) && !c.IsAllowed(event.GroupOpenID) {
		return
	}

	metadata := map[string]string{
		"chat_type":     "group",
		"group_id":      event.GroupOpenID,
		"member_openid": senderID,
		"msg_id":        event.ID,
	}

	c.HandleMessage(senderID, event.GroupOpenID, event.Content, nil, metadata, "group")
}

// handleChannelATMessage processes channel @messages.
func (c *Channel) handleChannelATMessage(data json.RawMessage) {
	var event channelATMessageEventData
	if err := json.Unmarshal(data, &event); err != nil {
		return
	}

	senderID := event.Author.ID
	if !c.IsAllowed(senderID) && !c.IsAllowed(event.ChannelID) {
		return
	}

	metadata := map[string]string{
		"chat_type":  "channel",
		"channel_id": event.ChannelID,
		"group_id":   event.GuildID,
		"msg_id":     event.ID,
	}

	c.HandleMessage(senderID, event.ChannelID, event.Content, nil, metadata, "group")
}

// Send delivers an outbound message via the QQ API.
func (c *Channel) Send(ctx context.Context, msg bus.OutboundMessage) error {
	if c.api == nil {
		return fmt.Errorf("qq: API not initialized")
	}

	messageToSend := &dto.MessageToCreate{
		Content:   msg.Content,
		Timestamp: time.Now().UnixMilli(),
	}

	chatType := msg.Metadata["chat_type"]
	switch chatType {
	case "group":
		return c.sendGroupMessage(ctx, msg.ChatID, messageToSend)
	case "channel":
		return c.sendChannelMessage(ctx, msg.ChatID, messageToSend)
	default:
		return c.sendC2CMessage(ctx, msg.ChatID, messageToSend)
	}
}

// sendC2CMessage sends a C2C message.
func (c *Channel) sendC2CMessage(ctx context.Context, openID string, msg *dto.MessageToCreate) error {
	_, err := c.api.PostC2CMessage(ctx, openID, msg)
	if err != nil {
		c.handleAPIError(err)
	}
	return err
}

// sendGroupMessage sends a group message.
func (c *Channel) sendGroupMessage(ctx context.Context, groupID string, msg *dto.MessageToCreate) error {
	_, err := c.api.PostGroupMessage(ctx, groupID, msg)
	if err != nil {
		c.handleAPIError(err)
	}
	return err
}

// sendChannelMessage sends a channel (guild) message.
func (c *Channel) sendChannelMessage(ctx context.Context, channelID string, msg *dto.MessageToCreate) error {
	_, err := c.api.PostMessage(ctx, channelID, msg)
	if err != nil {
		c.handleAPIError(err)
	}
	return err
}

// handleAPIError maps API errors to channel health states.
func (c *Channel) handleAPIError(err error) {
	if err == nil {
		return
	}
	// botgo errors are typically wrapped; degrade generically.
	c.MarkDegraded("api error", err.Error(), channels.ChannelFailureKindUnknown, true)
}

// Stop gracefully shuts down the channel.
func (c *Channel) Stop(_ context.Context) error {
	if c.tokenCancel != nil {
		c.tokenCancel()
	}
	if c.cancel != nil {
		c.cancel()
	}
	c.closeConnection()
	c.SetRunning(false)
	c.MarkStopped("stopped")
	return nil
}

// closeConnection closes the WebSocket connection.
func (c *Channel) closeConnection() {
	c.connMu.Lock()
	conn := c.conn
	c.conn = nil
	c.connMu.Unlock()

	if conn != nil {
		conn.Close()
	}
}
