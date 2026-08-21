package raft

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"

	"verity/node"
)

// recMaxUint64 is 2^64-1, written as a literal since node packages, and this
// test's subject, must not import math. It proves that Term, VotedFor,
// CommitIndex and Index survive the codec without truncation or
// sign-extension.
const recMaxUint64 = 18446744073709551615

// sortedCopy returns a sorted copy of ids without touching ids itself. It
// returns nil for an empty input, matching decodeSnapshot's behaviour of
// never manufacturing a non-nil empty Config.
func sortedCopy(ids []node.NodeID) []node.NodeID {
	if len(ids) == 0 {
		return nil
	}
	out := make([]node.NodeID, len(ids))
	copy(out, ids)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ------------------------------------------------------------ round trips ---

func TestHardStateRoundTrips(t *testing.T) {
	cases := []struct {
		name string
		h    HardState
	}{
		{"zero value", HardState{}},
		{"None as VotedFor", HardState{Term: 5, VotedFor: None, CommitIndex: 5}},
		{"max values", HardState{Term: Term(recMaxUint64), VotedFor: node.NodeID(recMaxUint64), CommitIndex: Index(recMaxUint64)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := encodeHardState(tc.h)
			got, err := decodeHardState(rec)
			if err != nil {
				t.Fatalf("%s: decodeHardState: unexpected error: %v", tc.name, err)
			}
			if !got.Equal(tc.h) {
				t.Fatalf("%s: round trip: got %+v, want %+v", tc.name, got, tc.h)
			}
		})
	}
}

func TestEntryRoundTrips(t *testing.T) {
	bigCmd := make([]byte, 100*1024)
	for i := range bigCmd {
		bigCmd[i] = byte(i % 256)
	}

	cases := []struct {
		name string
		e    Entry
	}{
		{"zero value", Entry{}},
		{"max term and index", Entry{Term: Term(recMaxUint64), Index: Index(recMaxUint64), Type: EntryNormal}},
		{"empty cmd", Entry{Term: 1, Index: 1, Type: EntryNormal, Cmd: []byte{}}},
		{"nil cmd", Entry{Term: 1, Index: 1, Type: EntryNoop, Cmd: nil}},
		{"large cmd", Entry{Term: 7, Index: 100, Type: EntryConfig, Cmd: bigCmd}},
		{"EntryNormal", Entry{Term: 2, Index: 2, Type: EntryNormal, Cmd: []byte("normal")}},
		{"EntryNoop", Entry{Term: 3, Index: 3, Type: EntryNoop, Cmd: []byte("noop-carries-nothing-by-convention-but-decodes-fine")}},
		{"EntryConfig", Entry{Term: 4, Index: 4, Type: EntryConfig, Cmd: []byte("config")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := encodeEntry(tc.e)
			got, err := decodeEntry(rec)
			if err != nil {
				t.Fatalf("%s: decodeEntry: unexpected error: %v", tc.name, err)
			}
			if got.Term != tc.e.Term {
				t.Fatalf("%s: Term: got %d, want %d", tc.name, got.Term, tc.e.Term)
			}
			if got.Index != tc.e.Index {
				t.Fatalf("%s: Index: got %d, want %d", tc.name, got.Index, tc.e.Index)
			}
			if got.Type != tc.e.Type {
				t.Fatalf("%s: Type: got %v, want %v", tc.name, got.Type, tc.e.Type)
			}
			// bytes.Equal treats nil and an empty slice as equal, which is
			// the right comparison here: decodeEntry never distinguishes a
			// nil Cmd from an empty one (both produce a payload of exactly
			// entryHeaderSize), so asserting reflect.DeepEqual would make
			// this test depend on a distinction the codec does not keep.
			if !bytes.Equal(got.Cmd, tc.e.Cmd) {
				t.Fatalf("%s: Cmd: got %v, want %v", tc.name, got.Cmd, tc.e.Cmd)
			}
		})
	}
}

func TestSnapshotRoundTrips(t *testing.T) {
	cases := []struct {
		name string
		s    Snapshot
	}{
		{"zero value", Snapshot{}},
		{"max values", Snapshot{
			LastIndex: Index(recMaxUint64),
			LastTerm:  Term(recMaxUint64),
			Config:    []node.NodeID{node.NodeID(recMaxUint64), 0},
			Data:      []byte{0xFF},
		}},
		{"several config members and data", Snapshot{
			LastIndex: 5,
			LastTerm:  2,
			Config:    []node.NodeID{9, 1, 5},
			Data:      []byte("snap-data"),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := encodeSnapshot(tc.s)
			got, err := decodeSnapshot(rec)
			if err != nil {
				t.Fatalf("%s: decodeSnapshot: unexpected error: %v", tc.name, err)
			}
			if got.LastIndex != tc.s.LastIndex {
				t.Fatalf("%s: LastIndex: got %d, want %d", tc.name, got.LastIndex, tc.s.LastIndex)
			}
			if got.LastTerm != tc.s.LastTerm {
				t.Fatalf("%s: LastTerm: got %d, want %d", tc.name, got.LastTerm, tc.s.LastTerm)
			}
			wantCfg := sortedCopy(tc.s.Config) // encodeSnapshot sorts Config; see INV-6
			if !reflect.DeepEqual(got.Config, wantCfg) {
				t.Fatalf("%s: Config: got %v, want %v", tc.name, got.Config, wantCfg)
			}
			if !bytes.Equal(got.Data, tc.s.Data) {
				t.Fatalf("%s: Data: got %v, want %v", tc.name, got.Data, tc.s.Data)
			}
		})
	}
}

// ---------------------------------------------------------- field placement ---

// TestEncodeHardStateFieldPlacement checks the parts of encodeHardState that a
// round trip alone cannot: which Kind and Index the record carries, and the
// exact payload length. A codec that swapped two fields, or wrote them to the
// wrong offsets, could still round-trip correctly through its own inverse
// while producing a record no other build (or a hand inspection of the file)
// could make sense of.
func TestEncodeHardStateFieldPlacement(t *testing.T) {
	h := HardState{Term: 9, VotedFor: 3, CommitIndex: 4}
	rec := encodeHardState(h)

	if rec.Kind != node.RecordHardState {
		t.Fatalf("Kind = %v, want RecordHardState", rec.Kind)
	}
	// A hard state describes the node, not a log position, so Index is
	// always zero regardless of the state's own contents.
	if rec.Index != 0 {
		t.Fatalf("Index = %d, want 0", rec.Index)
	}
	if len(rec.Data) != hardStateSize {
		t.Fatalf("len(Data) = %d, want %d", len(rec.Data), hardStateSize)
	}
}

// TestEncodeEntryFieldPlacement checks that the entry's index travels in
// Record.Index rather than being repeated inside the payload. Two entries
// that differ only in Index must produce byte-identical payloads: if the
// index leaked into the payload as well, this would fail.
func TestEncodeEntryFieldPlacement(t *testing.T) {
	e1 := Entry{Term: 6, Index: 11, Type: EntryNormal, Cmd: []byte("cmd")}
	e2 := Entry{Term: 6, Index: 999, Type: EntryNormal, Cmd: []byte("cmd")}

	rec1 := encodeEntry(e1)
	rec2 := encodeEntry(e2)

	if rec1.Kind != node.RecordEntry {
		t.Fatalf("Kind = %v, want RecordEntry", rec1.Kind)
	}
	if rec1.Index != uint64(e1.Index) {
		t.Fatalf("Index = %d, want %d", rec1.Index, e1.Index)
	}
	if rec2.Index != uint64(e2.Index) {
		t.Fatalf("Index = %d, want %d", rec2.Index, e2.Index)
	}
	wantLen := entryHeaderSize + len(e1.Cmd)
	if len(rec1.Data) != wantLen {
		t.Fatalf("len(Data) = %d, want %d", len(rec1.Data), wantLen)
	}
	if !bytes.Equal(rec1.Data, rec2.Data) {
		t.Fatalf("payloads differ despite identical fields other than Index: %v vs %v (the index must not be repeated in the payload)", rec1.Data, rec2.Data)
	}
}

// TestDecodeEntryTakesIndexFromRecord decodes a record whose Index has been
// altered after encoding and checks the decoded Entry follows the record, not
// whatever index the entry was originally encoded with. This proves Index
// really is read from Record.Index and not smuggled through the payload.
func TestDecodeEntryTakesIndexFromRecord(t *testing.T) {
	e := Entry{Term: 2, Index: 7, Type: EntryNormal, Cmd: []byte("x")}
	rec := encodeEntry(e)

	rec.Index = 4242 // deliberately diverge from the entry's original Index

	got, err := decodeEntry(rec)
	if err != nil {
		t.Fatalf("decodeEntry: unexpected error: %v", err)
	}
	if got.Index != 4242 {
		t.Fatalf("Index = %d, want 4242 (the altered record index, not the original 7)", got.Index)
	}
}

func TestEncodeSnapshotFieldPlacement(t *testing.T) {
	s := Snapshot{LastIndex: 77, LastTerm: 3, Config: []node.NodeID{1, 2}, Data: []byte("d")}
	rec := encodeSnapshot(s)

	if rec.Kind != node.RecordSnapshot {
		t.Fatalf("Kind = %v, want RecordSnapshot", rec.Kind)
	}
	if rec.Index != uint64(s.LastIndex) {
		t.Fatalf("Index = %d, want %d", rec.Index, s.LastIndex)
	}
}

// ------------------------------------------------------------ golden bytes ---

// TestHardStateGoldenBytes pins encodeHardState's byte layout to a hardcoded
// literal computed by hand from the documented format (SPEC 6.2): Term |
// VotedFor | CommitIndex, eight little-endian bytes each. The encoding is
// frozen because it is what already sits on disk in every existing log; this
// test exists so that changing the layout requires deliberately editing this
// literal, rather than happening as a side effect of an unrelated refactor
// that a round-trip test alone would wave through.
func TestHardStateGoldenBytes(t *testing.T) {
	h := HardState{
		Term:        0x0102030405060708,
		VotedFor:    node.NodeID(0x1112131415161718),
		CommitIndex: Index(0x2122232425262728),
	}
	want := []byte{
		0x08, 0x07, 0x06, 0x05, 0x04, 0x03, 0x02, 0x01, // Term
		0x18, 0x17, 0x16, 0x15, 0x14, 0x13, 0x12, 0x11, // VotedFor
		0x28, 0x27, 0x26, 0x25, 0x24, 0x23, 0x22, 0x21, // CommitIndex
	}

	got := encodeHardState(h).Data
	if !bytes.Equal(got, want) {
		t.Fatalf("got % x, want % x", got, want)
	}
}

// TestEntryGoldenBytes pins encodeEntry's byte layout: Term (8 bytes LE) |
// Type (1 byte) | Cmd. See TestHardStateGoldenBytes for why this exists.
func TestEntryGoldenBytes(t *testing.T) {
	e := Entry{
		Term: 0x0102030405060708,
		Type: EntryConfig,
		Cmd:  []byte{0xDE, 0xAD, 0xBE, 0xEF},
	}
	want := []byte{
		0x08, 0x07, 0x06, 0x05, 0x04, 0x03, 0x02, 0x01, // Term
		0x02,                   // Type = EntryConfig
		0xDE, 0xAD, 0xBE, 0xEF, // Cmd
	}

	got := encodeEntry(e).Data
	if !bytes.Equal(got, want) {
		t.Fatalf("got % x, want % x", got, want)
	}
}

// ------------------------------------------------------ config determinism ---

// TestEncodeSnapshotConfigOrderIsDeterministic feeds encodeSnapshot the same
// membership in several different input orders and checks every result is
// byte-identical. INV-6 (docs/SPEC.md) exists exactly for this: Go
// deliberately randomises map iteration, and the same discipline applies here
// by hand — a durable record must not vary with the order a caller happened
// to assemble a slice in, or two runs that agree on every decision would
// disagree on disk.
func TestEncodeSnapshotConfigOrderIsDeterministic(t *testing.T) {
	orders := [][]node.NodeID{
		{1, 2, 3, 4, 5},
		{5, 4, 3, 2, 1},
		{3, 1, 4, 5, 2},
		{2, 5, 1, 4, 3},
	}

	var want []byte
	for i, cfg := range orders {
		rec := encodeSnapshot(Snapshot{LastIndex: 1, LastTerm: 1, Config: cfg})
		if i == 0 {
			want = rec.Data
			continue
		}
		if !bytes.Equal(rec.Data, want) {
			t.Fatalf("order %v: got % x, want % x (order %v)", cfg, rec.Data, want, orders[0])
		}
	}
}

// TestEncodeSnapshotDoesNotMutateCallerConfig checks that sorting happens on
// a copy: encodeSnapshot must leave the caller's slice in whatever order it
// was passed in.
func TestEncodeSnapshotDoesNotMutateCallerConfig(t *testing.T) {
	cfg := []node.NodeID{5, 3, 1, 4, 2}
	before := append([]node.NodeID(nil), cfg...)

	encodeSnapshot(Snapshot{LastIndex: 1, LastTerm: 1, Config: cfg})

	if !reflect.DeepEqual(cfg, before) {
		t.Fatalf("caller's Config mutated by encodeSnapshot: got %v, want %v", cfg, before)
	}
}

// -------------------------------------------------------------- copy vs alias ---

// TestDecodeSnapshotCopiesConfigAndData checks decodeSnapshot's documented
// property that Config and Data share nothing with the source record:
// mutating rec.Data after decoding must not change an already-decoded
// Snapshot.
func TestDecodeSnapshotCopiesConfigAndData(t *testing.T) {
	s := Snapshot{LastIndex: 1, LastTerm: 1, Config: []node.NodeID{3, 1, 2}, Data: []byte("payload")}
	rec := encodeSnapshot(s)

	got, err := decodeSnapshot(rec)
	if err != nil {
		t.Fatalf("decodeSnapshot: unexpected error: %v", err)
	}
	wantCfg := append([]node.NodeID(nil), got.Config...)
	wantData := append([]byte(nil), got.Data...)

	for i := range rec.Data {
		rec.Data[i] = 0xFF
	}

	if !reflect.DeepEqual(got.Config, wantCfg) {
		t.Fatalf("Config changed after mutating rec.Data: got %v, want %v", got.Config, wantCfg)
	}
	if !bytes.Equal(got.Data, wantData) {
		t.Fatalf("Data changed after mutating rec.Data: got %v, want %v", got.Data, wantData)
	}
}

// TestDecodeEntryCmdAliasesRecordData documents, rather than merely
// tolerates, decodeEntry's aliasing of rec.Data: mutating the source record
// after decoding changes the decoded Cmd too. The doc comment on decodeEntry
// explains why this is safe on the path it exists for (Restore, fed by
// frame.DecodeAll's freshly copied records that nothing else references) —
// this test exists so that a future change to that aliasing is a deliberate
// decision, not an accidental regression caught only by a flaky downstream
// symptom.
func TestDecodeEntryCmdAliasesRecordData(t *testing.T) {
	e := Entry{Term: 1, Index: 1, Type: EntryNormal, Cmd: []byte("original")}
	rec := encodeEntry(e)

	got, err := decodeEntry(rec)
	if err != nil {
		t.Fatalf("decodeEntry: unexpected error: %v", err)
	}
	if !bytes.Equal(got.Cmd, []byte("original")) {
		t.Fatalf("Cmd = %q before mutation, want %q", got.Cmd, "original")
	}

	for i := entryHeaderSize; i < len(rec.Data); i++ {
		rec.Data[i] = '!'
	}

	if bytes.Equal(got.Cmd, []byte("original")) {
		t.Fatal("Cmd unaffected by mutating rec.Data: decodeEntry is documented to alias, want the mutation visible")
	}
	if !bytes.Equal(got.Cmd, bytes.Repeat([]byte("!"), len("original"))) {
		t.Fatalf("Cmd = %q after mutating rec.Data, want %q (aliased)", got.Cmd, bytes.Repeat([]byte("!"), len("original")))
	}
}

// ------------------------------------------------------------- corruption ---

// TestDecodeHardStateRejectsShortPayload feeds decodeHardState every payload
// length shorter than hardStateSize and checks each is rejected with
// ErrShortRecord rather than reading past the end of a short slice.
func TestDecodeHardStateRejectsShortPayload(t *testing.T) {
	for n := 0; n < hardStateSize; n++ {
		rec := node.Record{Kind: node.RecordHardState, Data: make([]byte, n)}
		if _, err := decodeHardState(rec); !errors.Is(err, ErrShortRecord) {
			t.Fatalf("len=%d: err = %v, want wraps ErrShortRecord", n, err)
		}
	}
}

// TestDecodeEntryRejectsShortPayload feeds decodeEntry every payload length
// shorter than entryHeaderSize and checks each is rejected with
// ErrShortRecord.
func TestDecodeEntryRejectsShortPayload(t *testing.T) {
	for n := 0; n < entryHeaderSize; n++ {
		rec := node.Record{Kind: node.RecordEntry, Index: 5, Data: make([]byte, n)}
		if _, err := decodeEntry(rec); !errors.Is(err, ErrShortRecord) {
			t.Fatalf("len=%d: err = %v, want wraps ErrShortRecord", n, err)
		}
	}
}

// TestDecodeSnapshotRejectsShortPayload feeds decodeSnapshot every payload
// length shorter than snapshotHeaderSize and checks each is rejected with
// ErrShortRecord.
func TestDecodeSnapshotRejectsShortPayload(t *testing.T) {
	for n := 0; n < snapshotHeaderSize; n++ {
		rec := node.Record{Kind: node.RecordSnapshot, Data: make([]byte, n)}
		if _, err := decodeSnapshot(rec); !errors.Is(err, ErrShortRecord) {
			t.Fatalf("len=%d: err = %v, want wraps ErrShortRecord", n, err)
		}
	}
}

// TestDecodeEntryRejectsUnknownType feeds decodeEntry several type bytes that
// EntryType.Valid rejects and checks each is reported as ErrBadEntryType, not
// silently accepted as some other type.
func TestDecodeEntryRejectsUnknownType(t *testing.T) {
	for _, bad := range []byte{3, 7, 255} {
		t.Run(fmt.Sprintf("type=%d", bad), func(t *testing.T) {
			data := make([]byte, entryHeaderSize)
			data[8] = bad
			rec := node.Record{Kind: node.RecordEntry, Index: 1, Data: data}
			if _, err := decodeEntry(rec); !errors.Is(err, ErrBadEntryType) {
				t.Fatalf("type=%d: err = %v, want wraps ErrBadEntryType", bad, err)
			}
		})
	}
}

// TestDecodeSnapshotRejectsOversizedConfigCount feeds decodeSnapshot a
// declared config count that is far larger than the bytes actually present,
// including the largest value the count field can hold. The guard in
// decodeSnapshot exists to reject this with ErrShortRecord before it is ever
// trusted as an allocation size — a payload naming a huge count must not make
// the decoder attempt a giant allocation or panic on a slice bounds check.
func TestDecodeSnapshotRejectsOversizedConfigCount(t *testing.T) {
	cases := []uint32{
		5,          // claims 5 members (40 bytes) but supplies none
		1 << 20,    // a plausible-looking but still false count
		0xFFFFFFFF, // the largest value the count field can encode
	}
	for _, count := range cases {
		t.Run(fmt.Sprintf("count=%d", count), func(t *testing.T) {
			data := make([]byte, snapshotHeaderSize)
			binary.LittleEndian.PutUint32(data[8:12], count)
			rec := node.Record{Kind: node.RecordSnapshot, Data: data}
			if _, err := decodeSnapshot(rec); !errors.Is(err, ErrShortRecord) {
				t.Fatalf("count=%d: err = %v, want wraps ErrShortRecord", count, err)
			}
		})
	}
}

// ----------------------------------------------------------- empty combos ---

// TestSnapshotEmptyConfigAndDataCombinations round-trips a snapshot with
// every combination of an empty/absent Config and Data. decodeSnapshot never
// manufactures a non-nil empty slice for either field, so this asserts on
// len() rather than reflect.DeepEqual against nil: the guarantee is "empty",
// not "identically nil".
func TestSnapshotEmptyConfigAndDataCombinations(t *testing.T) {
	cases := []struct {
		name   string
		config []node.NodeID
		data   []byte
	}{
		{"empty config and empty data", nil, nil},
		{"config but no data", []node.NodeID{3, 1, 2}, nil},
		{"data but no config", nil, []byte("payload")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := Snapshot{LastIndex: 1, LastTerm: 1, Config: tc.config, Data: tc.data}
			rec := encodeSnapshot(s)
			got, err := decodeSnapshot(rec)
			if err != nil {
				t.Fatalf("%s: decodeSnapshot: unexpected error: %v", tc.name, err)
			}

			if len(tc.config) == 0 {
				if len(got.Config) != 0 {
					t.Fatalf("%s: len(Config) = %d, want 0", tc.name, len(got.Config))
				}
			} else {
				want := sortedCopy(tc.config)
				if !reflect.DeepEqual(got.Config, want) {
					t.Fatalf("%s: Config = %v, want %v", tc.name, got.Config, want)
				}
			}

			if len(tc.data) == 0 {
				if len(got.Data) != 0 {
					t.Fatalf("%s: len(Data) = %d, want 0", tc.name, len(got.Data))
				}
			} else if !bytes.Equal(got.Data, tc.data) {
				t.Fatalf("%s: Data = %v, want %v", tc.name, got.Data, tc.data)
			}
		})
	}
}
