package frame_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"verity/internal/frame"
	"verity/node"
)

// buildRecords returns n deterministic records covering all three
// node.RecordKind values, empty/1-byte/100KiB Data, and both index 0 and
// math.MaxUint64 (written as a literal since node packages, and this test's
// subject, must not import math).
func buildRecords(n int) []node.Record {
	kinds := []node.RecordKind{node.RecordHardState, node.RecordEntry, node.RecordSnapshot}
	const maxUint64 = 18446744073709551615

	recs := make([]node.Record, n)
	for i := 0; i < n; i++ {
		var data []byte
		switch i % 4 {
		case 0:
			data = nil
		case 1:
			data = []byte{byte(i)}
		case 2:
			data = make([]byte, 100*1024)
			for j := range data {
				data[j] = byte((i + j) % 256)
			}
		default:
			data = []byte(fmt.Sprintf("payload-%d", i))
		}

		var index uint64
		switch i % 3 {
		case 0:
			index = 0
		case 1:
			index = maxUint64
		default:
			index = uint64(i) * 12345
		}

		recs[i] = node.Record{Kind: kinds[i%len(kinds)], Index: index, Data: data}
	}
	return recs
}

func TestRoundTrip(t *testing.T) {
	recs := buildRecords(100)

	var buf []byte
	for _, r := range recs {
		buf = frame.AppendRecord(buf, r)
	}

	got, off, err := frame.DecodeAll(buf)
	if err != nil {
		t.Fatalf("DecodeAll: %v", err)
	}
	if off != len(buf) {
		t.Fatalf("off = %d, want %d", off, len(buf))
	}
	if len(got) != len(recs) {
		t.Fatalf("got %d records, want %d", len(got), len(recs))
	}
	for i := range recs {
		if !reflect.DeepEqual(got[i], recs[i]) {
			t.Fatalf("record %d: got %+v, want %+v", i, got[i], recs[i])
		}
	}
}

func TestDecodeAllEmpty(t *testing.T) {
	recs, off, err := frame.DecodeAll(nil)
	if recs != nil {
		t.Fatalf("recs = %#v, want nil", recs)
	}
	if off != 0 {
		t.Fatalf("off = %d, want 0", off)
	}
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
}

// TestTruncationSweep builds a 3-record buffer and truncates it at every
// length from 1 to len(buf)-1. DecodeAll must never panic, must return
// exactly the records that were fully present, and off must land exactly at
// the start of the first incomplete frame.
func TestTruncationSweep(t *testing.T) {
	recs := buildRecords(3)

	var buf []byte
	starts := make([]int, len(recs))
	for i, r := range recs {
		starts[i] = len(buf)
		buf = frame.AppendRecord(buf, r)
	}
	full := len(buf)

	ends := make([]int, len(recs))
	for i := range recs {
		if i+1 < len(recs) {
			ends[i] = starts[i+1]
		} else {
			ends[i] = full
		}
	}

	for truncLen := 1; truncLen < full; truncLen++ {
		trunc := buf[:truncLen]

		wantN := 0
		for wantN < len(recs) && truncLen >= ends[wantN] {
			wantN++
		}
		wantOff := starts[wantN] // wantN < len(recs) always, since truncLen < full

		got, off, err := frame.DecodeAll(trunc)

		// If truncLen lands exactly on a frame boundary, trunc is a clean,
		// complete prefix of frames rather than a torn one: no error.
		// Otherwise there are leftover bytes belonging to an incomplete
		// frame, which must be rejected.
		if truncLen == wantOff {
			if err != nil {
				t.Fatalf("truncLen=%d: want nil error (clean boundary), got %v", truncLen, err)
			}
		} else {
			if err == nil {
				t.Fatalf("truncLen=%d: want error, got nil (off=%d, %d records)", truncLen, off, len(got))
			}
			if !errors.Is(err, frame.ErrShortFrame) {
				t.Fatalf("truncLen=%d: err = %v, want ErrShortFrame", truncLen, err)
			}
		}
		if off != wantOff {
			t.Fatalf("truncLen=%d: off = %d, want %d", truncLen, off, wantOff)
		}
		if len(got) != wantN {
			t.Fatalf("truncLen=%d: got %d records, want %d", truncLen, len(got), wantN)
		}
		for i := 0; i < wantN; i++ {
			if !reflect.DeepEqual(got[i], recs[i]) {
				t.Fatalf("truncLen=%d: record %d = %+v, want %+v", truncLen, i, got[i], recs[i])
			}
		}
	}
}

