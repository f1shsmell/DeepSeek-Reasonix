package remotecloud

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// relayWriteDeadline bounds every write to the relay. A controller that stops
// reading must not hold the device socket hostage: the write fails, the
// connection closes, and the read loop reconnects.
var relayWriteDeadline atomic.Int64

func init() { relayWriteDeadline.Store(int64(10 * time.Second)) }

// relayWriter serializes the device socket's writers: command goroutines, push
// pumps and the connect loop share one connection, and gorilla/websocket
// allows only one concurrent write.
type relayWriter struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func (w *relayWriter) write(wire []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.conn.SetWriteDeadline(time.Now().Add(time.Duration(relayWriteDeadline.Load()))); err != nil {
		return err
	}
	if err := w.conn.WriteMessage(websocket.TextMessage, wire); err != nil {
		w.conn.Close()
		return err
	}
	return nil
}

func writeDirected(w *relayWriter, connectionID, payload string) error {
	wire, err := json.Marshal(map[string]string{"to": connectionID, "payload": payload})
	if err != nil {
		return err
	}
	if len(wire) > 64<<10 {
		return fmt.Errorf("remote cloud: reply exceeds relay limit")
	}
	return w.write(wire)
}
