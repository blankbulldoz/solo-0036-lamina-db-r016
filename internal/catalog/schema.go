package catalog

import (
	"fmt"
	"lamina/internal/datum"
	"reflect"
	"regexp"
	"sort"
)

var identifier = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

func ValidName(s string) bool { return identifier.MatchString(s) }

type Column struct {
	Name     string       `json:"name"`
	Type     string       `json:"type"`
	Nullable bool         `json:"nullable"`
	Default  *datum.Value `json:"default,omitempty"`
}

// Revision is one immutable schema generation of a relation. Every revision is
// made durable as its own journal record and becomes effective at Sequence.
type Revision struct {
	Ordinal     int      `json:"ordinal"`
	EffectiveAt uint64   `json:"effective_at"`
	Columns     []Column `json:"columns"`
}

type Relation struct {
	Name    string `json:"name"`
	Primary string `json:"primary"`
	// Columns mirrors the columns of the current revision so existing payloads
	// and readers that only know the current shape keep working unchanged.
	Columns   []Column   `json:"columns"`
	Created   uint64     `json:"created"`
	Revisions []Revision `json:"revisions"`
	// Retired names can never be reused: an old stored row would otherwise be
	// interpreted as a value of a brand-new column sharing the same name.
	Retired []string `json:"retired,omitempty"`
}

// RelationView is the schema generation that explains rows at one snapshot.
type RelationView struct {
	Name      string
	Primary   string
	Created   uint64
	Ordinal   int
	Effective uint64
	Columns   []Column
}

func (v RelationView) Column(name string) (Column, bool) {
	for _, c := range v.Columns {
		if c.Name == name {
			return c, true
		}
	}
	return Column{}, false
}

// PrimaryColumn returns the typed primary key column of this generation.
func (v RelationView) PrimaryColumn() Column {
	c, _ := v.Column(v.Primary)
	return c
}

func validType(t string) bool { return t == "int" || t == "text" || t == "bool" }

// Validate admits both the baseline single-shape payload and fully versioned
// relations recovered from journal or image.
func (r Relation) Validate() error {
	if !ValidName(r.Name) || !ValidName(r.Primary) {
		return fmt.Errorf("invalid relation or primary name")
	}
	if len(r.Revisions) == 0 {
		return r.validateLegacy()
	}
	reserved := map[string]bool{}
	for _, retired := range r.Retired {
		if !ValidName(retired) {
			return fmt.Errorf("invalid retired column %q", retired)
		}
		if reserved[retired] {
			return fmt.Errorf("duplicate retired column %q", retired)
		}
		reserved[retired] = true
	}
	last := uint64(0)
	for idx, rev := range r.Revisions {
		if idx == 0 {
			if rev.Ordinal != 0 {
				return fmt.Errorf("first schema revision must have ordinal 0")
			}
		} else if rev.Ordinal <= r.Revisions[idx-1].Ordinal {
			return fmt.Errorf("schema revision ordinals must be strictly increasing")
		}
		if rev.EffectiveAt == 0 || rev.EffectiveAt <= last {
			return fmt.Errorf("schema revision %d has invalid effective sequence", rev.Ordinal)
		}
		last = rev.EffectiveAt
		// Column shape is validated on its own merits. A retired name is
		// legitimately absent from later generations; the rule that it can
		// never be re-added as a live column is enforced during admission.
		if err := r.validateColumns(rev.Columns, nil); err != nil {
			return err
		}
	}
	// Every retired name must have once been a live column, otherwise the
	// retired list references an unknown generation.
	known := map[string]bool{}
	for _, rev := range r.Revisions {
		for _, c := range rev.Columns {
			known[c.Name] = true
		}
	}
	for name := range reserved {
		if !known[name] {
			return fmt.Errorf("retired column %q never existed", name)
		}
	}
	if r.Created != r.Revisions[0].EffectiveAt {
		return fmt.Errorf("relation creation sequence does not match first revision")
	}
	current := r.Revisions[len(r.Revisions)-1]
	if len(r.Columns) != len(current.Columns) {
		return fmt.Errorf("relation columns must mirror current revision")
	}
	if !reflect.DeepEqual(r.Columns, current.Columns) {
		return fmt.Errorf("relation columns must mirror current revision")
	}
	return nil
}