// smallRecords returns n small records, varied in kind, index, and Data
// length, but without buildRecords' 100KiB entry — flipping every bit of
// every byte is O(bytes^2), so this keeps the sweep buffer small.
func smallRecords(n int) []node.Record {
	kinds := []node.RecordKind{node.RecordHardState, node.RecordEntry, node.RecordSnapshot}
	recs := make([]node.Record, n)
	for i := 0; i < n; i++ {
		var data []byte
		switch i % 3 {
		case 0:
			data = nil
		case 1:
			data = []byte{byte(i)}
		default:
			data = []byte(fmt.Sprintf("payload-%d", i))
		}
		recs[i] = node.Record{Kind: kinds[i%len(kinds)], Index: uint64(i) * 12345, Data: data}
	}
	return recs
}

// TestBitFlipSweep flips every single bit, at every byte position, of a
// 3-record buffer. A flip must never produce a wrong record: DecodeAll's
// returned prefix must always match the true records exactly, and if it
// stops early it must stop precisely at the start of the frame the flipped
// bit falls in.
func TestBitFlipSweep(t *testing.T) {
	recs := smallRecords(3)

	var orig []byte
	starts := make([]int, len(recs))
	for i, r := range recs {
		starts[i] = len(orig)
		orig = frame.AppendRecord(orig, r)
	}

	frameOf := func(pos int) int {
		idx := 0
		for idx+1 < len(starts) && pos >= starts[idx+1] {
			idx++
		}
		return idx
	}

	for pos := 0; pos < len(orig); pos++ {
		for bit := uint(0); bit < 8; bit++ {
			trial := append([]byte(nil), orig...)
			trial[pos] ^= 1 << bit
			fi := frameOf(pos)

			got, off, err := frame.DecodeAll(trial)

			// Whatever prefix came back must be exactly correct: never a
			// corrupted or shifted record silently accepted.
			for i := range got {
				if !reflect.DeepEqual(got[i], recs[i]) {
					t.Fatalf("pos=%d bit=%d: record %d wrong: got %+v, want %+v", pos, bit, i, got[i], recs[i])
				}
			}

			if err == nil {
				if len(got) != len(recs) || off != len(trial) {
					t.Fatalf("pos=%d bit=%d: no error but incomplete decode: %d records, off=%d", pos, bit, len(got), off)
				}
				continue
			}
			if off != starts[fi] {
				t.Fatalf("pos=%d bit=%d: off = %d, want %d (start of frame %d)", pos, bit, off, starts[fi], fi)
			}
			if len(got) != fi {
				t.Fatalf("pos=%d bit=%d: got %d records, want %d (frames before the corrupted one)", pos, bit, len(got), fi)
			}
		}
	}
}

func TestOversizeLength(t *testing.T) {
	var hdr [frame.HeaderSize]byte
	binary.LittleEndian.PutUint32(hdr[0:4], frame.MaxPayload+1)
	binary.LittleEndian.PutUint32(hdr[4:8], 0)

	payload, n, err := frame.Next(hdr[:])
	if !errors.Is(err, frame.ErrOversize) {
		t.Fatalf("err = %v, want ErrOversize", err)
	}
	if payload != nil {
		t.Fatalf("payload = %v, want nil", payload)
	}
	if n != 0 {
		t.Fatalf("n = %d, want 0", n)
	}
}

func TestBadKind(t *testing.T) {
	for _, kind := range []byte{0, 99} {
		payload := make([]byte, 9)
		payload[0] = kind

		if _, err := frame.DecodeRecord(payload); !errors.Is(err, frame.ErrBadKind) {
			t.Fatalf("kind=%d: DecodeRecord err = %v, want ErrBadKind", kind, err)
		}

		buf := frame.Append(nil, payload)
		recs, off, err := frame.DecodeAll(buf)
		if !errors.Is(err, frame.ErrBadKind) {
			t.Fatalf("kind=%d: DecodeAll err = %v, want ErrBadKind", kind, err)
		}
		if off != 0 {
			t.Fatalf("kind=%d: off = %d, want 0", kind, off)
		}
		if len(recs) != 0 {
			t.Fatalf("kind=%d: got %d records, want 0", kind, len(recs))
		}
	}
}

func TestAliasing(t *testing.T) {
	rec := node.Record{Kind: node.RecordEntry, Index: 7, Data: []byte("hello")}
	buf := frame.AppendRecord(nil, rec)

	recs, _, err := frame.DecodeAll(buf)
	if err != nil {
		t.Fatalf("DecodeAll: %v", err)
	}
	want := append([]byte(nil), recs[0].Data...)

	for i := range buf {
		buf[i] = 0xFF
	}

	if !bytes.Equal(recs[0].Data, want) {
		t.Fatalf("Data changed after mutating input buffer: got %v, want %v", recs[0].Data, want)
	}
}
