package model

import (
	"reflect"
	"testing"
)

func TestRecord_ValuesAndColumnNames(t *testing.T) {
	r := Record{Columns: []Column{{"a", "1"}, {"b", "2"}}}
	if got := r.Values(); !reflect.DeepEqual(got, []any{"1", "2"}) {
		t.Errorf("Values = %v", got)
	}
	if got := r.ColumnNames(); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("ColumnNames = %v", got)
	}
}

func TestRecord_Empty(t *testing.T) {
	var r Record
	if len(r.Values()) != 0 || len(r.ColumnNames()) != 0 {
		t.Error("zero Record should have no values or names")
	}
}
