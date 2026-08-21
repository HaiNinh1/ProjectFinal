package sim

import (
	"bytes"
	"fmt"
	"testing"

	"verity/internal/frame"
	"verity/node"
	"verity/prng"
)

// diskRecordHeaderSize mirrors internal/frame's unexported record header
// size (one kind byte plus an eight-byte index), so tests can compute an
// expected on-disk size without reaching into frame's internals.
const diskRecordHeaderSize = 1 + 8

// diskTestRecords returns n distinguishable records, indexed from 1.
func diskTestRecords(n int) []node.Record {
	recs := make([]node.Record, n)
	for i := 0; i < n; i++ {
		recs[i] = node.Record{
			Kind:  node.RecordEntry,
			Index: uint64(i + 1),
			Data:  []byte(fmt.Sprintf("payload-%d", i)),
		}
	}
	return recs
}

// diskFrameSize returns the number of on-disk bytes rec occupies once
// framed.
func diskFrameSize(rec node.Record) int {
	return frame.HeaderSize + diskRecordHeaderSize + len(rec.Data)
}

// diskAssertRecordsEqual fails the test unless got and want match exactly,
// record for record, in order.
func diskAssertRecordsEqual(t *testing.T, got, want []node.Record) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("record count = %d, want %d (got %+v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Kind != want[i].Kind || got[i].Index != want[i].Index || !bytes.Equal(got[i].Data, want[i].Data) {
			t.Fatalf("record %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestDiskPersistTiming(t *testing.T) {
	cfg := DiskConfig{WriteLatency: 100 * node.Microsecond, SyncLatency: 1 * node.Millisecond}
	d := NewDisk(cfg, prng.New(1))
	now := node.Time(1000)

	if got, want := d.Persist(now, diskTestRecords(1), false), now.Add(cfg.WriteLatency); got != want {
		t.Errorf("unsynced Persist returned %d, want %d", got, want)
	}
	if got, want := d.Persist(now, diskTestRecords(1), true), now.Add(cfg.WriteLatency+cfg.SyncLatency); got != want {
		t.Errorf("synced Persist returned %d, want %d", got, want)
	}
}

// TestDiskCrashLosesUnsyncedTail is T1.6's headline case: a crash before a
// sync loses everything in the tail.
func TestDiskCrashLosesUnsyncedTail(t *testing.T) {
	d := NewDisk(DiskConfig{}, prng.New(2))
	d.Persist(0, diskTestRecords(3), false)
	d.Completed(false)
	d.Crash()

	recs, discarded := d.Restore()
	if len(recs) != 0 {
		t.Fatalf("Restore returned %d records after an unsynced crash, want 0", len(recs))
	}
	if discarded != 0 {
		t.Fatalf("discarded = %d, want 0 (nothing was ever durable)", discarded)
	}
}

func TestDiskSyncedRecordsSurvive(t *testing.T) {
	want := diskTestRecords(3)
	d := NewDisk(DiskConfig{}, prng.New(3))
	d.Persist(0, want, true)
	d.Completed(true)
	d.Crash()

	got, discarded := d.Restore()
	diskAssertRecordsEqual(t, got, want)
	if discarded != 0 {
		t.Fatalf("discarded = %d, want 0", discarded)
	}
}

// TestDiskSyncFlushesWholeTail asserts, deliberately, that a sync promotes
// everything outstanding in the tail — not just the records of the persist
// whose sync is completing. This is real fsync behaviour and is documented
// on Disk.Completed because it is easy to mistake for a bug.
func TestDiskSyncFlushesWholeTail(t *testing.T) {
	recA := diskTestRecords(1)
	recB := []node.Record{{Kind: node.RecordEntry, Index: 2, Data: []byte("b")}}

	d := NewDisk(DiskConfig{}, prng.New(4))
	d.Persist(0, recA, false)
	d.Completed(false) // unsynced: no-op, A stays in the tail

	d.Persist(0, recB, true)
	d.Completed(true) // sync flushes the WHOLE tail: A and B both, intentionally

	d.Crash()
	got, _ := d.Restore()
	diskAssertRecordsEqual(t, got, append(append([]node.Record{}, recA...), recB...))
}

func TestDiskUnsyncedAfterSyncIsLost(t *testing.T) {
	recA := diskTestRecords(1)
	recB := []node.Record{{Kind: node.RecordEntry, Index: 2, Data: []byte("b")}}

	d := NewDisk(DiskConfig{}, prng.New(5))
	d.Persist(0, recA, true)
	d.Completed(true)

	d.Persist(0, recB, false)
	d.Completed(false)

	d.Crash()
	got, _ := d.Restore()
	diskAssertRecordsEqual(t, got, recA)
}

// TestDiskTornWriteRejected is T1.6's other headline case: a torn last
// record must never be returned as data, however the tear lands.
func TestDiskTornWriteRejected(t *testing.T) {
	want := diskTestRecords(3)
	for seed := uint64(0); seed < 100; seed++ {
		cfg := DiskConfig{TornRate: 1.0}
		d := NewDisk(cfg, prng.New(seed))
		d.Persist(0, want, true)
		d.Completed(true)
		d.Crash()

		got, discarded := d.Restore()
		if discarded <= 0 {
			t.Fatalf("seed %d: discarded = %d, want > 0", seed, discarded)
		}
		// The third record must not come back in any form: exactly the
		// first two, byte for byte, or the test below would already have
		// failed on a mismatch.
		diskAssertRecordsEqual(t, got, want[:2])
	}
}

func TestDiskTornRateZeroNeverTears(t *testing.T) {
	want := diskTestRecords(3)
	for seed := uint64(0); seed < 100; seed++ {
		cfg := DiskConfig{TornRate: 0.0}
		d := NewDisk(cfg, prng.New(seed))
		d.Persist(0, want, true)
		d.Completed(true)
		d.Crash()

		got, discarded := d.Restore()
		diskAssertRecordsEqual(t, got, want)
		if discarded != 0 {
			t.Fatalf("seed %d: discarded = %d, want 0", seed, discarded)
		}
	}
}

// TestDiskRestoreTruncatesForNextPersist checks that after a torn crash,
// Restore leaves the durable bytes clean enough that the next persist lands
// right after the recovered prefix, with no garbage in between.
func TestDiskRestoreTruncatesForNextPersist(t *testing.T) {
	initial := diskTestRecords(3)
	cfg := DiskConfig{TornRate: 1.0}
	d := NewDisk(cfg, prng.New(6))

	d.Persist(0, initial, true)
	d.Completed(true)
	d.Crash()

	prefix, discarded := d.Restore()
	if discarded <= 0 {
		t.Fatalf("discarded = %d, want > 0", discarded)
	}
	diskAssertRecordsEqual(t, prefix, initial[:2])

	next := []node.Record{{Kind: node.RecordEntry, Index: 99, Data: []byte("next")}}
	d.Persist(0, next, true)
	d.Completed(true)

	got, discarded2 := d.Restore()
	if discarded2 != 0 {
		t.Fatalf("second discarded = %d, want 0", discarded2)
	}
	diskAssertRecordsEqual(t, got, append(append([]node.Record{}, initial[:2]...), next...))
}

func TestDiskRestoreOnUntouchedDisk(t *testing.T) {
	d := NewDisk(DiskConfig{}, prng.New(7))
	recs, discarded := d.Restore()
	if len(recs) != 0 {
		t.Fatalf("recs = %v, want empty", recs)
	}
	if discarded != 0 {
		t.Fatalf("discarded = %d, want 0", discarded)
	}
}

// TestDiskDeterminism runs an identical scripted sequence against two Disks
// seeded alike and requires byte-identical durable content and identical
// Restore results, the property the whole simulator depends on.
func TestDiskDeterminism(t *testing.T) {
	script := func(d *Disk) {
		d.Persist(0, diskTestRecords(2), false)
		d.Completed(false)
		d.Persist(0, diskTestRecords(2), true)
		d.Completed(true)
		d.Crash()
		d.Persist(0, []node.Record{{Kind: node.RecordEntry, Index: 5, Data: []byte("more")}}, true)
		d.Completed(true)
		d.Crash()
	}

	cfg := DiskConfig{TornRate: 0.5}
	d1 := NewDisk(cfg, prng.New(42))
	d2 := NewDisk(cfg, prng.New(42))
	script(d1)
	script(d2)

	if !bytes.Equal(d1.durable, d2.durable) {
		t.Fatalf("durable bytes diverged:\n%x\n%x", d1.durable, d2.durable)
	}

	recs1, discarded1 := d1.Restore()
	recs2, discarded2 := d2.Restore()
	if discarded1 != discarded2 {
		t.Fatalf("discarded diverged: %d vs %d", discarded1, discarded2)
	}
	diskAssertRecordsEqual(t, recs1, recs2)
}

func TestDiskSizesTrackThroughSequence(t *testing.T) {
	d := NewDisk(DiskConfig{}, prng.New(8))
	if got := d.DurableSize(); got != 0 {
		t.Fatalf("initial DurableSize = %d, want 0", got)
	}
	if got := d.TailSize(); got != 0 {
		t.Fatalf("initial TailSize = %d, want 0", got)
	}

	recs := diskTestRecords(2)
	wantTail := diskFrameSize(recs[0]) + diskFrameSize(recs[1])
	d.Persist(0, recs, false)
	if got := d.TailSize(); got != wantTail {
		t.Fatalf("TailSize after unsynced persist = %d, want %d", got, wantTail)
	}
	if got := d.DurableSize(); got != 0 {
		t.Fatalf("DurableSize after unsynced persist = %d, want 0", got)
	}

	d.Completed(false)
	if got := d.TailSize(); got != wantTail {
		t.Fatalf("TailSize after Completed(false) = %d, want %d (unchanged)", got, wantTail)
	}

	more := []node.Record{{Kind: node.RecordEntry, Index: 3, Data: []byte("z")}}
	wantTail += diskFrameSize(more[0])
	d.Persist(0, more, true)

	d.Completed(true)
	if got := d.TailSize(); got != 0 {
		t.Fatalf("TailSize after Completed(true) = %d, want 0", got)
	}
	if got := d.DurableSize(); got != wantTail {
		t.Fatalf("DurableSize after Completed(true) = %d, want %d", got, wantTail)
	}
}

func TestDiskPersistZeroRecordsNoop(t *testing.T) {
	cfg := DiskConfig{WriteLatency: 10 * node.Microsecond}
	d := NewDisk(cfg, prng.New(9))

	now := node.Time(500)
	got := d.Persist(now, nil, false)
	if want := now.Add(cfg.WriteLatency); got != want {
		t.Fatalf("Persist(nil) returned %d, want %d", got, want)
	}
	if got := d.TailSize(); got != 0 {
		t.Fatalf("TailSize after empty persist = %d, want 0", got)
	}

	// A subsequent real persist still works normally.
	recs := diskTestRecords(1)
	d.Persist(now, recs, false)
	if got, want := d.TailSize(), diskFrameSize(recs[0]); got != want {
		t.Fatalf("TailSize after real persist = %d, want %d", got, want)
	}
}