func (r Relation) validateLegacy() error {
	if len(r.Columns) == 0 || len(r.Columns) > 64 {
		return fmt.Errorf("relation needs 1..64 columns")
	}
	return r.validateColumns(r.Columns, map[string]bool{})
}

func (r Relation) validateColumns(columns []Column, reserved map[string]bool) error {
	if len(columns) == 0 || len(columns) > 64 {
		return fmt.Errorf("relation needs 1..64 columns")
	}
	seen := map[string]bool{}
	found := false
	for _, c := range columns {
		if !ValidName(c.Name) || seen[c.Name] {
			return fmt.Errorf("invalid or duplicate column %q", c.Name)
		}
		// A live column must never reuse a name that belonged to a
		// generation where it meant something different.
		if reserved[c.Name] {
			return fmt.Errorf("column name %q was retired and cannot be reused", c.Name)
		}
		seen[c.Name] = true
		if !validType(c.Type) {
			return fmt.Errorf("unsupported type %q", c.Type)
		}
		if c.Default != nil && !c.Default.Null() {
			if c.Default.Kind != c.Type {
				return fmt.Errorf("default for column %s requires %s", c.Name, c.Type)
			}
			if c.Type == "text" && len(c.Default.Text) > 65536 {
				return fmt.Errorf("default text exceeds 64KiB")
			}
		}
		if c.Name == r.Primary {
			found = true
			if c.Nullable || c.Type == "bool" {
				return fmt.Errorf("primary must be non-null int or text")
			}
			if c.Default != nil {
				return fmt.Errorf("primary column cannot carry a default")
			}
		}
	}
	if !found {
		return fmt.Errorf("primary column does not exist")
	}
	return nil
}

// ViewAt returns the revision effective at snapshot at.
func (r Relation) ViewAt(at uint64) (RelationView, bool) {
	if at < r.Created || len(r.Revisions) == 0 {
		return RelationView{}, false
	}
	chosen := 0
	for i, rev := range r.Revisions {
		if rev.EffectiveAt <= at {
			chosen = i
			continue
		}
		break
	}
	rev := r.Revisions[chosen]
	return RelationView{Name: r.Name, Primary: r.Primary, Created: r.Created, Ordinal: rev.Ordinal, Effective: rev.EffectiveAt, Columns: rev.Columns}, true
}

// CurrentView returns the newest schema generation.
func (r Relation) CurrentView() RelationView {
	view, _ := r.ViewAt(r.Revisions[len(r.Revisions)-1].EffectiveAt)
	return view
}

func (r Relation) Column(name string) (Column, bool) { return r.CurrentView().Column(name) }

// Normalize validates and fills a row using the given schema generation.
func Normalize(v RelationView, row datum.Row) (datum.Row, error) {
	for k := range row {
		if _, ok := v.Column(k); !ok {
			return nil, fmt.Errorf("unknown column %q", k)
		}
	}
	out := make(datum.Row, len(v.Columns))
	for _, c := range v.Columns {
		val, exists := row[c.Name]
		switch {
		case !exists:
			// Omitted column: use the typed default, then null for nullable
			// columns, otherwise the non-null constraint has no value to use.
			if c.Default != nil && !c.Default.Null() {
				val = *c.Default
			} else if c.Nullable {
				val = datum.Value{Kind: "null"}
			} else {
				return nil, fmt.Errorf("column %s cannot be null", c.Name)
			}
		case val.Null():
			// An explicit null honors NOT NULL even when a default exists; a
			// default only fills a column the writer left out.
			if !c.Nullable {
				return nil, fmt.Errorf("column %s cannot be null", c.Name)
			}
		}
		if !val.Null() && val.Kind != c.Type {
			return nil, fmt.Errorf("column %s requires %s", c.Name, c.Type)
		}
		if val.Kind == "text" && len(val.Text) > 65536 {
			return nil, fmt.Errorf("text exceeds 64KiB")
		}
		out[c.Name] = val
	}
	return out, nil
}

