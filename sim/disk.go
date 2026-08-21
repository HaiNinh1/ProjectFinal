package sim

import (
	"verity/internal/frame"
	"verity/node"
	"verity/prng"
)

// DiskConfig holds the storage parameters for one run.
type DiskConfig struct {
	WriteLatency node.Duration // time to append to the unsynced tail
	SyncLatency  node.Duration // additional time for a sync to reach stable storage
	TornRate     float64       // probability that a crash also tears the last durable record
}

// Disk is a self-contained model of one node's storage: an unsynced tail
// that a crash discards, and a durable region that survives, subject to the
// torn-write chance drawn on the way down. It does not know about the
// timeline or any node; it only reports when a persist would complete, at
// node.Time, node.Time — the caller (the driver wiring sim together) is what
// schedules the matching PersistDone. That separation is what makes Disk
// testable on its own, without a cluster around it.
//
// Records are framed with internal/frame's length | crc32 | payload
// encoding, the same code store's real WAL uses. Sharing the encoding is
// what lets a torn write produced here be rejected by the same loader logic
// a real recovery would run (docs/SPEC.md §5.3).
type Disk struct {
	cfg DiskConfig
	rng *prng.Rand

	tail            []byte // framed bytes written but not yet synced
	tailFrameStarts []int  // offset of each frame's header within tail

	durable            []byte // framed bytes that have reached stable storage
	durableFrameStarts []int  // offset of each frame's header within durable
}

// NewDisk returns a Disk governed by cfg, drawing its torn-write decisions
// from rng.
func NewDisk(cfg DiskConfig, rng *prng.Rand) *Disk {
	return &Disk{cfg: cfg, rng: rng}
}

// Persist stages recs and returns the time at which the matching
// PersistDone should fire.
//
// The framed bytes are appended to the unsynced tail immediately, before
// Persist returns, so that on-disk order always matches write order
// regardless of when the caller later reports completion. If sync is false
// the records remain in the tail — durable only once a later Completed(true)
// promotes them. If sync is true the caller must arrange for the returned
// time to include the sync, which is why WriteLatency alone is added when
// sync is false and WriteLatency+SyncLatency when it is true.
func (d *Disk) Persist(now node.Time, recs []node.Record, sync bool) node.Time {
	for _, rec := range recs {
		start := len(d.tail)
		d.tail = frame.AppendRecord(d.tail, rec)
		d.tailFrameStarts = append(d.tailFrameStarts, start)
	}
	if sync {
		return now.Add(d.cfg.WriteLatency + d.cfg.SyncLatency)
	}
	return now.Add(d.cfg.WriteLatency)
}

// Completed applies the durability effect of a persist whose PersistDone is
// firing now. sync must match the sync flag of the Persist call it answers.
//
// Completed(false) does nothing: the records stay in the tail, still lost on
// a crash. That is INV-8 made concrete — a node that treats an unsynced
// PersistDone as durable is a bug, and the model must let that bug happen
// rather than quietly saving it.
//
// Completed(true) moves the entire unsynced tail to durable, not only the
// records belonging to the persist that is completing. This matches real
// fsync semantics: a sync flushes everything outstanding, including writes
// issued after the one being synced. It is surprising on first read and easy
// to "fix" into per-persist tracking, which would be wrong — leave it as is.
func (d *Disk) Completed(sync bool) {
	if !sync {
		return
	}
	base := len(d.durable)
	d.durable = append(d.durable, d.tail...)
	for _, off := range d.tailFrameStarts {
		d.durableFrameStarts = append(d.durableFrameStarts, base+off)
	}
	d.tail = d.tail[:0]
	d.tailFrameStarts = d.tailFrameStarts[:0]
}

// Crash discards everything that was written but not synced.
//
// With probability TornRate it additionally tears the last durable record,
// truncating the durable bytes to a point strictly inside that record's
// frame so the frame is left incomplete — neither absent nor whole. The
// Chance draw is always made, even when there is no durable record to tear,
// so that adding or removing records from a test does not shift the rng
// stream for anything drawn afterward (mirrors prng.Chance's own reasoning).
func (d *Disk) Crash() {
	d.tail = d.tail[:0]
	d.tailFrameStarts = d.tailFrameStarts[:0]

	torn := d.rng.Chance(d.cfg.TornRate)
	if !torn || len(d.durableFrameStarts) == 0 {
		return
	}
	lastStart := d.durableFrameStarts[len(d.durableFrameStarts)-1]
	frameSize := len(d.durable) - lastStart
	// frameSize is at least frame.HeaderSize (8), so there is always at
	// least one interior point to truncate to.
	newLen := lastStart + 1 + d.rng.Intn(frameSize-1)
	d.durable = d.durable[:newLen]
}

// Restore returns the records that survived, in write order, and the number
// of trailing bytes discarded as unreadable.
//
// The durable bytes are decoded with frame.DecodeAll, which stops at the
// first frame that fails to decode cleanly — a torn trailing write among
// them. That is how a torn write is survived rather than mistaken for data:
// the good prefix is returned, the rest is reported only as a byte count.
//
// Restore also truncates the durable bytes to the recovered length, the way
// a real WAL truncates at recovery, so a subsequent Persist appends after
// clean data rather than after garbage.
func (d *Disk) Restore() (recs []node.Record, discarded int) {
	recs, off, _ := frame.DecodeAll(d.durable)
	discarded = len(d.durable) - off
	d.durable = d.durable[:off]

	i := 0
	for i < len(d.durableFrameStarts) && d.durableFrameStarts[i] < off {
		i++
	}
	d.durableFrameStarts = d.durableFrameStarts[:i]

	return recs, discarded
}

// DurableSize reports the number of bytes that have reached stable storage.
func (d *Disk) DurableSize() int { return len(d.durable) }

// TailSize reports the number of bytes written but not yet synced.
func (d *Disk) TailSize() int { return len(d.tail) }
