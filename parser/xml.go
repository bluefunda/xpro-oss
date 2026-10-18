// Package parser parses XML files into generic model.Record values using a
// caller-supplied, config-driven field mapping. There is no hardcoded XML
// schema anywhere in xpro-oss — point it at any XML shape by editing the
// `mapping` section of config.yaml.
package parser

import (
	"bytes"
	"fmt"

	"github.com/bluefunda/xpro-oss/config"
	"github.com/bluefunda/xpro-oss/model"
	"gopkg.in/xmlpath.v2"
)

// Mapping is a precompiled config.MappingField — compiled once at startup so
// repeated Parse calls don't recompile XPath expressions per file. The
// compiled path is unexported: Compile is the only correct constructor, and
// this keeps the third-party xmlpath.Path type out of this package's public
// API (so swapping the underlying XPath library later isn't a breaking change).
type Mapping struct {
	Column string
	Const  *string       // non-nil: use this literal value (even ""), ignore path
	path   *xmlpath.Path // nil (and Const nil): store the raw file content
}

// Compile precompiles the configured field mappings.
func Compile(fields []config.MappingField) ([]Mapping, error) {
	out := make([]Mapping, 0, len(fields))
	for _, f := range fields {
		m := Mapping{Column: f.Column, Const: f.Const}
		if m.Const == nil && f.XPath != "" {
			path, err := xmlpath.Compile(f.XPath)
			if err != nil {
				return nil, fmt.Errorf("mapping %q: compile xpath %q: %w", f.Column, f.XPath, err)
			}
			m.path = path
		}
		out = append(out, m)
	}
	return out, nil
}

// Parse applies the compiled mapping to raw XML content, producing a Record.
// A missing/absent XPath match yields an empty string for that column — the
// caller decides whether that's an error (e.g. via a NOT NULL DB constraint).
func Parse(sourceName string, raw []byte, mapping []Mapping) (model.Record, error) {
	root, err := xmlpath.Parse(bytes.NewReader(raw))
	if err != nil {
		return model.Record{}, fmt.Errorf("parse xml: %w", err)
	}

	rec := model.Record{SourceName: sourceName, Columns: make([]model.Column, len(mapping))}
	for i, m := range mapping {
		switch {
		case m.Const != nil:
			rec.Columns[i] = model.Column{Name: m.Column, Value: *m.Const}
		case m.path == nil:
			rec.Columns[i] = model.Column{Name: m.Column, Value: string(raw)}
		default:
			value, _ := m.path.String(root)
			rec.Columns[i] = model.Column{Name: m.Column, Value: value}
		}
	}
	return rec, nil
}
