// Package model defines the generic, schema-less record shape that flows
// from a Source through the parser into a Target database. xpro-oss has no
// built-in knowledge of any particular XML schema — every field is driven by
// the column mapping in config.yaml.
package model

// Record is one parsed unit of work: the source file it came from and the
// mapped column values ready for insertion. The raw file content is not
// retained (a raw-XML column carries its own copy), so a batch of records
// does not pin every source file in memory.
type Record struct {
	// SourceName is the source-relative file/object name this record was read from.
	SourceName string

	// Columns holds one entry per configured mapping field, in mapping order.
	Columns []Column
}

// Column is one mapped output value.
type Column struct {
	Name  string
	Value string
}

// Values returns the column values in mapping order, suitable for
// database/sql positional parameter binding.
func (r Record) Values() []any {
	out := make([]any, len(r.Columns))
	for i, c := range r.Columns {
		out[i] = c.Value
	}
	return out
}

// ColumnNames returns the column names in mapping order.
func (r Record) ColumnNames() []string {
	out := make([]string, len(r.Columns))
	for i, c := range r.Columns {
		out[i] = c.Name
	}
	return out
}
