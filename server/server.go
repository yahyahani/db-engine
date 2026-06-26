package server

import (
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/yahya/db-engine/executor"
)

// Server listens on a TCP port and dispatches SQL queries to a DB instance.
// Each client connection is handled in its own goroutine, which means the
// executor's per-goroutine MVCC transaction state (BEGIN/COMMIT/ROLLBACK)
// works correctly over the wire: each connection has its own isolated tx slot.
type Server struct {
	db  *executor.DB
	mu  sync.Mutex
	ln  net.Listener
	wg  sync.WaitGroup

	shutdown chan struct{}
	once     sync.Once
}

// New creates a Server backed by db. Call ListenAndServe or Serve to start.
func New(db *executor.DB) *Server {
	return &Server{db: db, shutdown: make(chan struct{})}
}

// ListenAndServe binds to addr (e.g. ":5433") and starts serving connections.
// It blocks until Close is called.
func (s *Server) ListenAndServe(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %q: %w", addr, err)
	}
	return s.Serve(ln)
}

// Serve accepts connections from ln. Useful when the caller wants to choose
// the listener (e.g. ":0" for a free port in tests).
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-s.shutdown:
				return nil
			default:
				return fmt.Errorf("accept: %w", err)
			}
		}
		s.wg.Add(1)
		go func(c net.Conn) {
			defer s.wg.Done()
			s.handle(c)
		}(conn)
	}
}

// Addr returns the listener's network address (e.g. "127.0.0.1:5433").
// Returns "" before Serve is called.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Close shuts the server down gracefully: stops accepting new connections,
// closes the listener, and waits for all active connections to finish.
func (s *Server) Close() {
	s.once.Do(func() {
		close(s.shutdown)
		s.mu.Lock()
		if s.ln != nil {
			s.ln.Close()
		}
		s.mu.Unlock()
	})
	s.wg.Wait()
}

// handle serves one client connection for its full lifetime.
// The loop reads request frames, executes the SQL, and writes response frames.
// It exits when the client closes the connection (io.EOF) or on any error.
func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	for {
		var req Request
		if err := readFrame(conn, &req); err != nil {
			if err != io.EOF {
				_ = writeFrame(conn, Response{Error: err.Error()})
			}
			return
		}
		resp := s.execSQL(req.SQL)
		if err := writeFrame(conn, resp); err != nil {
			return
		}
	}
}

// execSQL runs sql against the database and converts the result to a Response.
func (s *Server) execSQL(sql string) Response {
	res, err := s.db.Exec(sql)
	if err != nil {
		return Response{Error: err.Error()}
	}
	resp := Response{
		Columns: res.Columns,
		Message: res.Message,
	}
	for _, row := range res.Rows {
		cells := make([]string, len(row))
		for i, v := range row {
			cells[i] = v.String()
		}
		resp.Rows = append(resp.Rows, cells)
	}
	return resp
}
