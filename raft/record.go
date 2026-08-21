package raft

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"

	"verity/node"
)

// Sizes of the fixed portions of each record payload. Kind and Index are
// carried by node.Record itself and by internal/frame's header, so a payload
// only holds what those two do not.
const (
	// hardStateSize is Term | VotedFor | CommitIndex, eight bytes each.
	hardStateSize = 24
	// entryHeaderSize is Term | Type. The command is whatever follows, and
	// needs no length prefix because the frame already delimits the record.
	entryHeaderSize = 9
	// snapshotHeaderSize is LastTerm | the number of nodes in Config.
	snapshotHeaderSize = 12
)

// Errors reported by the record codec. Each can be tested with errors.Is.
//
// A record that fails to decode is corruption, not a torn write: the framing
// layer (internal/frame) already stops at the first frame whose checksum fails
// and hands Restore the clean prefix. Anything that reaches this codec passed
// its CRC, so a failure here means the bytes were written wrong rather than
// lost, and it is reported rather than skipped.
var (
	// ErrShortRecord means a payload was smaller than its fixed header.
	ErrShortRecord = errors.New("raft: short record")
	// ErrBadRecordKind means a record carried a kind this package never writes.
	ErrBadRecordKind = errors.New("raft: unknown record kind")
	// ErrBadEntryType means a log entry carried a type this build does not
	// know how to interpret, which is what reading a log written by a newer
	// version looks like.
	ErrBadEntryType = errors.New("raft: unknown entry type")
)

// Snapshot is a point-in-time capture of the state machine, together with the
// log position and cluster configuration it was taken at.
//
// Data is opaque here: Raft neither produces nor interprets it, it only stores
// it and ships it to followers that have fallen too far behind. The format of
// RecordSnapshot is fixed by SPEC section 6.2.
type Snapshot struct {
	LastIndex Index
	LastTerm  Term
	Config    []node.NodeID
	Data      []byte
}

// encodeHardState renders a hard state as the single durable record that
// supersedes every earlier one. Its Record.Index is zero: a hard state
// describes the node, not a log position.
func encodeHardState(h HardState) node.Record {
	data := make([]byte, hardStateSize)
	binary.LittleEndian.PutUint64(data[0:8], uint64(h.Term))
	binary.LittleEndian.PutUint64(data[8:16], uint64(h.VotedFor))
	binary.LittleEndian.PutUint64(data[16:24], uint64(h.CommitIndex))
	return node.Record{Kind: node.RecordHardState, Index: 0, Data: data}
}

// decodeHardState reverses encodeHardState.
func decodeHardState(rec node.Record) (HardState, error) {
	if len(rec.Data) < hardStateSize {
		return HardState{}, fmt.Errorf("raft: %w: hard state needs %d bytes, have %d", ErrShortRecord, hardStateSize, len(rec.Data))
	}
	return HardState{
		Term:        Term(binary.LittleEndian.Uint64(rec.Data[0:8])),
		VotedFor:    node.NodeID(binary.LittleEndian.Uint64(rec.Data[8:16])),
		CommitIndex: Index(binary.LittleEndian.Uint64(rec.Data[16:24])),
	}, nil
}

// encodeEntry renders one log entry as a durable record. The entry's index
// travels in Record.Index rather than in the payload, so it is not repeated.
func encodeEntry(e Entry) node.Record {
	data := make([]byte, entryHeaderSize+len(e.Cmd))
	binary.LittleEndian.PutUint64(data[0:8], uint64(e.Term))
	data[8] = byte(e.Type)
	copy(data[entryHeaderSize:], e.Cmd)
	return node.Record{Kind: node.RecordEntry, Index: uint64(e.Index), Data: data}
}

