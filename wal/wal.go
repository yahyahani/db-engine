// Package wal implements a Write-Ahead Log for crash recovery.
//
// Design overview
//
// The WAL is an append-only file of fixed-size records. Every page that a
// committed transaction writes is captured here before the page reaches the
// data file. On crash recovery the log is replayed: all writes belonging to
// a committed transaction are re-applied to the data files.
//
// Record layout (4149 bytes per record):
//
//	[0..7]       LSN        uint64   — log sequence number, monotonically increasing
//	[8..11]      XID        uint32   — transaction ID
//	[12]         Type       uint8    — RecordBegin / RecordWrite / RecordCommit / RecordRollback
//	[13..44]     TableName  [32]byte — basename of the table .db file (zero-padded)
//	[45..48]     PageID     uint32   — page number within the table file
//	[49..4144]   PageData   [4096]byte — full after-image of the page
//	[4145..4148] CRC32      uint32   — checksum over bytes 0..4144
//
// For non-Write records (Begin / Commit / Rollback) the TableName, PageID, and
// PageData fields are all zeroed; only LSN, XID, Type, and CRC32 are meaningful.
//
// Recovery protocol (REDO-only)
//
//  1. Scan every record in the WAL.
//  2. Identify all committed XIDs (those that have a RecordCommit record).
//  3. For each RecordWrite whose XID is committed, replay the page image into
//     the corresponding table file — the highest-LSN write wins per page.
//
// Why REDO-only (no UNDO)?
//   TxPager implements the "no-steal" policy: uncommitted pages never reach the
//   data files. There is therefore nothing to undo. Any page that is on disk was
//   written as part of a previous Flush() call, which only executes after the
//   WAL commit record is synced. Recovery just ensures those writes survive a
//   crash between the WAL sync and the data-file flush.
package wal

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/yahya/db-engine/pager"
	"github.com/yahya/db-engine/storage"
)

const (
	tableNameSize = 32
	// RecordSize is the on-disk size of every WAL record (fixed for O(1) seek).
	// Using a fixed size avoids a length-scanning pass during recovery and lets
	// us jump directly to record N via offset = N × RecordSize.
	RecordSize = 8 + 4 + 1 + tableNameSize + 4 + storage.PageSize + 4 // = 4149
)

// RecordType classifies the role of a WAL record in a transaction's lifecycle.
type RecordType uint8

const (
	RecordBegin    RecordType = 1
	RecordWrite    RecordType = 2
	RecordCommit   RecordType = 3
	RecordRollback RecordType = 4
)

// Record is the in-memory representation of one WAL entry.
type Record struct {
	LSN       uint64
	XID       uint32
	Type      RecordType
	TableName string                 // basename of .db file; empty for non-Write records
	PageID    uint32                 // set for RecordWrite
	PageData  [storage.PageSize]byte // set for RecordWrite
}

// WAL is an append-only write-ahead log.
// All exported methods are safe for concurrent use.
type WAL struct {
	mu      sync.Mutex
	f       *os.File
	records uint64 // total valid records in the file
	nextLSN uint64 // next LSN to assign (1-based)
	nextXID uint32 // next XID to assign (1-based)
}

// Open opens or creates the WAL at path.
// Existing records are scanned to restore the LSN and XID counters.
// A record with a bad CRC is treated as end-of-log (partial write from a crash).
func Open(path string) (*WAL, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, fmt.Errorf("open WAL %q: %w", path, err)
	}
	w := &WAL{f: f, nextLSN: 1, nextXID: 1}
	if err := w.scanInit(); err != nil {
		f.Close()
		return nil, fmt.Errorf("scan WAL %q: %w", path, err)
	}
	return w, nil
}

// Close syncs pending OS buffers and closes the WAL file.
func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.f.Sync(); err != nil {
		return fmt.Errorf("WAL sync on close: %w", err)
	}
	return w.f.Close()
}

// AllocXID reserves a new transaction ID and appends a Begin record.
// The Begin record is the anchor that recovery uses to identify XIDs in progress.
func (w *WAL) AllocXID() (uint32, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	xid := w.nextXID
	w.nextXID++
	return xid, w.appendLocked(Record{LSN: w.allocLSN(), XID: xid, Type: RecordBegin})
}

