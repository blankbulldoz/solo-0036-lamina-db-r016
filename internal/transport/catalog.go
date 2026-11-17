package transport

import (
	"lamina/internal/catalog"
	"lamina/internal/engine"
	"net/http"
)

func (s *Server) catalog(w http.ResponseWriter, r *http.Request) {
	relations, indexes, err := s.db.Relations()
	if err != nil {
		respondError(w, err)
		return
	}
	write(w, 200, map[string]any{"relations": relations, "indexes": indexes})
}
func (s *Server) relationDetail(w http.ResponseWriter, r *http.Request) {
	out, err := s.db.RelationDetail(r.PathValue("name"))
	if err != nil {
		respondError(w, err)
		return
	}
	write(w, 200, out)
}
func (s *Server) createRelation(w http.ResponseWriter, r *http.Request) {
	var in catalog.Relation
	if !decode(w, r, &in) {
		return
	}
	out, err := s.db.CreateRelation(in)
	if err != nil {
		respondError(w, err)
		return
	}
	write(w, 201, out)
}

// evolveRelation accepts the relation both from the URL path and, optionally,
// the JSON body. A body value must agree with the path.
func (s *Server) evolveRelation(w http.ResponseWriter, r *http.Request) {
	var in catalog.EvolutionRequest
	if !decode(w, r, &in) {
		return
	}
	pathName := r.PathValue("name")
	if in.Relation == "" {
		in.Relation = pathName
	}
	if in.Relation != pathName {
		respondError(w, &engine.Error{Code: "invalid", Message: "relation in body must match URL"})
		return
	}
	if len(in.Changes) == 0 {
		respondError(w, &engine.Error{Code: "invalid", Message: "at least one column change is required"})
		return
	}
	for _, change := range in.Changes {
		switch change.Op {
		case "add_column", "alter_column", "drop_column":
		default:
			respondError(w, &engine.Error{Code: "invalid", Message: "unsupported evolution op"})
			return
		}
	}
	out, err := s.db.EvolveRelation(in)
	if err != nil {
		respondError(w, err)
		return
	}
	write(w, 200, out)
}
func (s *Server) createIndex(w http.ResponseWriter, r *http.Request) {
	var in catalog.Index
	if !decode(w, r, &in) {
		return
	}
	out, err := s.db.CreateIndex(in)
	if err != nil {
		respondError(w, err)
		return
	}
	write(w, 201, out)
}
