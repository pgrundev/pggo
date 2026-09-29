package pggo

import (
	"fmt"
	"reflect"
	"strings"
)

// CollectStructs reads every row into a T, matching columns to struct fields
// by `db:"name"` tag, or by field name (case-insensitive) when untagged.
// Fields tagged `db:"-"` are ignored. Struct fields with no matching column
// keep their zero value; a column with no matching field is an error.
// Rows is closed when CollectStructs returns.
func CollectStructs[T any](rows *Rows) ([]T, error) {
	defer rows.Close()
	out := []T{}
	var plan [][]int
	for rows.Next() {
		if plan == nil {
			var err error
			if plan, err = structPlan(reflect.TypeOf((*T)(nil)).Elem(), rows.Columns()); err != nil {
				return nil, err
			}
		}
		var v T
		if err := scanInto(rows, reflect.ValueOf(&v).Elem(), plan); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// CollectOneStruct is CollectStructs for exactly one row: it returns
// ErrNoRows or ErrTooManyRows otherwise.
func CollectOneStruct[T any](rows *Rows) (T, error) {
	var zero T
	all, err := CollectStructs[T](rows)
	if err != nil {
		return zero, err
	}
	switch len(all) {
	case 0:
		return zero, ErrNoRows
	case 1:
		return all[0], nil
	}
	return zero, ErrTooManyRows
}

// CollectStructsByPos reads every row into a T, assigning columns to the
// struct's exported fields in order. The counts must match.
func CollectStructsByPos[T any](rows *Rows) ([]T, error) {
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		var v T
		rv := reflect.ValueOf(&v).Elem()
		var fields []any
		for i := 0; i < rv.NumField(); i++ {
			if rv.Type().Field(i).IsExported() {
				fields = append(fields, rv.Field(i).Addr().Interface())
			}
		}
		if err := rows.Scan(fields...); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// structPlan maps each column to the index path of its struct field.
func structPlan(t reflect.Type, cols []Column) ([][]int, error) {
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("pggo: %s is not a struct", t)
	}
	names := map[string]int{}
	lower := map[string]int{}
	fields := flatFields(t)
	for i, f := range fields {
		if f.tagged {
			names[f.name] = i
		} else {
			lower[normalize(f.name)] = i
		}
	}
	plan := make([][]int, len(cols))
	for ci, c := range cols {
		i, ok := names[c.Name]
		if !ok {
			i, ok = lower[normalize(c.Name)]
		}
		if !ok {
			return nil, fmt.Errorf("pggo: struct %s has no field for column %q", t, c.Name)
		}
		plan[ci] = fields[i].index
	}
	return plan, nil
}

// normalize matches untagged fields the way pgx does: ignoring case and underscores.
func normalize(s string) string { return strings.ToLower(strings.ReplaceAll(s, "_", "")) }

type flatField struct {
	name   string
	tagged bool
	index  []int
}

// flatFields lists exported fields, descending into untagged embedded structs.
func flatFields(t reflect.Type) []flatField {
	var out []flatField
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		tag, hasTag := f.Tag.Lookup("db")
		if tag == "-" {
			continue
		}
		if f.Anonymous && !hasTag && f.Type.Kind() == reflect.Struct {
			for _, sub := range flatFields(f.Type) {
				sub.index = append([]int{i}, sub.index...)
				out = append(out, sub)
			}
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		ff := flatField{name: f.Name, index: []int{i}}
		if name != "" {
			ff.name, ff.tagged = name, true
		}
		out = append(out, ff)
	}
	return out
}

func scanInto(rows *Rows, v reflect.Value, plan [][]int) error {
	dest := make([]any, len(plan))
	for ci, index := range plan {
		dest[ci] = v.FieldByIndex(index).Addr().Interface()
	}
	return rows.Scan(dest...)
}