// AppendWrite logs an after-image of pageID in tableName for transaction xid.
// Must be called BEFORE the page is written to the data file.
func (w *WAL) AppendWrite(xid uint32, tableName string, pageID uint32, data [storage.PageSize]byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.appendLocked(Record{
		LSN:       w.allocLSN(),
		XID:       xid,
		Type:      RecordWrite,
		TableName: tableName,
		PageID:    pageID,
		PageData:  data,
	})
}

// AppendCommit marks xid as durably committed in the log.
// After this record is synced, the transaction is recoverable across crashes.
func (w *WAL) AppendCommit(xid uint32) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.appendLocked(Record{LSN: w.allocLSN(), XID: xid, Type: RecordCommit})
}

// AppendRollback marks xid as rolled back in the log.
func (w *WAL) AppendRollback(xid uint32) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.appendLocked(Record{LSN: w.allocLSN(), XID: xid, Type: RecordRollback})
}

// Sync forces all WAL records to durable storage (fdatasync).
// The executor calls this after writing all page records but before flushing
// pages to the data files, satisfying the write-ahead invariant.
func (w *WAL) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f.Sync()
}

// RecordCount returns the number of valid records in the log.
func (w *WAL) RecordCount() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.records
}

// CommittedXIDs returns the set of all XIDs that have a RecordCommit entry.
// Called by the executor after Open+Recover to restore MVCC visibility state:
// rows inserted by these XIDs must be visible to new snapshots.
func (w *WAL) CommittedXIDs() ([]uint32, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	records, err := w.readAllLocked()
	if err != nil {
		return nil, err
	}
	seen := make(map[uint32]struct{})
	for _, r := range records {
		if r.Type == RecordCommit {
			seen[r.XID] = struct{}{}
		}
	}
	out := make([]uint32, 0, len(seen))
	for xid := range seen {
		out = append(out, xid)
	}
	return out, nil
}

// Recover replays all committed writes from the WAL into the data files under dir.
//
// The algorithm is:
//  1. Read all records.
//  2. Find committed XIDs (RecordCommit present).
//  3. For each committed RecordWrite, apply the highest-LSN after-image per page.
//
// Pages that belong to uncommitted transactions are never touched because
// TxPager (no-steal) guarantees they were never written to the data files.
func (w *WAL) Recover(dir string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	records, err := w.readAllLocked()
	if err != nil {
		return fmt.Errorf("WAL recover: read records: %w", err)
	}
	if len(records) == 0 {
		return nil
	}

	// Phase 1: identify committed XIDs.
	committed := make(map[uint32]bool, len(records)/3)
	for _, r := range records {
		if r.Type == RecordCommit {
			committed[r.XID] = true
		}
	}

	// Phase 2: for each (table, page) keep only the highest-LSN write.
	type key struct{ table string; pageID uint32 }
	type entry struct {
		data [storage.PageSize]byte
		lsn  uint64
	}
	latest := make(map[key]entry)
	for _, r := range records {
		if r.Type != RecordWrite || !committed[r.XID] {
			continue
		}
		k := key{r.TableName, r.PageID}
		if r.LSN > latest[k].lsn {
			latest[k] = entry{r.PageData, r.LSN}
		}
	}
	if len(latest) == 0 {
		return nil
	}

	// Phase 3: group writes by table file and apply them.
	byTable := make(map[string]map[uint32][storage.PageSize]byte)
	for k, e := range latest {
		if byTable[k.table] == nil {
			byTable[k.table] = make(map[uint32][storage.PageSize]byte)
		}
		byTable[k.table][k.pageID] = e.data
	}

	for tableName, pages := range byTable {
		path := filepath.Join(dir, tableName)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			// Table file was removed externally or never created — skip.
			continue
		}
		pg, err := pager.Open(path)
		if err != nil {
			return fmt.Errorf("WAL recover: open %q: %w", tableName, err)
		}
		for pageID, raw := range pages {
			page, err := storage.Decode(raw)
			if err != nil {
				_ = pg.Close()
				return fmt.Errorf("WAL recover: decode page %d in %q: %w", pageID, tableName, err)
			}
			if err := pg.WritePage(page); err != nil {
				_ = pg.Close()
				return fmt.Errorf("WAL recover: write page %d in %q: %w", pageID, tableName, err)
			}
		}
		if err := pg.Close(); err != nil {
			return fmt.Errorf("WAL recover: close %q: %w", tableName, err)
		}
	}
	return nil
}

