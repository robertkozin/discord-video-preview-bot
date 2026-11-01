package browser

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/cdp"
	"github.com/gorilla/websocket"
)

// wsConn is a cdp.WebSocketable backed by gorilla/websocket. rod's own
// websocket ignores frame opcodes, so when the server closes the session (e.g.
// browserless hitting its TIMEOUT) the close frame is parsed as json and rod
// panics in a background goroutine, crashing the process. gorilla turns close
// frames into a read error, which rod handles by failing pending calls.
type wsConn struct {
	conn    *websocket.Conn
	writeMu sync.Mutex // gorilla allows one concurrent writer
}

var _ cdp.WebSocketable = (*wsConn)(nil)

func (w *wsConn) Send(data []byte) error {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	return w.conn.WriteMessage(websocket.TextMessage, data)
}

func (w *wsConn) Read() ([]byte, error) {
	_, data, err := w.conn.ReadMessage()
	return data, err
}

// connectCDP opens a browser on a new connection to a ws(s):// CDP endpoint.
// close disconnects; for browserless that also ends the session.
func connectCDP(wsURL string) (b *rod.Browser, close func() error, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// gorilla rejects user:pass@ in websocket urls, so send it as basic auth
	u, err := url.Parse(wsURL)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing cdp url: %w", err)
	}
	header := http.Header{}
	if u.User != nil {
		password, _ := u.User.Password()
		auth := base64.StdEncoding.EncodeToString([]byte(u.User.Username() + ":" + password))
		header.Set("Authorization", "Basic "+auth)
		u.User = nil
	}

	conn, resp, err := websocket.DefaultDialer.DialContext(ctx, u.String(), header)
	if err != nil {
		if resp != nil {
			return nil, nil, fmt.Errorf("connecting to cdp: %s: %w", resp.Status, err)
		}
		return nil, nil, fmt.Errorf("connecting to cdp: %w", err)
	}

	client := cdp.New().Start(&wsConn{conn: conn})
	b = rod.New().Client(client)
	if err := b.Connect(); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("connecting to cdp: %w", err)
	}

	return b, func() error {
		_ = b.Close()
		return conn.Close()
	}, nil
}
