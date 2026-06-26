// Package server implements the db-engine TCP server and wire protocol.
//
// Wire protocol
//
// Every message on the wire is a length-prefixed JSON frame:
//
//	[length: 4 bytes, big-endian uint32][body: <length> bytes of UTF-8 JSON]
//
// Client → Server (Request):
//
//	{"sql":"SELECT * FROM users"}
//
// Server → Client (Response, success):
//
//	{"columns":["id","name"],"rows":[["1","Alice"]],"message":""}
//
// Server → Client (Response, error):
//
//	{"error":"table \"foo\" does not exist"}
//
// The protocol is intentionally simple: one request per round-trip, fully
// synchronous on each connection. Pipelining is left for a future phase.
package server

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// maxFrameSize is the largest payload we will read in one frame (4 MiB).
// This guards against a misbehaving client sending a huge length prefix.
const maxFrameSize = 4 << 20

// Request is a client-to-server message carrying one SQL statement.
type Request struct {
	SQL string `json:"sql"`
}

// Response is a server-to-client message.
// On success, Columns/Rows/Message are populated and Error is empty.
// On failure, only Error is set.
type Response struct {
	Columns []string   `json:"columns,omitempty"`
	Rows    [][]string `json:"rows,omitempty"`
	Message string     `json:"message,omitempty"`
	Error   string     `json:"error,omitempty"`
}

// writeFrame encodes v as JSON and writes it as a length-prefixed frame.
func writeFrame(w io.Writer, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("proto marshal: %w", err)
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(payload)
	return err
}

// readFrame reads a length-prefixed frame and decodes the JSON body into v.
func readFrame(r io.Reader, v any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > maxFrameSize {
		return fmt.Errorf("proto: frame too large (%d bytes)", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	return json.Unmarshal(buf, v)
}
