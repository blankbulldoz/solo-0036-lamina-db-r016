package engine

import "sort"

type SchemaStat struct {
	Relation        string `json:"relation"`
	RevisionOrdinal int    `json:"revision_ordinal"`
	Revisions       int    `json:"revisions_retained"`
	RetiredColumns  int    `json:"retired_columns"`
	// OldestRevision is the effective sequence of the oldest retained schema
	// generation. Together with the top-level horizon it tells callers which
	// historical snapshots can still be interpreted.
	OldestRevision uint64 `json:"oldest_revision_at"`
}

type Stats struct {
	Sequence        uint64         `json:"sequence"`
	Horizon         uint64         `json:"horizon"`
	Active          []Transaction  `json:"active"`
	Keys            int            `json:"keys"`
	Versions        int            `json:"versions"`
	IndexEntries    map[string]int `json:"index_entries"`
	JournalRecords  int            `json:"journal_records"`
	HistoryBase     uint64         `json:"history_base"`
	RecoveredTail   int64          `json:"recovered_tail_bytes"`
	Commits         uint64         `json:"commits_since_open"`
	Conflicts       uint64         `json:"conflicts_since_open"`
	Queries         uint64         `json:"queries_since_open"`
	SchemaRevisions int            `json:"schema_revisions"`
	Schemas         []SchemaStat   `json:"schemas"`
}

func (d *DB) Stats() (Stats, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if err := d.usable(); err != nil {
		return Stats{}, err
	}
	keys, versions := d.store.Count()
	s := Stats{Sequence: d.sequence, Horizon: d.horizon, Keys: keys, Versions: versions, Active: []Transaction{}, IndexEntries: map[string]int{}, JournalRecords: len(d.wal.Records), HistoryBase: d.wal.Base, RecoveredTail: d.wal.RecoveredTail, Commits: d.commits, Conflicts: d.conflicts, Queries: d.queries, Schemas: []SchemaStat{}}
	for _, tx := range d.transactions {
		copy := *tx
		copy.writes = nil
		s.Active = append(s.Active, copy)
	}
	sort.Slice(s.Active, func(i, j int) bool { return s.Active[i].ID < s.Active[j].ID })
	for name, t := range d.indexes.Trees {
		s.IndexEntries[name] = t.Size()
	}
	for _, relation := range d.catalog.List() {
		stat := SchemaStat{Relation: relation.Name, Revisions: len(relation.Revisions), RetiredColumns: len(relation.Retired)}
		if len(relation.Revisions) > 0 {
			current := relation.Revisions[len(relation.Revisions)-1]
			oldest := relation.Revisions[0]
			stat.RevisionOrdinal = current.Ordinal
			stat.OldestRevision = oldest.EffectiveAt
		}
		s.SchemaRevisions += stat.Revisions
		s.Schemas = append(s.Schemas, stat)
	}
	sort.Slice(s.Schemas, func(i, j int) bool { return s.Schemas[i].Relation < s.Schemas[j].Relation })
	return s, nil
}
