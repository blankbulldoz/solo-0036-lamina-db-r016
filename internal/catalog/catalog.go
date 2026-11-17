package catalog

import (
	"fmt"
	"sort"
)

type Index struct {
	Name     string `json:"name"`
	Relation string `json:"relation"`
	Column   string `json:"column"`
	Unique   bool   `json:"unique"`
	Created  uint64 `json:"created"`
}
type Catalog struct {
	Relations map[string]Relation `json:"relations"`
	Indexes   map[string]Index    `json:"indexes"`
}

func New() *Catalog { return &Catalog{Relations: map[string]Relation{}, Indexes: map[string]Index{}} }
func (c *Catalog) Relation(name string) (Relation, error) {
	r, ok := c.Relations[name]
	if !ok {
		return r, fmt.Errorf("unknown relation %s", name)
	}
	return r, nil
}
func (c *Catalog) CheckIndex(i Index) error {
	if !ValidName(i.Name) {
		return fmt.Errorf("invalid index name")
	}
	if _, exists := c.Indexes[i.Name]; exists {
		return fmt.Errorf("index already exists")
	}
	r, err := c.Relation(i.Relation)
	if err != nil {
		return err
	}
	if _, ok := r.Column(i.Column); !ok {
		return fmt.Errorf("unknown index column")
	}
	return nil
}
func (c *Catalog) List() []Relation {
	out := make([]Relation, 0, len(c.Relations))
	for _, r := range c.Relations {
		out = append(out, r.Clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
func (c *Catalog) IndexList() []Index {
	out := make([]Index, 0, len(c.Indexes))
	for _, v := range c.Indexes {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
func (c *Catalog) Clone() *Catalog {
	n := New()
	for name, r := range c.Relations {
		n.Relations[name] = r.Clone()
	}
	for k, v := range c.Indexes {
		n.Indexes[k] = v
	}
	return n
}

// MigrateLegacy upgrades every pre-evolution relation (no revision chain) to
// carry its synthesized revision 0. It is idempotent.
func (c *Catalog) MigrateLegacy() {
	for name, r := range c.Relations {
		r.MigrateLegacy()
		c.Relations[name] = r
	}
}