// --- private helpers ---

func (w *WAL) allocLSN() uint64 {
	lsn := w.nextLSN
	w.nextLSN++
	return lsn
}

// scanInit reads all valid records on startup to restore nextLSN and nextXID.
// Stops at the first record with a bad CRC (partial write from a crash).
func (w *WAL) scanInit() error {
	info, err := w.f.Stat()
	if err != nil {
		return err
	}
	total := uint64(info.Size()) / uint64(RecordSize)
	w.records = 0
	for i := uint64(0); i < total; i++ {
		r, err := w.readRecordAt(i)
		if err != nil {
			// Corrupt or partial tail record — truncate the logical log here.
			break
		}
		w.records++
		if r.LSN >= w.nextLSN {
			w.nextLSN = r.LSN + 1
		}
		if r.XID >= w.nextXID {
			w.nextXID = r.XID + 1
		}
	}
	return nil
}

func (w *WAL) readAllLocked() ([]Record, error) {
	out := make([]Record, 0, w.records)
	for i := uint64(0); i < w.records; i++ {
		r, err := w.readRecordAt(i)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func (w *WAL) readRecordAt(index uint64) (Record, error) {
	var buf [RecordSize]byte
	offset := int64(index) * RecordSize
	if _, err := w.f.ReadAt(buf[:], offset); err != nil {
		return Record{}, fmt.Errorf("read WAL record %d: %w", index, err)
	}
	return decodeRecord(buf)
}

func (w *WAL) appendLocked(r Record) error {
	buf := encodeRecord(r)
	offset := int64(w.records) * RecordSize
	if _, err := w.f.WriteAt(buf[:], offset); err != nil {
		return fmt.Errorf("write WAL record: %w", err)
	}
	w.records++
	return nil
}

// encodeRecord serialises r into a [RecordSize]byte buffer.
//
// Layout offsets:
//
//	0-7:       LSN
//	8-11:      XID
//	12:        Type
//	13-44:     TableName (32 bytes, zero-padded)
//	45-48:     PageID
//	49-4144:   PageData (4096 bytes)
//	4145-4148: CRC32 over bytes 0..4144
func encodeRecord(r Record) [RecordSize]byte {
	var buf [RecordSize]byte
	binary.LittleEndian.PutUint64(buf[0:8], r.LSN)
	binary.LittleEndian.PutUint32(buf[8:12], r.XID)
	buf[12] = byte(r.Type)
	name := []byte(r.TableName)
	if len(name) > tableNameSize {
		name = name[:tableNameSize]
	}
	copy(buf[13:13+tableNameSize], name)
	binary.LittleEndian.PutUint32(buf[45:49], r.PageID)
	copy(buf[49:49+storage.PageSize], r.PageData[:])
	crc := crc32.ChecksumIEEE(buf[:RecordSize-4])
	binary.LittleEndian.PutUint32(buf[RecordSize-4:], crc)
	return buf
}

func decodeRecord(buf [RecordSize]byte) (Record, error) {
	stored := binary.LittleEndian.Uint32(buf[RecordSize-4:])
	got := crc32.ChecksumIEEE(buf[:RecordSize-4])
	if stored != got {
		return Record{}, fmt.Errorf("WAL record CRC mismatch (stored 0x%08X, computed 0x%08X)", stored, got)
	}
	r := Record{}
	r.LSN = binary.LittleEndian.Uint64(buf[0:8])
	r.XID = binary.LittleEndian.Uint32(buf[8:12])
	r.Type = RecordType(buf[12])
	r.TableName = strings.TrimRight(string(buf[13:13+tableNameSize]), "\x00")
	r.PageID = binary.LittleEndian.Uint32(buf[45:49])
	copy(r.PageData[:], buf[49:49+storage.PageSize])
	return r, nil
}
