package catalog

import (
	"encoding/json"
	"fmt"
	"lamina/internal/datum"
)

// ColumnChange is one requested schema evolution operation on one column.
type ColumnChange struct {
	// Op is add_column, alter_column or drop_column.
	Op string `json:"op"`
	// Name is the target column. For add_column it is the new column.
	Name string `json:"name"`
	Type string `json:"type,omitempty"`
	// NullablePtr distinguishes an omitted nullable flag from an explicit one.
	NullablePtr *bool        `json:"nullable,omitempty"`
	Default     *datum.Value `json:"default,omitempty"`
	// DefaultPresent marks whether the request carried a "default" key at all,
	// so that null can explicitly clear an existing default.
	DefaultPresent bool `json:"-"`
}

// UnmarshalJSON keeps DefaultPresent observable while reusing the typed value
// decoding of datum.Value (including its signed 64-bit integer enforcement).
func (c *ColumnChange) UnmarshalJSON(data []byte) error {
	type alias ColumnChange
	var aux struct {
		alias
		Nullable *bool            `json:"nullable"`
		Default  *json.RawMessage `json:"default"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*c = ColumnChange(aux.alias)
	c.NullablePtr = aux.Nullable
	if aux.Default != nil {
		c.DefaultPresent = true
		var v datum.Value
		if err := json.Unmarshal(*aux.Default, &v); err != nil {
			return err
		}
		c.Default = &v
	}
	return nil
}

type EvolutionRequest struct {
	Relation string         `json:"relation"`
	Changes  []ColumnChange `json:"changes"`
}

// ChangeRecord is the durable, user-visible description of one applied change.
type ChangeRecord struct {
	Op     string  `json:"op"`
	Column string  `json:"column"`
	Type   string  `json:"type,omitempty"`
	From   *Column `json:"from,omitempty"`
	To     *Column `json:"to,omitempty"`
}

// EvolutionRecord is the durable journal payload of one schema evolution.
type EvolutionRecord struct {
	Relation  string         `json:"relation"`
	Ordinal   int            `json:"ordinal"`
	Effective uint64         `json:"effective_at"`
	Changes   []ChangeRecord `json:"changes"`
	// After carries the complete relation state following the evolution so the
	// record is self-contained for replay and change consumers.
	After Relation `json:"after"`
}

// EvolutionResult is the response to a successful schema evolution.
type EvolutionResult struct {
	Relation  string         `json:"relation"`
	Sequence  uint64         `json:"sequence"`
	Ordinal   int            `json:"ordinal"`
	Effective uint64         `json:"effective_at"`
	Changes   []ChangeRecord `json:"changes"`
	Columns   []Column       `json:"columns"`
	Revisions []Revision     `json:"revisions"`
	Retired   []string       `json:"retired,omitempty"`
}

// Evolve applies the requested changes against the relation as it stands at
// effectiveAt and returns the next generation plus a durable change record.
// Incompatible requests return an error without mutating the relation.
func Evolve(previous Relation, req EvolutionRequest, effectiveAt uint64) (Relation, []ChangeRecord, error) {
	if len(req.Changes) == 0 || len(req.Changes) > 64 {
		return previous, nil, fmt.Errorf("evolution needs 1..64 column changes")
	}
	next := previous.Clone()
	view := next.CurrentView()
	current := map[string]Column{}
	for _, c := range view.Columns {
		current[c.Name] = c
	}
	retired := map[string]bool{}
	for _, name := range next.Retired {
		retired[name] = true
	}
	// Pending names cover adds within the same request so later changes in the
	// same batch can address them.
	pending := map[string]Column{}
	added := map[string]bool{}
	droppedInBatch := map[string]bool{}
	records := make([]ChangeRecord, 0, len(req.Changes))
	for _, change := range req.Changes {
		if !ValidName(change.Name) {
			return previous, nil, fmt.Errorf("invalid column name %q", change.Name)
		}
		before, existed := current[change.Name]
		if !existed {
			if p, ok := pending[change.Name]; ok {
				before, existed = p, true
			}
		}
		switch change.Op {
		case "add_column":
			if existed || retired[change.Name] {
				return previous, nil, fmt.Errorf("column %q already exists or was retired", change.Name)
			}
			// Final width = current columns minus drops plus net new adds.
			if len(view.Columns)-len(droppedInBatch)+len(added)+1 > 64 {
				return previous, nil, fmt.Errorf("relation column limit 64 reached")
			}
			col, err := admitAdd(next.Primary, change)
			if err != nil {
				return previous, nil, err
			}
			pending[col.Name] = col
			added[col.Name] = true
			records = append(records, ChangeRecord{Op: "add_column", Column: col.Name, Type: col.Type, To: ptrColumn(col)})
		case "alter_column":
			if !existed {
				return previous, nil, fmt.Errorf("cannot alter unknown column %q", change.Name)
			}
			col, err := admitAlter(next.Primary, before, change)
			if err != nil {
				return previous, nil, err
			}
			pending[col.Name] = col
			records = append(records, ChangeRecord{Op: "alter_column", Column: col.Name, From: ptrColumn(before), To: ptrColumn(col)})
		case "drop_column":
			if !existed {
				return previous, nil, fmt.Errorf("cannot drop unknown column %q", change.Name)
			}
			if change.Name == next.Primary {
				return previous, nil, fmt.Errorf("primary column cannot be dropped")
			}
			// Only a column that existed in a durable generation can be
			// retired; one added earlier in this batch simply disappears.
			if _, preExisting := current[change.Name]; preExisting {
				retired[change.Name] = true
			}
			droppedInBatch[change.Name] = true
			delete(pending, change.Name)
			delete(added, change.Name)
			records = append(records, ChangeRecord{Op: "drop_column", Column: change.Name, From: ptrColumn(before)})
		default:
			return previous, nil, fmt.Errorf("unsupported evolution op %q", change.Op)
		}
	}
	// Compose the new column list: existing columns survive unless dropped,
	// pending alters replace them and pending adds append at the end.
	dropped := map[string]bool{}
	for _, change := range req.Changes {
		if change.Op == "drop_column" {
			dropped[change.Name] = true
		}
	}
	columns := make([]Column, 0, len(view.Columns)+len(req.Changes))
	for _, c := range view.Columns {
		if replacement, ok := pending[c.Name]; ok {
			columns = append(columns, replacement)
			continue
		}
		if dropped[c.Name] {
			continue
		}
		columns = append(columns, c)
	}
	existing := map[string]bool{}
	for _, c := range columns {
		existing[c.Name] = true
	}
	// Preserve request order for freshly added columns. A column added and
	// dropped within the same batch no longer exists and must not be appended.
	for _, change := range req.Changes {
		if change.Op != "add_column" {
			continue
		}
		col, stillPending := pending[change.Name]
		if !stillPending || existing[col.Name] {
			continue
		}
		columns = append(columns, col)
		existing[col.Name] = true
	}
	temp := Relation{Name: next.Name, Primary: next.Primary, Columns: columns}
	if err := temp.validateColumns(columns, retired); err != nil {
		return previous, nil, err
	}
	next.Columns = cloneColumns(columns)
	next.Revisions = append(next.Revisions, Revision{Ordinal: next.NextOrdinal(), EffectiveAt: effectiveAt, Columns: cloneColumns(columns)})
	next.Retired = sortedRetired(retired)
	return next, records, nil
}
func admitAdd(primary string, change ColumnChange) (Column, error) {
	if !validType(change.Type) {
		return Column{}, fmt.Errorf("add_column %q requires a supported type", change.Name)
	}
	nullable := true
	if change.NullablePtr != nil {
		nullable = *change.NullablePtr
	}
	if change.Name == primary {
		return Column{}, fmt.Errorf("primary column cannot be added")
	}
	col := Column{Name: change.Name, Type: change.Type, Nullable: nullable, Default: change.Default}
	if !nullable {
		if change.Default == nil || change.Default.Null() {
			return Column{}, fmt.Errorf("non-null column %q requires a typed default so existing rows stay valid", change.Name)
		}
	}
	if change.Default != nil && !change.Default.Null() && change.Default.Kind != change.Type {
		return Column{}, fmt.Errorf("default for column %s requires %s", change.Name, change.Type)
	}
	return col, nil
}

func admitAlter(primary string, before Column, change ColumnChange) (Column, error) {
	if before.Name == primary && change.DefaultPresent {
		return Column{}, fmt.Errorf("primary column %s cannot carry a default", before.Name)
	}
	col := before
	if change.Type != "" && change.Type != before.Type {
		return Column{}, fmt.Errorf("incompatible evolution: column %q cannot change type %s -> %s", before.Name, before.Type, change.Type)
	}
	if change.NullablePtr != nil && *change.NullablePtr != before.Nullable {
		direction := "tighten nullability"
		if *change.NullablePtr {
			direction = "relax nullability"
		}
		return Column{}, fmt.Errorf("incompatible evolution: column %q cannot %s", before.Name, direction)
	}
	if change.DefaultPresent {
		if change.Default == nil || change.Default.Null() {
			col.Default = nil
		} else {
			if change.Default.Kind != before.Type {
				return Column{}, fmt.Errorf("default for column %s requires %s", before.Name, before.Type)
			}
			d := *change.Default
			col.Default = &d
		}
	}
	return col, nil
}

func ptrColumn(c Column) *Column {
	out := c
	if c.Default != nil {
		d := *c.Default
		out.Default = &d
	}
	return &out
}

// NewRelation builds the first schema generation for a create-relation request.
func NewRelation(r Relation, effectiveAt uint64) (Relation, error) {
	r.Created = effectiveAt
	r.Columns = cloneColumns(r.Columns)
	r.Revisions = []Revision{{Ordinal: 0, EffectiveAt: effectiveAt, Columns: cloneColumns(r.Columns)}}
	r.Retired = nil
	if err := r.Validate(); err != nil {
		return Relation{}, err
	}
	return r, nil
}
