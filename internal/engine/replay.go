package engine

import (
	"fmt"
	"lamina/internal/catalog"
	"lamina/internal/datum"
	"lamina/internal/journal"
)

func (d *DB) apply(r journal.Record) error {
	if r.Sequence != d.sequence+1 || r.Format != 1 {
		return fmt.Errorf("invalid record sequence")
	}
	switch r.Kind {
	case "relation":
		if r.Relation == nil || r.Relation.Created != r.Sequence {
			return fmt.Errorf("invalid relation record")
		}
		r.Relation.MigrateLegacy()
		if err := r.Relation.Validate(); err != nil {
			return err
		}
		if _, ok := d.catalog.Relations[r.Relation.Name]; ok {
			return fmt.Errorf("duplicate relation")
		}
		d.catalog.Relations[r.Relation.Name] = *r.Relation
		d.store.Ensure(r.Relation.Name)
	case "evolution":
		if err := d.applyEvolution(r); err != nil {
			return err
		}
	case "index":
		if r.Index == nil || r.Index.Created != r.Sequence {
			return fmt.Errorf("invalid index record")
		}
		if err := d.catalog.CheckIndex(*r.Index); err != nil {
			return err
		}
		d.catalog.Indexes[r.Index.Name] = *r.Index
	case "commit":
		if err := d.applyCommit(r); err != nil {
			return err
		}
	case "vacuum":
		if r.Horizon < d.horizon || r.Horizon > d.sequence {
			return fmt.Errorf("invalid vacuum horizon")
		}
		d.applyVacuum(r.Horizon)
		d.horizon = r.Horizon
	default:
		return fmt.Errorf("unknown record kind %s", r.Kind)
	}
	d.sequence = r.Sequence
	return nil
}

func (d *DB) applyEvolution(r journal.Record) error {
	e := r.Evolution
	if e == nil || e.Relation == "" || e.Effective != r.Sequence {
		return fmt.Errorf("invalid evolution record")
	}
	previous, ok := d.catalog.Relations[e.Relation]
	if !ok {
		return fmt.Errorf("evolution for unknown relation %s", e.Relation)
	}
	after := e.After
	if err := after.Validate(); err != nil {
		return err
	}
	if after.Name != e.Relation || len(after.Revisions) != len(previous.Revisions)+1 {
		return fmt.Errorf("evolution payload does not extend relation %s", e.Relation)
	}
	last := after.Revisions[len(after.Revisions)-1]
	if last.Ordinal != e.Ordinal || last.EffectiveAt != r.Sequence {
		return fmt.Errorf("evolution revision boundary mismatch")
	}
	if len(e.Changes) == 0 {
		return fmt.Errorf("evolution without changes")
	}
	d.catalog.Relations[e.Relation] = after
	return nil
}

func (d *DB) applyCommit(r journal.Record) error {
	seen := map[string]bool{}
	for _, m := range r.Writes {
		relation, err := d.catalog.Relation(m.Relation)
		if err != nil {
			return err
		}
		token := m.Relation + "\x00" + m.Key
		if seen[token] {
			return fmt.Errorf("duplicate mutation")
		}
		seen[token] = true
		if m.Deleted {
			continue
		}
		// Re-validate every row under the exact schema generation it was
		// admitted with; stored bytes keep their original interpretation.
		view, ok := revisionView(relation, m.SchemaOrdinal, r.Sequence)
		if !ok {
			return fmt.Errorf("row references missing schema revision %d", m.SchemaOrdinal)
		}
		row, err := catalog.Normalize(view, m.Row)
		if err != nil {
			return err
		}
		if view.Key(row) != m.Key {
			return fmt.Errorf("stored primary mismatch")
		}
	}
	d.store.Apply(r.Sequence, r.Writes)
	return nil
}

// revisionView resolves the schema generation that explains a stored row.
// Ordinal 0 on a baseline row is resolved from the row's own sequence.
func revisionView(relation catalog.Relation, ordinal int, sequence uint64) (catalog.RelationView, bool) {
	if ordinal == 0 {
		if view, ok := relation.ViewAt(sequence); ok {
			return view, true
		}
	}
	rev, ok := relation.Revision(ordinal)
	if !ok {
		return catalog.RelationView{}, false
	}
	return relation.ViewAt(rev.EffectiveAt)
}

// applyVacuum prunes row versions and every schema generation no longer
// needed to explain retained rows or retained snapshots.
func (d *DB) applyVacuum(horizon uint64) {
	d.store.Vacuum(horizon)
	refs := d.store.ReferencedSchemaOrdinals()
	for name, relation := range d.catalog.Relations {
		relation.PruneRevisions(horizon, refs[name])
		d.catalog.Relations[name] = relation
	}
}

func overlay(rows map[string]datum.Row, writes map[string]map[string]staged, table string) {
	for key, m := range writes[table] {
		if m.Mutation.Deleted {
			delete(rows, key)
		} else {
			rows[key] = datum.Clone(m.Mutation.Row)
		}
	}
}
