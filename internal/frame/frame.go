// Package frame implements Verity's on-disk record framing: length | crc32 |
// payload, little-endian throughout. The framing is implemented once here so
// the simulator's disk model (package sim) and the real WAL (package store)
// agree byte-for-byte on what a torn write leaves behind and on the exact
// rule for recognising one, rather than each guessing independently. See
// docs/SPEC.md, section 5.3.
package frame

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"

	"verity/node"
)

// HeaderSize is the number of bytes preceding a frame's payload: a uint32
// length followed by a uint32 crc32, both little-endian.
const HeaderSize = 8

// MaxPayload bounds a frame's declared payload length. A corrupt header can
// claim any length a uint32 can hold; without this bound, decoding it would
// mean allocating on the strength of bytes nothing has verified yet.
const MaxPayload = 64 << 20

// recordHeaderSize is the number of payload bytes preceding a record's Data:
// one byte of RecordKind followed by an eight-byte little-endian Index.
const recordHeaderSize = 1 + 8

// Errors returned by Next, DecodeRecord, and DecodeAll. Each can be tested
// with errors.Is. DecodeAll wraps them again to name the byte offset at
// which decoding stopped, which is what turns a torn trailing write into a
// recoverable prefix instead of an all-or-nothing failure.
var (
	// ErrShortFrame means fewer bytes remain than the header, or than the
	// header's declared length, requires.
	ErrShortFrame = errors.New("frame: short frame")
	// ErrBadCRC means the payload's crc32 does not match the header.
	ErrBadCRC = errors.New("frame: bad crc")
	// ErrOversize means the header's declared length exceeds MaxPayload.
	ErrOversize = errors.New("frame: oversize length")
	// ErrShortRecord means a frame's payload is too short to hold the
	// kind and index that precede a record's Data.
	ErrShortRecord = errors.New("frame: short record")
	// ErrBadKind means a record's kind byte is not one of the three
	// node.RecordKind values.
	ErrBadKind = errors.New("frame: bad kind")
)

// Append appends payload to dst as one frame and returns the extended slice.
func Append(dst, payload []byte) []byte {
	var hdr [HeaderSize]byte
	binary.LittleEndian.PutUint32(hdr[0:4], uint32(len(payload)))
	binary.LittleEndian.PutUint32(hdr[4:8], crc32.ChecksumIEEE(payload))
	dst = append(dst, hdr[:]...)
	dst = append(dst, payload...)
	return dst
}

// AppendRecord encodes rec as kind | index | data and appends it as one
// frame.
func AppendRecord(dst []byte, rec node.Record) []byte {
	payload := make([]byte, recordHeaderSize+len(rec.Data))
	payload[0] = byte(rec.Kind)
	binary.LittleEndian.PutUint64(payload[1:recordHeaderSize], rec.Index)
	copy(payload[recordHeaderSize:], rec.Data)
	return Append(dst, payload)
}

// Next decodes the frame at the start of b. n is the total bytes consumed
// (HeaderSize plus the declared length). The returned payload aliases b;
// callers that keep it past the next mutation of b must copy it. On error n
// is 0 and payload is nil.
func Next(b []byte) (payload []byte, n int, err error) {
	if len(b) < HeaderSize {
		return nil, 0, fmt.Errorf("frame: %w: need %d header bytes, have %d", ErrShortFrame, HeaderSize, len(b))
	}
	length := binary.LittleEndian.Uint32(b[0:4])
	crc := binary.LittleEndian.Uint32(b[4:8])
	if length > MaxPayload {
		return nil, 0, fmt.Errorf("frame: %w: declared length %d exceeds %d", ErrOversize, length, MaxPayload)
	}
	need := HeaderSize + int(length)
	if len(b) < need {
		return nil, 0, fmt.Errorf("frame: %w: need %d payload bytes, have %d", ErrShortFrame, length, len(b)-HeaderSize)
	}
	payload = b[HeaderSize:need]
	if got := crc32.ChecksumIEEE(payload); got != crc {
		return nil, 0, fmt.Errorf("frame: %w: want %08x, got %08x", ErrBadCRC, crc, got)
	}
	return payload, need, nil
}

// DecodeRecord decodes one record payload, as returned by Next. The
// returned Record's Data aliases payload.
func DecodeRecord(payload []byte) (node.Record, error) {
	if len(payload) < recordHeaderSize {
		return node.Record{}, fmt.Errorf("frame: %w: need %d bytes, have %d", ErrShortRecord, recordHeaderSize, len(payload))
	}
	kind := node.RecordKind(payload[0])
	if kind < node.RecordHardState || kind > node.RecordSnapshot {
		return node.Record{}, fmt.Errorf("frame: %w: %d", ErrBadKind, kind)
	}
	index := binary.LittleEndian.Uint64(payload[1:recordHeaderSize])
	return node.Record{Kind: kind, Index: index, Data: payload[recordHeaderSize:]}, nil
}

// DecodeAll reads records from b until the first frame that does not decode
// cleanly, which is how a torn trailing write is survived rather than
// mistaken for data. It returns the records recovered, the byte offset at
// which decoding stopped, and the error that stopped it. A clean full
// consumption of b returns (recs, len(b), nil).
//
// Each returned Record's Data is a copy; none alias b.
func DecodeAll(b []byte) (recs []node.Record, off int, err error) {
	for off < len(b) {
		start := off
		payload, n, ferr := Next(b[off:])
		if ferr != nil {
			return recs, start, fmt.Errorf("frame: DecodeAll: stopped at offset %d: %w", start, ferr)
		}
		rec, derr := DecodeRecord(payload)
		if derr != nil {
			return recs, start, fmt.Errorf("frame: DecodeAll: stopped at offset %d: %w", start, derr)
		}
		rec.Data = append([]byte(nil), rec.Data...)
		recs = append(recs, rec)
		off += n
	}
	return recs, off, nil
}
