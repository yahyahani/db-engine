// Command dashboard starts an HTTP server that serves a web UI for the
// db-engine database. It exposes three JSON API endpoints:
//
//	GET  /api/tables          list all tables with schema and statistics
//	POST /api/query           execute a SQL statement, return rows or message
//	GET  /api/pool            buffer pool hit/miss counters
//
// The static dashboard (cmd/dashboard/static/index.html) is embedded into the
// binary at build time so the server has no external file dependency.
//
// Usage:
//
//	go run ./cmd/dashboard -dir mydb -port 8080
package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/yahya/db-engine/catalog"
	"github.com/yahya/db-engine/executor"
)

//go:embed static
var staticFiles embed.FS

// ---- request / response types -----------------------------------------------

type colInfo struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type idxInfo struct {
	Name   string `json:"name"`
	Column string `json:"column"`
}

type colStatInfo struct {
	Name      string `json:"name"`
	NDistinct uint64 `json:"nDistinct"`
	Min       uint64 `json:"min"`
	Max       uint64 `json:"max"`
}

type tableStatsInfo struct {
	RowCount uint64        `json:"rowCount"`
	Columns  []colStatInfo `json:"columns"`
}

type tableInfo struct {
	Name    string          `json:"name"`
	Columns []colInfo       `json:"columns"`
	Indexes []idxInfo       `json:"indexes"`
	Stats   *tableStatsInfo `json:"stats,omitempty"`
}

type queryReq struct {
	SQL string `json:"sql"`
}

type queryResp struct {
	Columns  []string        `json:"columns,omitempty"`
	Rows     [][]interface{} `json:"rows,omitempty"`
	Message  string          `json:"message,omitempty"`
	Duration string          `json:"duration"`
	RowCount int             `json:"rowCount"`
	Error    string          `json:"error,omitempty"`
}

// multiResult is the response for /api/query.
// Results has one entry per SQL statement (split on ';').
type multiResult struct {
	Results       []queryResp `json:"results"`
	TotalDuration string      `json:"totalDuration"`
}

type poolResp struct {
	Hits     uint64  `json:"hits"`
	Misses   uint64  `json:"misses"`
	HitRatio float64 `json:"hitRatio"`
}

// ---- server -----------------------------------------------------------------

type server struct {
	db *executor.DB
	mu sync.Mutex // serialises all DB calls; the DB is single-threaded
}

func main() {
	dbDir := flag.String("dir", "mydb", "database directory to open (created if missing)")
	port := flag.Int("port", 8080, "HTTP port")
	flag.Parse()

	db, err := executor.Open(*dbDir)
	if err != nil {
		log.Fatalf("open database %q: %v", *dbDir, err)
	}
	defer db.Close()

	srv := &server{db: db}

	static, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatalf("embed: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(static)))
	mux.HandleFunc("/api/tables", srv.handleTables)
	mux.HandleFunc("/api/query", srv.handleQuery)
	mux.HandleFunc("/api/pool", srv.handlePool)

	addr := fmt.Sprintf(":%d", *port)
	fmt.Printf("db-engine dashboard  →  http://localhost%s\n", addr)
	fmt.Printf("database directory   →  %s\n\n", *dbDir)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// ---- handlers ---------------------------------------------------------------

func (s *server) handleTables(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	tables := s.db.Tables()
	infos := make([]tableInfo, 0, len(tables))

	for _, t := range tables {
		info := tableInfo{
			Name:    t.Name,
			Columns: make([]colInfo, 0, len(t.Columns)),
			Indexes: make([]idxInfo, 0, len(t.Indexes)),
		}
		for _, c := range t.Columns {
			info.Columns = append(info.Columns, colInfo{Name: c.Name, Type: c.Type.String()})
		}
		for _, idx := range t.Indexes {
			info.Indexes = append(info.Indexes, idxInfo{Name: idx.Name, Column: idx.Column})
		}
		if ts, ok := s.db.TableStats(t.Name); ok {
			si := &tableStatsInfo{RowCount: ts.RowCount}
			for _, cs := range ts.Columns {
				si.Columns = append(si.Columns, colStatInfo{
					Name: cs.Name, NDistinct: cs.NDistinct,
					Min: cs.Min, Max: cs.Max,
				})
			}
			info.Stats = si
		}
		infos = append(infos, info)
	}
	s.mu.Unlock()

	jsonOK(w, infos)
}

func (s *server) handleQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req queryReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SQL == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	stmts := splitStatements(req.SQL)
	if len(stmts) == 0 {
		http.Error(w, "empty query", http.StatusBadRequest)
		return
	}

	totalStart := time.Now()
	results := make([]queryResp, 0, len(stmts))

	for _, stmt := range stmts {
		start := time.Now()
		s.mu.Lock()
		res, execErr := s.db.Exec(stmt)
		s.mu.Unlock()
		results = append(results, buildQueryResp(res, time.Since(start), execErr))
		if execErr != nil {
			break // stop on first error; partial results still returned
		}
	}

	jsonOK(w, multiResult{
		Results:       results,
		TotalDuration: fmtDur(time.Since(totalStart)),
	})
}

func (s *server) handlePool(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	ps := s.db.PoolStats()
	s.mu.Unlock()

	var ratio float64
	if total := ps.Hits + ps.Misses; total > 0 {
		ratio = float64(ps.Hits) / float64(total)
	}
	jsonOK(w, poolResp{Hits: ps.Hits, Misses: ps.Misses, HitRatio: ratio})
}

// ---- helpers ----------------------------------------------------------------

func jsonOK(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func fmtDur(d time.Duration) string {
	if d < time.Millisecond {
		return fmt.Sprintf("%dµs", d.Microseconds())
	}
	return fmt.Sprintf("%.1fms", float64(d.Microseconds())/1000.0)
}

// splitStatements splits SQL source on ';' while respecting single-quoted
// string literals (including escaped '' inside strings).
// Empty/whitespace-only fragments are dropped.
func splitStatements(sql string) []string {
	var stmts []string
	var buf strings.Builder
	inStr := false

	for i := 0; i < len(sql); i++ {
		ch := sql[i]
		switch {
		case ch == '\'' && !inStr:
			inStr = true
			buf.WriteByte(ch)
		case ch == '\'' && inStr:
			if i+1 < len(sql) && sql[i+1] == '\'' { // escaped ''
				buf.WriteByte(ch)
				buf.WriteByte(ch)
				i++
			} else {
				inStr = false
				buf.WriteByte(ch)
			}
		case ch == ';' && !inStr:
			if s := strings.TrimSpace(buf.String()); s != "" {
				stmts = append(stmts, s)
			}
			buf.Reset()
		default:
			buf.WriteByte(ch)
		}
	}
	if s := strings.TrimSpace(buf.String()); s != "" {
		stmts = append(stmts, s)
	}
	return stmts
}

// buildQueryResp converts an executor result + timing into a queryResp.
func buildQueryResp(res *executor.Result, dur time.Duration, execErr error) queryResp {
	resp := queryResp{Duration: fmtDur(dur)}
	if execErr != nil {
		resp.Error = execErr.Error()
		return resp
	}
	resp.Message = res.Message
	if len(res.Columns) > 0 {
		resp.Columns = res.Columns
		resp.Rows = make([][]interface{}, len(res.Rows))
		for i, row := range res.Rows {
			cells := make([]interface{}, len(row))
			for j, v := range row {
				if v.Type == catalog.TypeInt {
					cells[j] = v.IntVal
				} else {
					cells[j] = v.TextVal
				}
			}
			resp.Rows[i] = cells
		}
		resp.RowCount = len(res.Rows)
	}
	return resp
}
