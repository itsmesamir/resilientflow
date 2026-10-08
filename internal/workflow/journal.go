package workflow

import (
	"encoding/json"
	"sync"
)

// RecordKind distinguishes activity results from the recorded outputs of the
// nondeterministic helpers (Now, Random, UUID). Both must be journaled, because
// replay reproduces a run by replaying its recorded decisions, not by making
// them again (ADR-001).
type RecordKind string

const (
	KindActivity RecordKind = "activity"
	KindNow      RecordKind = "now"
	KindRandom   RecordKind = "random"
	KindUUID     RecordKind = "uuid"
)

// Record is one entry in the journal.
//
// In Phase 1 this lives in memory and dies with the process, which is the whole
// point of the naive baseline. From Phase 2 the same shape is persisted as an
// append-only event in Postgres; the Journal interface is the seam.
type Record struct {
	Seq  int        `json:"seq"`
	Kind RecordKind `json:"kind"`

	// Activity-only fields.
	Name  string `json:"name,omitempty"`
	Class Class  `json:"class,omitempty"`

	// InputHash is recorded but deliberately NOT part of the idempotency key
	// (ADR-003). It exists so that a replay whose input differs from the
	// original can be detected and reported as failure F6.3, rather than
	// silently minting a new key and producing a duplicate effect.
	InputHash string `json:"input_hash,omitempty"`

	IdempotencyKey string `json:"idempotency_key,omitempty"`

	Status AttemptState    `json:"status,omitempty"`
	Effect EffectState     `json:"effect,omitempty"`
	Output json.RawMessage `json:"output,omitempty"`
	Err    string          `json:"err,omitempty"`
}

// Journal is the append-only history of a workflow run.
//
// Append-only is invariant I1: a committed record is never modified. A later
// record for the same Seq supersedes an earlier one for lookup purposes — that
// is how PREPARED becomes CONFIRMED — but the earlier record remains in the
// history and can still be read.
type Journal interface {
	// Append adds a record. It never overwrites.
	Append(Record) error

	// Latest returns the most recent record for a sequence number.
	Latest(seq int) (Record, bool)

	// All returns every record in append order.
	All() []Record

	// Len returns the total number of records appended.
	Len() int
}

// MemoryJournal is the Phase 1 journal: fast, correct, and lost entirely when
// the process dies. Baseline A depends on exactly that property.
type MemoryJournal struct {
	mu      sync.RWMutex
	records []Record
}

func NewMemoryJournal() *MemoryJournal { return &MemoryJournal{} }

func (j *MemoryJournal) Append(r Record) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.records = append(j.records, r)
	return nil
}

func (j *MemoryJournal) Latest(seq int) (Record, bool) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	for i := len(j.records) - 1; i >= 0; i-- {
		if j.records[i].Seq == seq {
			return j.records[i], true
		}
	}
	return Record{}, false
}

func (j *MemoryJournal) All() []Record {
	j.mu.RLock()
	defer j.mu.RUnlock()
	out := make([]Record, len(j.records))
	copy(out, j.records)
	return out
}

func (j *MemoryJournal) Len() int {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return len(j.records)
}

// Effects returns the latest effect state for every activity that has one.
// Used by the invariant checker and, from Phase 6, by recovery.
func Effects(j Journal) map[int]EffectState {
	out := map[int]EffectState{}
	for _, r := range j.All() {
		if r.Effect != "" {
			out[r.Seq] = r.Effect
		}
	}
	return out
}
