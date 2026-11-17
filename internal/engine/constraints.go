package engine

import (
	"lamina/internal/catalog"
	"lamina/internal/datum"
	"lamina/internal/index"
	"lamina/internal/journal"
)

func (d *DB) unique(table string, writes map[string]map[string]staged) error {
	relation, err := d.catalog.Relation(table)
	if err != nil {
		return invalid(err)
	}
	view := relation.CurrentView()
	rows := d.store.Scan(table, d.sequence)
	overlay(rows, writes, table)
	for _, i := range d.catalog.Indexes {
		if i.Relation == table && i.Unique {
			if err := checkUnique(view, rows, i); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkUnique(view catalog.RelationView, rows map[string]datum.Row, i catalog.Index) error {
	values := map[string]string{}
	for key, raw := range rows {
		row := catalog.Project(view, raw)
		v := row[i.Column]
		if v.Null() {
			continue
		}
		token := v.Key()
		if prior, exists := values[token]; exists && prior != key {
			return fault("conflict", "unique constraint %s violated", i.Name)
		}
		values[token] = key
	}
	return nil
}

func (d *DB) CreateRelation(r catalog.Relation) (catalog.Relation, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.usable(); err != nil {
		return r, err
	}
	if _, exists := d.catalog.Relations[r.Name]; exists {
		return r, fault("conflict", "relation exists")
	}
	effective := d.sequence + 1
	built, err := catalog.NewRelation(r, effective)
	if err != nil {
		return r, invalid(err)
	}
	if err := d.publish(journal.Record{Kind: "relation", Relation: &built}); err != nil {
		return r, err
	}
	return built, nil
}

// EvolveRelation admits a controlled, compatible schema evolution. All
// validation happens before any durable record is appended, so a rejected
// evolution consumes no sequence and leaves the catalog untouched.
func (d *DB) EvolveRelation(req catalog.EvolutionRequest) (catalog.EvolutionResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.usable(); err != nil {
		return catalog.EvolutionResult{}, err
	}
	previous, err := d.catalog.Relation(req.Relation)
	if err != nil {
		return catalog.EvolutionResult{}, invalid(err)
	}
	previous.MigrateLegacy()
	effective := d.sequence + 1
	next, changes, err := catalog.Evolve(previous, req, effective)
	if err != nil {
		// Incompatible adjustments are rejected as conflicts against the live
		// schema; malformed request shapes still surface as invalid input via
		// the transport layer validation below.
		return catalog.EvolutionResult{}, fault("conflict", "%s", err)
	}
	// Dropping an indexed column would orphan the index and break candidate
	// resolution, so it is rejected up front.
	for _, change := range changes {
		if change.Op == "drop_column" {
			for _, definition := range d.catalog.Indexes {
				if definition.Relation == req.Relation && definition.Column == change.Column {
					return catalog.EvolutionResult{}, fault("conflict", "column %s is used by index %s", change.Column, definition.Name)
				}
			}
		}
	}
	ordinal := next.Revisions[len(next.Revisions)-1].Ordinal
	record := catalog.EvolutionRecord{Relation: req.Relation, Ordinal: ordinal, Effective: effective, Changes: changes, After: next}
	if err := d.publish(journal.Record{Kind: "evolution", Evolution: &record}); err != nil {
		return catalog.EvolutionResult{}, err
	}
	return catalog.EvolutionResult{
		Relation:  next.Name,
		Sequence:  d.sequence,
		Ordinal:   ordinal,
		Effective: effective,
		Changes:   changes,
		Columns:   next.Columns,
		Revisions: next.Revisions,
		Retired:   next.Retired,
	}, nil
}

func (d *DB) CreateIndex(i catalog.Index) (catalog.Index, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.usable(); err != nil {
		return i, err
	}
	if err := d.catalog.CheckIndex(i); err != nil {
		return i, invalid(err)
	}
	relation, err := d.catalog.Relation(i.Relation)
	if err != nil {
		return i, invalid(err)
	}
	if i.Unique {
		if err := checkUnique(relation.CurrentView(), d.store.Scan(i.Relation, d.sequence), i); err != nil {
			return i, err
		}
	}
	i.Created = d.sequence + 1
	if err := d.publish(journal.Record{Kind: "index", Index: &i}); err != nil {
		return i, err
	}
	d.indexes = index.Build(d.catalog, d.store)
	return i, nil
}
