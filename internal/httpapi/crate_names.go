package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"reflect"

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

// crateNamer names the crate references of the values of one call. Its
// reachability memo belongs to the call and holds only finished results, so
// no other request can read a partial result.
type crateNamer struct {
	reaches map[reflect.Type]bool
	names   map[string]string
}

// mayHold reports whether a value of type t can reach a crate reference
// through fields that JSON encodes, pointers, slices, arrays, maps, or
// interfaces. Each result comes from one complete search from t.
func (n *crateNamer) mayHold(t reflect.Type) bool {
	if reaches, ok := n.reaches[t]; ok {
		return reaches
	}
	reaches := typeReachesTaskCrate(t, map[reflect.Type]bool{})
	n.reaches[t] = reaches
	return reaches
}

// typeReachesTaskCrate searches the type graph from t. The visited set is
// local to the search, so a type seen twice on one search adds nothing new.
func typeReachesTaskCrate(t reflect.Type, visited map[reflect.Type]bool) bool {
	if t == taskCratePointerType {
		return true
	}
	if visited[t] {
		return false
	}
	visited[t] = true
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
		return typeReachesTaskCrate(t.Elem(), visited)
	case reflect.Interface:
		return true
	case reflect.Struct:
		for i := range t.NumField() {
			if field := t.Field(i); jsonEncodesField(field) && typeReachesTaskCrate(field.Type, visited) {
				return true
			}
		}
	}
	return false
}

func jsonEncodesField(field reflect.StructField) bool {
	return (field.IsExported() || field.Anonymous) && field.Tag.Get("json") != "-"
}

// collect adds the crate ID of every reference v reaches. It reads only.
func (n *crateNamer) collect(v reflect.Value, ids map[string]bool) {
	if !v.IsValid() || !n.mayHold(v.Type()) {
		return
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return
		}
		if v.Type() == taskCratePointerType {
			ids[v.Interface().(*core.TaskCrate).ID] = true
			return
		}
		n.collect(v.Elem(), ids)
	case reflect.Interface:
		if !v.IsNil() {
			n.collect(v.Elem(), ids)
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			n.collect(v.Index(i), ids)
		}
	case reflect.Map:
		for _, key := range v.MapKeys() {
			n.collect(v.MapIndex(key), ids)
		}
	case reflect.Struct:
		t := v.Type()
		for i := range v.NumField() {
			if jsonEncodesField(t.Field(i)) {
				n.collect(v.Field(i), ids)
			}
		}
	}
}

// name replaces every crate reference v reaches with one that carries the
// crate's current name. A value that a map or an interface holds is not
// addressable, so name copies it, names the copy, and stores the copy back.
// A reference name cannot replace is an error, never an empty name.
func (n *crateNamer) name(v reflect.Value) error {
	if !v.IsValid() || !n.mayHold(v.Type()) {
		return nil
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return nil
		}
		if v.Type() == taskCratePointerType {
			if !v.CanSet() {
				return fmt.Errorf("name task crates: a crate reference cannot be set")
			}
			id := v.Interface().(*core.TaskCrate).ID
			v.Set(reflect.ValueOf(&core.TaskCrate{ID: id, Name: n.names[id]}))
			return nil
		}
		return n.name(v.Elem())
	case reflect.Interface:
		if v.IsNil() {
			return nil
		}
		held := v.Elem()
		if held.Kind() == reflect.Pointer && held.Type() != taskCratePointerType {
			return n.name(held)
		}
		copied := reflect.New(held.Type()).Elem()
		copied.Set(held)
		if err := n.name(copied); err != nil {
			return err
		}
		if !v.CanSet() {
			return fmt.Errorf("name task crates: a %s held in an interface cannot be stored back", held.Type())
		}
		v.Set(copied)
	case reflect.Slice:
		for i := range v.Len() {
			if err := n.name(v.Index(i)); err != nil {
				return err
			}
		}
	case reflect.Map:
		for _, key := range v.MapKeys() {
			copied := reflect.New(v.Type().Elem()).Elem()
			copied.Set(v.MapIndex(key))
			if err := n.name(copied); err != nil {
				return err
			}
			v.SetMapIndex(key, copied)
		}
	case reflect.Array, reflect.Struct:
		if !v.CanAddr() {
			return fmt.Errorf("name task crates: a %s is not addressable", v.Type())
		}
		if v.Kind() == reflect.Array {
			for i := range v.Len() {
				if err := n.name(v.Index(i)); err != nil {
					return err
				}
			}
			return nil
		}
		t := v.Type()
		for i := range v.NumField() {
			if jsonEncodesField(t.Field(i)) {
				if err := n.name(v.Field(i)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// nameTaskCrates gives every crate reference that the targets reach the
// crate's current name in one batched read. Each target is a non-nil
// pointer to a value the handler owns. A reference is replaced, never
// changed in place, so a crate record another reader shares keeps its own
// reference.
func (s *Server) nameTaskCrates(ctx context.Context, targets ...any) error {
	backend, ok := s.Store.(store.CrateStore)
	if !ok {
		return nil
	}
	namer := &crateNamer{reaches: map[reflect.Type]bool{}}
	values := make([]reflect.Value, 0, len(targets))
	for _, target := range targets {
		value := reflect.ValueOf(target)
		if value.Kind() != reflect.Pointer || value.IsNil() {
			return fmt.Errorf("name task crates: target %T is not a non-nil pointer", target)
		}
		values = append(values, value.Elem())
	}
	seen := map[string]bool{}
	for _, value := range values {
		namer.collect(value, seen)
	}
	if len(seen) == 0 {
		return nil
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	names, err := backend.CrateNames(ctx, ids)
	if err != nil {
		return err
	}
	// A crate is never deleted and a task joins only a crate of its own
	// workspace, so a missing name is a fault, never an empty name.
	for _, id := range ids {
		if _, found := names[id]; !found {
			return fmt.Errorf("name task crates: crate %s has no name in this workspace", id)
		}
	}
	namer.names = names
	for _, value := range values {
		if err := namer.name(value); err != nil {
			return err
		}
	}
	return nil
}

// writeTaskJSON writes a value that holds a task or a task row after it
// names every crate the value reaches. A failed name read or a reference
// that cannot be named fails the response (DEC-105(1)).
func (s *Server) writeTaskJSON(w http.ResponseWriter, r *http.Request, code int, v any) {
	target := reflect.New(reflect.TypeOf(v))
	target.Elem().Set(reflect.ValueOf(v))
	if err := s.nameTaskCrates(r.Context(), target.Interface()); err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	writeJSON(w, code, target.Elem().Interface())
}
