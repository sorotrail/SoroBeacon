package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sorotrail/sorobeacon/internal/api/graphql"
	"github.com/sorotrail/sorobeacon/internal/store"
)

type registerOperationInput struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Document string `json:"document"`
}

func (s *Server) registerPersistedOperation(w http.ResponseWriter, r *http.Request) {
	var in registerOperationInput
	if !readJSON(w, r, &in) {
		return
	}

	var problems []FieldError
	if in.Document == "" {
		problems = append(problems, FieldError{Field: "document", Reason: "required"})
	}
	if len(problems) > 0 {
		writeValidation(w, r, problems)
		return
	}

	normDoc, hash, err := graphql.ValidateAndHashDocument(in.Document, in.ID)
	if err != nil {
		writeValidation(w, r, []FieldError{
			{Field: "document", Reason: err.Error()},
		})
		return
	}

	name := in.Name
	if name == "" {
		name = hash[:12]
	}

	op := store.PersistedOperation{
		ID:        hash,
		Name:      name,
		Document:  normDoc,
		CreatedAt: time.Now().UTC(),
	}

	if err := s.store.RegisterPersistedOperation(r.Context(), op); err != nil {
		s.log.Error("register_persisted_operation", "err", err)
		writeErr(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusCreated, op)
}

func (s *Server) listPersistedOperations(w http.ResponseWriter, r *http.Request) {
	ops, err := s.store.ListPersistedOperations(r.Context())
	if err != nil {
		s.log.Error("list_persisted_operations", "err", err)
		writeErr(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if ops == nil {
		ops = []store.PersistedOperation{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"operations": ops})
}

func (s *Server) getPersistedOperation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	op, err := s.store.GetPersistedOperation(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, r, http.StatusNotFound, "persisted operation not found")
			return
		}
		s.log.Error("get_persisted_operation", "err", err)
		writeErr(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, op)
}

func (s *Server) deletePersistedOperation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.store.DeletePersistedOperation(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, r, http.StatusNotFound, "persisted operation not found")
			return
		}
		s.log.Error("delete_persisted_operation", "err", err)
		writeErr(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
