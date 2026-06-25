package storage

import (
	"bytes"
	"testing"
)

// TestNewPage verifies the zero-state of a freshly created in-memory page.
func TestNewPage(t *testing.T) {
	p := NewPage(42, PageTypeData)

	if p.Header.PageID != 42 {
		t.Errorf("PageID: got %d, want 42", p.Header.PageID)
	}
	if p.Header.PageType != PageTypeData {
		t.Errorf("PageType: got %d, want PageTypeData", p.Header.PageType)
	}
	if p.Header.FreeSpaceOffset != 0 {
		t.Errorf("FreeSpaceOffset: got %d, want 0 for a new page", p.Header.FreeSpaceOffset)
	}
	if p.FreeSpace() != DataSize {
		t.Errorf("FreeSpace: got %d, want DataSize (%d)", p.FreeSpace(), DataSize)
	}
}

// TestPageWrite verifies that Write advances FreeSpaceOffset and copies bytes correctly.
func TestPageWrite(t *testing.T) {
	p := NewPage(1, PageTypeData)
	payload := []byte("hello, storage engine!")

	if err := p.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if int(p.Header.FreeSpaceOffset) != len(payload) {
		t.Errorf("FreeSpaceOffset: got %d, want %d", p.Header.FreeSpaceOffset, len(payload))
	}
	if p.FreeSpace() != DataSize-len(payload) {
		t.Errorf("FreeSpace after write: got %d, want %d", p.FreeSpace(), DataSize-len(payload))
	}
	if !bytes.Equal(p.Data[:len(payload)], payload) {
		t.Errorf("Data mismatch: got %q, want %q", p.Data[:len(payload)], payload)
	}
	if p.Header.NumCells != 1 {
		t.Errorf("NumCells: got %d, want 1", p.Header.NumCells)
	}
}

// TestPageWriteMultiple verifies sequential writes concatenate correctly.
func TestPageWriteMultiple(t *testing.T) {
	p := NewPage(1, PageTypeData)
	_ = p.Write([]byte("foo"))
	_ = p.Write([]byte("bar"))

	if p.Header.NumCells != 2 {
		t.Errorf("NumCells: got %d, want 2", p.Header.NumCells)
	}
	if !bytes.Equal(p.Data[:6], []byte("foobar")) {
		t.Errorf("expected 'foobar' in Data, got %q", p.Data[:6])
	}
}

// TestPageWriteOverflow verifies that writing past DataSize is rejected.
func TestPageWriteOverflow(t *testing.T) {
	p := NewPage(1, PageTypeData)
	tooBig := make([]byte, DataSize+1)
	if err := p.Write(tooBig); err == nil {
		t.Error("expected error when writing more than DataSize bytes, got nil")
	}
}

// TestEncodeDecodeRoundtrip is the core correctness test:
// every field written must survive a full encode → decode cycle.
func TestEncodeDecodeRoundtrip(t *testing.T) {
	original := NewPage(99, PageTypeData)
	_ = original.Write([]byte("roundtrip test — does binary survive disk?"))

	raw, err := Encode(original)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	decoded, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Header.PageID != original.Header.PageID {
		t.Errorf("PageID: got %d, want %d", decoded.Header.PageID, original.Header.PageID)
	}
	if decoded.Header.PageType != original.Header.PageType {
		t.Errorf("PageType: got %d, want %d", decoded.Header.PageType, original.Header.PageType)
	}
	if decoded.Header.FreeSpaceOffset != original.Header.FreeSpaceOffset {
		t.Errorf("FreeSpaceOffset: got %d, want %d", decoded.Header.FreeSpaceOffset, original.Header.FreeSpaceOffset)
	}
	if decoded.Header.NumCells != original.Header.NumCells {
		t.Errorf("NumCells: got %d, want %d", decoded.Header.NumCells, original.Header.NumCells)
	}
	if decoded.Data != original.Data {
		t.Error("Data area mismatch after encode/decode")
	}
}

// TestEncodedSizeIsExactlyPageSize ensures the on-disk footprint is always exactly 4096 bytes.
// If this ever fails, the file layout is broken and existing database files become unreadable.
func TestEncodedSizeIsExactlyPageSize(t *testing.T) {
	p := NewPage(0, PageTypeData)
	raw, _ := Encode(p)
	if len(raw) != PageSize {
		t.Errorf("encoded size %d != PageSize %d", len(raw), PageSize)
	}
}

// TestDecodeRejectsBadMagic ensures we fail loudly when opening a non-db-engine file.
func TestDecodeRejectsBadMagic(t *testing.T) {
	var raw [PageSize]byte
	raw[0], raw[1], raw[2], raw[3] = 0xDE, 0xAD, 0xBE, 0xEF // wrong magic

	if _, err := Decode(raw); err == nil {
		t.Error("expected error for wrong magic number, got nil")
	}
}

// TestDecodeDetectsCorruption verifies that a single flipped bit in the data area
// is caught by the CRC32 checksum. Silent data corruption is a real failure mode
// on spinning disks and some SSDs (the so-called "silent corruption" or "bit rot").
func TestDecodeDetectsCorruption(t *testing.T) {
	p := NewPage(5, PageTypeData)
	_ = p.Write([]byte("critical data — must survive bit rot"))

	raw, _ := Encode(p)

	// Flip a bit in the middle of the data area.
	raw[HeaderSize+20] ^= 0xFF

	if _, err := Decode(raw); err == nil {
		t.Error("expected checksum error for corrupted page, got nil")
	}
}

// TestChecksumIsStoredInHeader verifies the checksum field is non-zero for a non-empty page.
func TestChecksumIsStoredInHeader(t *testing.T) {
	p := NewPage(1, PageTypeData)
	_ = p.Write([]byte("data"))

	raw, _ := Encode(p)
	decoded, _ := Decode(raw)

	if decoded.Header.Checksum == 0 {
		t.Error("expected non-zero checksum for a page with data")
	}
}
