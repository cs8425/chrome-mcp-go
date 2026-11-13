package cdp

//go:generate go run ./gen

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"
)

// ErrClosed is returned when a call is attempted on a closed connection.
var ErrClosed = errors.New("cdp: connection closed")

// rpcError is a JSON-RPC 2.0 error object on the CDP wire.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data"`
}

func (e *rpcError) Error() string { return e.Message }

type rpcResult struct {
	result json.RawMessage
	err    *rpcError
}

// Conn is one WebSocket connection to a Chrome DevTools endpoint (the browser
// endpoint, which multiplexes every attached target via sessionId, or a single
// page endpoint). It runs a reader goroutine that routes responses by id and
// events by method.
type Conn struct {
	ws   *websocket.Conn
	wmu  sync.Mutex // serialises writes (one conn, many callers)
	next atomic.Int64

	pmu     sync.Mutex
	pending map[int64]chan rpcResult

	emMu        sync.Mutex
	subscribers map[string][]*sub // key: method + "\x00" + sessionID

	closed    chan struct{}
	closeOnce sync.Once
}

type sub struct {
	sid  string
	fn   func(json.RawMessage)
	conn *Conn
	key  string
}

// Dial opens a CDP WebSocket connection and starts the read loop.
func Dial(ctx context.Context, wsURL string) (*Conn, error) {
	ws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return nil, err
	}
	// CDP responses can be very large (DOM trees, heap snapshots, screenshots);
	// disable the read limit.
	ws.SetReadLimit(-1)
	c := &Conn{
		ws:          ws,
		pending:     make(map[int64]chan rpcResult),
		subscribers: make(map[string][]*sub),
		closed:      make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

// DialBrowser connects to a browser's DevTools endpoint (Target management +
// flattened page sessions).
func DialBrowser(ctx context.Context, hostPort string) (*Conn, error) {
	info, err := version(ctx, hostPort)
	if err != nil {
		return nil, err
	}
	return Dial(ctx, info.WebSocketDebuggerURL)
}

type versionInfo struct {
	WebSocketDebuggerURL string `json:"WebsocketDebuggerUrl"`
	Browser              string `json:"Browser"`
}

func version(ctx context.Context, hostPort string) (*versionInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+hostPort+"/json/version", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var v versionInfo
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return nil, err
	}
	return &v, nil
}

// call sends a JSON-RPC request and waits for its response (routed by id).
func (c *Conn) call(ctx context.Context, sessionID, method string, params, result any) error {
	id := c.next.Add(1)
	ch := make(chan rpcResult, 1)
	c.pmu.Lock()
	c.pending[id] = ch
	c.pmu.Unlock()

	req := struct {
		ID        int64  `json:"id"`
		SessionID string `json:"sessionId,omitempty"`
		Method    string `json:"method"`
		Params    any    `json:"params,omitempty"`
	}{id, sessionID, method, params}

	if err := c.writeMsg(ctx, req); err != nil {
		c.drop(id)
		return err
	}

	select {
	case res := <-ch:
		if res.err != nil {
			return res.err
		}
		if result != nil && len(res.result) > 0 {
			return json.Unmarshal(res.result, result)
		}
		return nil
	case <-ctx.Done():
		c.drop(id)
		return ctx.Err()
	case <-c.closed:
		return ErrClosed
	}
}

func (c *Conn) writeMsg(ctx context.Context, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.ws.Write(ctx, websocket.MessageText, data)
}

func (c *Conn) drop(id int64) {
	c.pmu.Lock()
	delete(c.pending, id)
	c.pmu.Unlock()
}

func (c *Conn) readLoop() {
	ctx := context.Background()
	for {
		_, data, err := c.ws.Read(ctx)
		if err != nil {
			c.Close()
			return
		}
		var msg struct {
			ID        *int64          `json:"id"`
			SessionID string          `json:"sessionId"`
			Method    string          `json:"method"`
			Result    json.RawMessage `json:"result"`
			Error     *rpcError       `json:"error"`
			Params    json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		if msg.ID != nil {
			c.pmu.Lock()
			ch := c.pending[*msg.ID]
			delete(c.pending, *msg.ID)
			c.pmu.Unlock()
			if ch != nil {
				ch <- rpcResult{result: msg.Result, err: msg.Error}
			}
			continue
		}
		if msg.Method != "" {
			c.dispatchEvent(msg.Method, msg.SessionID, msg.Params)
		}
	}
}

func (c *Conn) dispatchEvent(method, sessionID string, params json.RawMessage) {
	c.emMu.Lock()
	subs := c.subscribers[method+"\x00"+sessionID]
	// Copy so handlers can (un)subscribe without holding the lock.
	snapshot := make([]*sub, len(subs))
	copy(snapshot, subs)
	c.emMu.Unlock()
	for _, s := range snapshot {
		c.safeCall(s.fn, params)
	}
}

// safeCall invokes an event handler with recovery so that a panicking handler
// (e.g. a bug in one subscriber) cannot take down the read loop and the whole
// connection/process. The panic is logged instead.
func (c *Conn) safeCall(fn func(json.RawMessage), params json.RawMessage) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("cdp: event handler panic recovered: %v", r)
		}
	}()
	fn(params)
}

// Subscribe registers a handler for a CDP event method. Pass sessionID "" to
// receive the event for every session (browser-level events). The returned
// func unsubscribes.
func (c *Conn) Subscribe(method, sessionID string, fn func(params json.RawMessage)) func() {
	key := method + "\x00" + sessionID
	s := &sub{sid: sessionID, fn: fn, conn: c, key: key}
	c.emMu.Lock()
	c.subscribers[key] = append(c.subscribers[key], s)
	c.emMu.Unlock()
	return func() {
		c.emMu.Lock()
		defer c.emMu.Unlock()
		subs := c.subscribers[key]
		for i, x := range subs {
			if x == s {
				c.subscribers[key] = append(subs[:i], subs[i+1:]...)
				break
			}
		}
	}
}

// Close closes the WebSocket connection.
func (c *Conn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		close(c.closed)
		// CloseNow: immediate, non-blocking (a graceful handshake can stall if
		// the peer already vanished; pending calls are released via c.closed).
		err = c.ws.CloseNow()
	})
	return err
}

// IsClosed reports whether the connection has been closed (for example because
// the browser went away). Callers use it to decide whether to (re)dial.
func (c *Conn) IsClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

// Client is a typed CDP client bound to a session ("" = browser-level). All
// generated protocol methods are defined on *Client.
type Client struct {
	conn      *Conn
	sessionID string
}

// NewClient returns a Client bound to the connection's given session.
func (c *Conn) Client(sessionID string) *Client {
	return &Client{conn: c, sessionID: sessionID}
}

func (c *Client) call(ctx context.Context, method string, params, result any) error {
	return c.conn.call(ctx, c.sessionID, method, params, result)
}

// RawCall sends an arbitrary CDP method (decoupled from the generated typed
// helpers — useful in the tool layer, where a stable shape matters more than
// full typing). result may be nil.
func (c *Client) RawCall(ctx context.Context, method string, params, result any) error {
	return c.call(ctx, method, params, result)
}

// SessionID returns the session this client is bound to ("" at browser level).
func (c *Client) SessionID() string { return c.sessionID }

// Subscribe registers a handler for an event on this client's session.
func (c *Client) Subscribe(method string, fn func(params json.RawMessage)) func() {
	return c.conn.Subscribe(method, c.sessionID, fn)
}
