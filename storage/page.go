// Package storage defines the on-disk representation of a single page.
// A page is the fundamental unit of I/O in this engine — we never read or
// write less than one full page at a time, mirroring how real databases work.
package storage

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
)

// PageSize is 4096 bytes (4 KiB).
//
// Why 4 KiB?
//   - The Linux kernel's virtual memory subsystem works in 4 KiB pages.
//     Aligning our pages to that means one database read = one OS read = one
//     TLB/cache entry. No partial-page overhead, no read amplification.
//   - Most SSDs and HDDs have 4 KiB physical sectors. Smaller writes get
//     silently padded to 4 KiB anyway, so we might as well own the full unit.
//   - PostgreSQL, SQLite, and InnoDB all default to 4–8 KiB pages for exactly
//     these reasons. 4 KiB is the sweet spot: small enough to keep waste low
//     when pages are mostly empty, large enough to hold meaningful records.
const PageSize = 4096

// HeaderSize is the number of bytes reserved for metadata at the start of every page.
// All pages — regardless of type — share this fixed prefix so the pager can read
// any page header without knowing its type first.
const HeaderSize = 24

// DataSize is the usable payload area per page.
const DataSize = PageSize - HeaderSize // 4072 bytes

// MagicNumber is stored at bytes 0–3 of every page.
//
// Why a magic number?
//   If we open a file that isn't our database (or a truncated write corrupted
//   the start of a page), the magic number lets us fail loudly instead of
//   silently reading garbage. Pick something unlikely to appear by accident.
const MagicNumber = uint32(0xDB110011)

// PageType identifies the role of a page so the engine knows how to interpret
// the data area without additional out-of-band information.
type PageType uint8

const (
	PageTypeFree PageType = 0 // unallocated; its data area is undefined
	PageTypeMeta PageType = 1 // page 0 only: global metadata + free list
	PageTypeData PageType = 2 // user data page
)

// PageHeader is the fixed 24-byte prefix written at the start of every page.
//
// On-disk layout (all multi-byte fields are little-endian):
//
//	Bytes  0– 3  Magic           uint32   always 0xDB110011
//	Bytes  4– 7  PageID          uint32   this page's own ID
//	Byte   8     PageType        uint8    free / meta / data
//	Byte   9     Flags           uint8    reserved for future use
//	Bytes 10–11  FreeSpaceOffset uint16   offset into Data[] where free space starts
//	Bytes 12–13  NumCells        uint16   number of records appended so far
//	Bytes 14–15  (reserved)               zero-padded
//	Bytes 16–19  Checksum        uint32   CRC32 of the data area (bytes 24–4095)
//	Bytes 20–23  LSN             uint32   Log Sequence Number — unused until Phase 4 WAL
//
// Why little-endian?
//   x86 and ARM (the platforms this will run on) are natively little-endian.
//   Using the same byte order avoids per-field byte swaps on every read/write,
//   and matches what most production databases do (PostgreSQL, MySQL, SQLite).
type PageHeader struct {
	PageID          uint32
	PageType        PageType
	Flags           uint8
	FreeSpaceOffset uint16 // byte offset from the start of Data[] to the first free byte
	NumCells        uint16 // number of records written into Data
	Checksum        uint32 // CRC32 of Data[]; detected on read to catch silent corruption
	LSN             uint32 // reserved for Phase 4: WAL log sequence number
}

// Page is the in-memory representation of one 4 KiB page.
// Encoding it to [PageSize]byte and decoding back is always a lossless round-trip.
type Page struct {
	Header PageHeader
	Data   [DataSize]byte
}

// NewPage allocates an in-memory Page with the given ID and type.
// FreeSpaceOffset starts at 0, meaning the entire Data area is available.
func NewPage(id uint32, pt PageType) *Page {
	return &Page{
		Header: PageHeader{
			PageID:   id,
			PageType: pt,
		},
	}
}