// decodeEntry reverses encodeEntry, taking the entry's index from the record.
//
// The returned command aliases rec.Data. That is safe on the path this codec
// exists for: frame.DecodeAll gives Restore freshly copied record data that
// nothing else holds a reference to.
func decodeEntry(rec node.Record) (Entry, error) {
	if len(rec.Data) < entryHeaderSize {
		return Entry{}, fmt.Errorf("raft: %w: entry needs %d bytes, have %d", ErrShortRecord, entryHeaderSize, len(rec.Data))
	}
	t := EntryType(rec.Data[8])
	if !t.Valid() {
		return Entry{}, fmt.Errorf("raft: %w: entry type %d at index %d", ErrBadEntryType, rec.Data[8], rec.Index)
	}
	e := Entry{
		Term:  Term(binary.LittleEndian.Uint64(rec.Data[0:8])),
		Index: Index(rec.Index),
		Type:  t,
	}
	if len(rec.Data) > entryHeaderSize {
		e.Cmd = rec.Data[entryHeaderSize:]
	}
	return e, nil
}

// encodeSnapshot renders a snapshot as a durable record.
//
// The configuration is sorted before it is written. Nothing downstream depends
// on the order, but a durable record whose bytes vary with the order a caller
// happened to assemble a slice in would make two runs that agree on every
// decision disagree on disk, and the determinism story does not survive
// exceptions (INV-6). Sorting a copy also leaves the caller's slice alone.
func encodeSnapshot(s Snapshot) node.Record {
	cfg := make([]node.NodeID, len(s.Config))
	copy(cfg, s.Config)
	sort.Slice(cfg, func(i, j int) bool { return cfg[i] < cfg[j] })

	data := make([]byte, snapshotHeaderSize+8*len(cfg)+len(s.Data))
	binary.LittleEndian.PutUint64(data[0:8], uint64(s.LastTerm))
	binary.LittleEndian.PutUint32(data[8:12], uint32(len(cfg)))
	off := snapshotHeaderSize
	for _, id := range cfg {
		binary.LittleEndian.PutUint64(data[off:off+8], uint64(id))
		off += 8
	}
	copy(data[off:], s.Data)
	return node.Record{Kind: node.RecordSnapshot, Index: uint64(s.LastIndex), Data: data}
}

// decodeSnapshot reverses encodeSnapshot. Config and Data are copied out, so
// the result shares nothing with rec.
func decodeSnapshot(rec node.Record) (Snapshot, error) {
	if len(rec.Data) < snapshotHeaderSize {
		return Snapshot{}, fmt.Errorf("raft: %w: snapshot needs %d bytes, have %d", ErrShortRecord, snapshotHeaderSize, len(rec.Data))
	}
	s := Snapshot{
		LastIndex: Index(rec.Index),
		LastTerm:  Term(binary.LittleEndian.Uint64(rec.Data[0:8])),
	}
	count := int(binary.LittleEndian.Uint32(rec.Data[8:12]))
	// Guard the count before trusting it as a length: a corrupt record must
	// fail the length check, not allocate whatever number it happens to name.
	//
	// The first two tests are unreachable where int is 64 bits, since a
	// uint32 widens into it without overflowing. They are kept because they
	// are not unreachable on a 32-bit build, where 8*count can wrap and turn
	// a corrupt length into a negative one that passes the third test.
	need := snapshotHeaderSize + 8*count
	if count < 0 || need < snapshotHeaderSize || len(rec.Data) < need {
		return Snapshot{}, fmt.Errorf("raft: %w: snapshot names %d config members, needing %d bytes, have %d", ErrShortRecord, count, need, len(rec.Data))
	}
	if count > 0 {
		s.Config = make([]node.NodeID, count)
		off := snapshotHeaderSize
		for i := range s.Config {
			s.Config[i] = node.NodeID(binary.LittleEndian.Uint64(rec.Data[off : off+8]))
			off += 8
		}
	}
	if rest := rec.Data[need:]; len(rest) > 0 {
		s.Data = make([]byte, len(rest))
		copy(s.Data, rest)
	}
	return s, nil
}