// Project rewrites a row stored under an older generation into the columns of
// the view being read: dropped columns disappear and columns introduced later
// receive the default that was in force for that generation.
func Project(v RelationView, row datum.Row) datum.Row {
	if row == nil {
		return nil
	}
	out := make(datum.Row, len(v.Columns))
	for _, c := range v.Columns {
		val, present := row[c.Name]
		if !present || val.Null() {
			if c.Default != nil && !c.Default.Null() {
				val = *c.Default
			} else {
				val = datum.Value{Kind: "null"}
			}
		}
		out[c.Name] = val
	}
	return out
}

// Key derives the canonical primary key from an already normalized row.
func (v RelationView) Key(row datum.Row) string { return row[v.Primary].Key() }

// Revision returns one durable schema generation by ordinal.
func (r Relation) Revision(ordinal int) (Revision, bool) {
	for _, rev := range r.Revisions {
		if rev.Ordinal == ordinal {
			return rev, true
		}
	}
	return Revision{}, false
}

// PruneRevisions drops generations that are older than horizon, unless a
// retained row version still references them. The first retained generation
// and every generation effective at or after horizon always survive.
// Ordinals stay monotonic and may become non-contiguous after pruning.
func (r *Relation) PruneRevisions(horizon uint64, referenced map[int]bool) int {
	if len(r.Revisions) <= 1 {
		return 0
	}
	kept := make([]Revision, 0, len(r.Revisions))
	removed := 0
	for i, rev := range r.Revisions {
		drop := i > 0 && rev.EffectiveAt < horizon && !referenced[rev.Ordinal]
		if drop {
			removed++
			continue
		}
		kept = append(kept, rev)
	}
	r.Revisions = kept
	return removed
}

// NextOrdinal returns the ordinal of the next schema generation; it remains
// monotonic even when earlier generations were vacuumed away.
func (r Relation) NextOrdinal() int {
	if len(r.Revisions) == 0 {
		return 0
	}
	return r.Revisions[len(r.Revisions)-1].Ordinal + 1
}

// MigrateLegacy synthesizes revision 0 for a relation stored by a pre-evolution
// build that had no revision chain. It is a no-op once revisions exist.
func (r *Relation) MigrateLegacy() {
	if len(r.Revisions) > 0 || r.Created == 0 {
		return
	}
	r.Columns = cloneColumns(r.Columns)
	r.Revisions = []Revision{{Ordinal: 0, EffectiveAt: r.Created, Columns: cloneColumns(r.Columns)}}
}

func cloneColumns(in []Column) []Column {
	out := make([]Column, len(in))
	for i, c := range in {
		out[i] = c
		if c.Default != nil {
			d := *c.Default
			out[i].Default = &d
		}
	}
	return out
}

func cloneRevisions(in []Revision) []Revision {
	out := make([]Revision, len(in))
	for i, rev := range in {
		out[i] = Revision{Ordinal: rev.Ordinal, EffectiveAt: rev.EffectiveAt, Columns: cloneColumns(rev.Columns)}
	}
	return out
}

// Clone returns a deep, safe-to-mutate copy of the relation.
func (r Relation) Clone() Relation {
	n := r
	n.Columns = cloneColumns(r.Columns)
	n.Revisions = cloneRevisions(r.Revisions)
	n.Retired = append([]string(nil), r.Retired...)
	return n
}

// sortedRetired keeps durable output stable.
func sortedRetired(in map[string]bool) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for name := range in {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
