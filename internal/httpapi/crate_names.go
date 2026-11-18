package httpapi

import (
	"context"
	"net/http"
	"reflect"
	"sync"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Stores carry only a task's crate ID. A response that writes a task or a
// task row names the crate where the response is written, so every task row
// carries crate as {id, name} with the current name, and a task in no crate
// carries none (component-crates, Public surfaces). No dispatch, claim, gate,
// review, merge, next-action, or lineage code reads the name
// (component-crates invariant 1). TestCrateTaskResponsesNameEveryCrate holds
// every JSON write of a task-bearing value in this package to this rule.

var taskCratePointerType = reflect.TypeFor[*core.TaskCrate]()

// taskCrateHolders caches, per type, whether a value of the type can reach a
// crate reference through exported fields, pointers, slices, arrays, maps,
// or interfaces, so naming skips every other part of a response.
var taskCrateHolders sync.Map

func mayHoldTaskCrate(t reflect.Type) bool {
	if cached, ok := taskCrateHolders.Load(t); ok {
		return cached.(bool)
	}
	// A recursive type reaches itself; assume false while it is in progress.
	taskCrateHolders.Store(t, false)
	holds := false
	switch t.Kind() {
	case reflect.Pointer:
		holds = t == taskCratePointerType || mayHoldTaskCrate(t.Elem())
	case reflect.Slice, reflect.Array, reflect.Map:
		holds = mayHoldTaskCrate(t.Elem())
	case reflect.Interface:
		holds = true
	case reflect.Struct:
		for i := range t.NumField() {
			field := t.Field(i)
			if (field.IsExported() || field.Anonymous) && field.Tag.Get("json") != "-" && mayHoldTaskCrate(field.Type) {
				holds = true
				break
			}
		}
	}
	taskCrateHolders.Store(t, holds)
	return holds
}

// collectTaskCrates appends every settable, non-nil crate reference that v
// reaches through fields that JSON encodes. A crate reference held directly
// as a map value is not settable, and no response type holds one that way.
func collectTaskCrates(v reflect.Value, slots *[]reflect.Value) {
	if !v.IsValid() || !mayHoldTaskCrate(v.Type()) {
		return
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return
		}
		if v.Type() == taskCratePointerType {
			if v.CanSet() {
				*slots = append(*slots, v)
			}
			return
		}
		collectTaskCrates(v.Elem(), slots)
	case reflect.Interface:
		if !v.IsNil() {
			collectTaskCrates(v.Elem(), slots)
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			collectTaskCrates(v.Index(i), slots)
		}
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			collectTaskCrates(iter.Value(), slots)
		}
	case reflect.Struct:
		t := v.Type()
		for i := range v.NumField() {
			if field := t.Field(i); (field.IsExported() || field.Anonymous) && field.Tag.Get("json") != "-" {
				collectTaskCrates(v.Field(i), slots)
			}
		}
	}
}

// nameTaskCrates gives every crate reference that the targets reach the
// crate's current name in one batched read. Each target is a non-nil pointer
// to a value the handler owns. A reference is replaced, never changed in
// place, so a value another reader shares keeps its own reference.
func (s *Server) nameTaskCrates(ctx context.Context, targets ...any) error {
	backend, ok := s.Store.(store.CrateStore)
	if !ok {
		return nil
	}
	var slots []reflect.Value
	for _, target := range targets {
		collectTaskCrates(reflect.ValueOf(target), &slots)
	}
	if len(slots) == 0 {
		return nil
	}
	seen := map[string]bool{}
	ids := make([]string, 0, len(slots))
	for _, slot := range slots {
		if id := slot.Interface().(*core.TaskCrate).ID; !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	names, err := backend.CrateNames(ctx, ids)
	if err != nil {
		return err
	}
	for _, slot := range slots {
		id := slot.Interface().(*core.TaskCrate).ID
		slot.Set(reflect.ValueOf(&core.TaskCrate{ID: id, Name: names[id]}))
	}
	return nil
}

// writeTaskJSON writes a value that holds a task or a task row after it
// names every crate the value reaches. A failed name read fails the response
// (DEC-105(1)).
func (s *Server) writeTaskJSON(w http.ResponseWriter, r *http.Request, code int, v any) {
	target := reflect.New(reflect.TypeOf(v))
	target.Elem().Set(reflect.ValueOf(v))
	if err := s.nameTaskCrates(r.Context(), target.Interface()); err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	writeJSON(w, code, target.Elem().Interface())
}