// Write appends data to the page's Data area, advancing FreeSpaceOffset.
// Returns an error if the page doesn't have enough contiguous free space.
//
// This is a simplified sequential append — no slot directory, no fragmentation
// handling. Phase 2 (B-Tree) will introduce a proper slotted-page layout that
// can insert and delete records without leaving dead space.
func (p *Page) Write(data []byte) error {
	offset := int(p.Header.FreeSpaceOffset)
	if offset+len(data) > DataSize {
		return fmt.Errorf("not enough space on page %d: need %d bytes, only %d free",
			p.Header.PageID, len(data), DataSize-offset)
	}
	copy(p.Data[offset:], data)
	p.Header.FreeSpaceOffset += uint16(len(data))
	p.Header.NumCells++
	return nil
}

// FreeSpace returns the number of bytes available for new writes.
func (p *Page) FreeSpace() int {
	return DataSize - int(p.Header.FreeSpaceOffset)
}

// Encode serializes a Page into a fixed [PageSize]byte ready for disk.
//
// The checksum is computed over the data area (bytes 24–4095) only.
// Why exclude the header from the checksum?
//   The Checksum field lives in the header at bytes 16–19. If we checksummed
//   the whole page including the header, we'd have a circular dependency:
//   the checksum covers the checksum field. Checksumming only the data area
//   sidesteps this cleanly, and the header fields (PageID, PageType, etc.) are
//   implicitly protected because a corrupted header typically changes the data
//   layout offsets, which would then fail a higher-level consistency check.
func Encode(p *Page) ([PageSize]byte, error) {
	var raw [PageSize]byte

	binary.LittleEndian.PutUint32(raw[0:4], MagicNumber)
	binary.LittleEndian.PutUint32(raw[4:8], p.Header.PageID)
	raw[8] = byte(p.Header.PageType)
	raw[9] = p.Header.Flags
	binary.LittleEndian.PutUint16(raw[10:12], p.Header.FreeSpaceOffset)
	binary.LittleEndian.PutUint16(raw[12:14], p.Header.NumCells)
	// raw[14:16] stays zero (reserved)
	binary.LittleEndian.PutUint32(raw[20:24], p.Header.LSN)

	copy(raw[HeaderSize:], p.Data[:])

	checksum := crc32.ChecksumIEEE(raw[HeaderSize:])
	binary.LittleEndian.PutUint32(raw[16:20], checksum)

	return raw, nil
}

// Decode deserializes a raw [PageSize]byte (read from disk) back into a Page.
// Returns an error if the magic number is wrong or the CRC32 checksum fails.
func Decode(raw [PageSize]byte) (*Page, error) {
	magic := binary.LittleEndian.Uint32(raw[0:4])
	if magic != MagicNumber {
		return nil, fmt.Errorf(
			"bad magic number: got 0x%08X, want 0x%08X — not a db-engine file, or page is corrupt",
			magic, MagicNumber)
	}

	storedChecksum := binary.LittleEndian.Uint32(raw[16:20])
	actualChecksum := crc32.ChecksumIEEE(raw[HeaderSize:])
	if storedChecksum != actualChecksum {
		return nil, fmt.Errorf(
			"checksum mismatch on page %d: stored=0x%08X computed=0x%08X — data is corrupt",
			binary.LittleEndian.Uint32(raw[4:8]), storedChecksum, actualChecksum)
	}

	p := &Page{}
	p.Header.PageID = binary.LittleEndian.Uint32(raw[4:8])
	p.Header.PageType = PageType(raw[8])
	p.Header.Flags = raw[9]
	p.Header.FreeSpaceOffset = binary.LittleEndian.Uint16(raw[10:12])
	p.Header.NumCells = binary.LittleEndian.Uint16(raw[12:14])
	p.Header.Checksum = storedChecksum
	p.Header.LSN = binary.LittleEndian.Uint32(raw[20:24])
	copy(p.Data[:], raw[HeaderSize:])

	return p, nil
}
