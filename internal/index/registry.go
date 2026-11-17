package index

import (
	"lamina/internal/catalog"
	"lamina/internal/versions"
)

type Registry struct{ Trees map[string]*Tree }

func Build(c *catalog.Catalog, s *versions.Store) *Registry {
	r := &Registry{Trees: map[string]*Tree{}}
	for name, definition := range c.Indexes {
		tree := New()
		relation, ok := c.Relations[definition.Relation]
		if !ok {
			continue
		}
		view := relation.CurrentView()
		for key, chain := range s.Tables[definition.Relation] {
			for _, v := range chain {
				if v.Deleted {
					continue
				}
				row := catalog.Project(view, v.Row)
				tree.Add(row[definition.Column], key)
			}
		}
		r.Trees[name] = tree
	}
	return r
}

func (r *Registry) Apply(c *catalog.Catalog, writes []versions.Mutation) {
	for _, m := range writes {
		if m.Deleted {
			continue
		}
		relation, ok := c.Relations[m.Relation]
		if !ok {
			continue
		}
		view := relation.CurrentView()
		row := catalog.Project(view, m.Row)
		for name, i := range c.Indexes {
			if i.Relation == m.Relation {
				r.Trees[name].Add(row[i.Column], m.Key)
			}
		}
	}
}
