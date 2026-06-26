// Package client provides a TCP client for db-engine.
//
// Usage:
//
//	c, err := client.Dial("localhost:5433")
//	if err != nil { ... }
//	defer c.Close()
//
//	res, err := c.Exec("SELECT * FROM users")
//	if err != nil { ... }
//	for _, row := range res.Rows {
//	    fmt.Println(row)
//	}
package client

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
)

const maxFrameSize = 4 << 20

// Result holds the response from a successful Exec call.
type Result struct {
	Columns []string
	Rows    [][]string
	Message string
}

// Client is a connection to a db-engine server.
// A single Client must not be used from multiple goroutines concurrently;
// create one Client per goroutine (or use a pool).
type Client struct {
	conn net.Conn
	mu   sync.Mutex
}

// Dial opens a TCP connection to the db-engine server at addr.
func Dial(addr string) (*Client, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial %q: %w", addr, err)
	}
	return &Client{conn: conn}, nil
}

// Exec sends sql to the server and returns the result.
// Returns an error if the server responds with an error message or if the
// network connection fails.
func (c *Client) Exec(sql string) (*Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := writeFrame(c.conn, map[string]string{"sql": sql}); err != nil {
		return nil, fmt.Errorf("send: %w", err)
	}

	var resp struct {
		Columns []string   `json:"columns"`
		Rows    [][]string `json:"rows"`
		Message string     `json:"message"`
		Error   string     `json:"error"`
	}
	if err := readFrame(c.conn, &resp); err != nil {
		return nil, fmt.Errorf("recv: %w", err)
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("%s", resp.Error)
	}
	return &Result{Columns: resp.Columns, Rows: resp.Rows, Message: resp.Message}, nil
}

// Close closes the underlying TCP connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

// --- wire helpers (mirrored from server/proto.go) ---

func writeFrame(w io.Writer, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(payload)
	return err
}

func readFrame(r io.Reader, v any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > maxFrameSize {
		return fmt.Errorf("frame too large (%d bytes)", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	return json.Unmarshal(buf, v)
}
