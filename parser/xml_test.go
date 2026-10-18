package parser

import (
	"testing"

	"github.com/bluefunda/xpro-oss/config"
)

// billingEnvelopeFixture mirrors the shape of the SAP ABAP billing XML
// envelope (asx:abap → asx:values → GS_DOC_HEADER → BILL_DATA / SNAP_DATA /
// EVENT_DATA) that xpro-oss's sibling SAP billing ingestion daemons consume
// in production. Field names match that real-world shape; all values are
// synthetic and carry no customer-identifying information.
//
// This fixture exists to prove a concrete claim: a config-driven mapping —
// with zero SAP-specific code anywhere in xpro-oss — can correctly extract
// every field that a hand-written, schema-specific parser (like a
// vendor-specific billing parser) would extract from the same input.
const billingEnvelopeFixture = `<asx:abap xmlns:asx="http://www.sap.com/abapxml">
  <asx:values>
    <GS_DOC_HEADER>
      <BILL_DATA>
        <BILL_DOC_HEADER>
          <COMPANY_CODE>1000</COMPANY_CODE>
          <SERVICE_LINE>ELEC</SERVICE_LINE>
          <ACCT_NO>0000123456</ACCT_NO>
          <DOC_DATE>2026-01-15</DOC_DATE>
          <POSTING_DATE>2026-01-16</POSTING_DATE>
          <PRINTDOC>PD-000001</PRINTDOC>
          <BILL_KEY>2026-01-01</BILL_KEY>
        </BILL_DOC_HEADER>
      </BILL_DATA>
      <SNAP_DATA>
        <BUSINESSPARTNERNUMBER>BP-0009876</BUSINESSPARTNERNUMBER>
        <PREMISENUMBER>PR-0004321</PREMISENUMBER>
        <FINALBILLREFUND></FINALBILLREFUND>
      </SNAP_DATA>
      <EVENT_DATA>
        <LETTER_CODE>STD</LETTER_CODE>
      </EVENT_DATA>
    </GS_DOC_HEADER>
  </asx:values>
</asx:abap>`

// billingEnvelopeMapping is what an xpro-oss operator would put in
// config.yaml's `mapping` section to ingest the fixture above — the direct
// config-driven equivalent of the Go struct tags a hand-written parser would
// use for the same fields.
func billingEnvelopeMapping() []config.MappingField {
	return []config.MappingField{
		{Column: "company_code", XPath: "/abap/values/GS_DOC_HEADER/BILL_DATA/BILL_DOC_HEADER/COMPANY_CODE"},
		{Column: "line_of_business", XPath: "/abap/values/GS_DOC_HEADER/BILL_DATA/BILL_DOC_HEADER/SERVICE_LINE"},
		{Column: "account_number", XPath: "/abap/values/GS_DOC_HEADER/BILL_DATA/BILL_DOC_HEADER/ACCT_NO"},
		{Column: "bill_date", XPath: "/abap/values/GS_DOC_HEADER/BILL_DATA/BILL_DOC_HEADER/DOC_DATE"},
		{Column: "print_date", XPath: "/abap/values/GS_DOC_HEADER/BILL_DATA/BILL_DOC_HEADER/POSTING_DATE"},
		{Column: "reference_id", XPath: "/abap/values/GS_DOC_HEADER/BILL_DATA/BILL_DOC_HEADER/PRINTDOC"},
		{Column: "cycle_date", XPath: "/abap/values/GS_DOC_HEADER/BILL_DATA/BILL_DOC_HEADER/BILL_KEY"},
		{Column: "customer_number", XPath: "/abap/values/GS_DOC_HEADER/SNAP_DATA/BUSINESSPARTNERNUMBER"},
		{Column: "premise_number", XPath: "/abap/values/GS_DOC_HEADER/SNAP_DATA/PREMISENUMBER"},
		{Column: "letter_code", XPath: "/abap/values/GS_DOC_HEADER/EVENT_DATA/LETTER_CODE"},
		{Column: "raw_xml", XPath: ""},
	}
}

func TestParse_BillingEnvelopeShape(t *testing.T) {
	mapping, err := Compile(billingEnvelopeMapping())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	rec, err := Parse("sample.xml", []byte(billingEnvelopeFixture), mapping)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	want := map[string]string{
		"company_code":     "1000",
		"line_of_business": "ELEC",
		"account_number":   "0000123456",
		"bill_date":        "2026-01-15",
		"print_date":       "2026-01-16",
		"reference_id":     "PD-000001",
		"cycle_date":       "2026-01-01",
		"customer_number":  "BP-0009876",
		"premise_number":   "PR-0004321",
		"letter_code":      "STD",
	}

	got := make(map[string]string, len(rec.Columns))
	for _, c := range rec.Columns {
		got[c.Name] = c.Value
	}

	for col, wantVal := range want {
		if got[col] != wantVal {
			t.Errorf("column %q: got %q, want %q", col, got[col], wantVal)
		}
	}

	if got["raw_xml"] != billingEnvelopeFixture {
		t.Errorf("raw_xml column: got %d bytes, want the full %d-byte fixture", len(got["raw_xml"]), len(billingEnvelopeFixture))
	}

	if rec.SourceName != "sample.xml" {
		t.Errorf("SourceName: got %q, want %q", rec.SourceName, "sample.xml")
	}

	if len(rec.Columns) != len(want)+1 { // +1 for raw_xml
		t.Errorf("Columns: got %d entries, want %d", len(rec.Columns), len(want)+1)
	}
}

// TestParse_MissingField documents the contract for XPath expressions that
// don't match: an empty element and a wholly absent element both yield an
// empty string rather than an error. The caller (e.g. a target DB's NOT
// NULL constraint) decides whether that's fatal.
func TestParse_MissingField(t *testing.T) {
	mapping, err := Compile([]config.MappingField{
		{Column: "final_bill_refund", XPath: "/abap/values/GS_DOC_HEADER/SNAP_DATA/FINALBILLREFUND"}, // present but empty
		{Column: "missing", XPath: "/abap/values/GS_DOC_HEADER/SNAP_DATA/DOES_NOT_EXIST"},            // absent entirely
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	rec, err := Parse("sample.xml", []byte(billingEnvelopeFixture), mapping)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	for _, c := range rec.Columns {
		if c.Value != "" {
			t.Errorf("column %q: got %q, want empty string", c.Name, c.Value)
		}
	}
}

// TestParse_Const confirms a mapping field with Const set always yields that
// literal, ignoring XPath entirely, for every record regardless of content.
func TestParse_Const(t *testing.T) {
	mapping, err := Compile([]config.MappingField{
		{Column: "source_system", Const: ptr("XPRO-OSS-TEST")},
		{Column: "company_code", XPath: "/abap/values/GS_DOC_HEADER/BILL_DATA/BILL_DOC_HEADER/COMPANY_CODE"},
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	rec, err := Parse("sample.xml", []byte(billingEnvelopeFixture), mapping)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	got := make(map[string]string, len(rec.Columns))
	for _, c := range rec.Columns {
		got[c.Name] = c.Value
	}
	if got["source_system"] != "XPRO-OSS-TEST" {
		t.Errorf("source_system: got %q, want %q", got["source_system"], "XPRO-OSS-TEST")
	}
	if got["company_code"] != "1000" {
		t.Errorf("company_code: got %q, want %q (const must not affect other fields)", got["company_code"], "1000")
	}
}

func ptr(s string) *string { return &s }

// TestParse_EmptyConstIsNotRawXML: an explicitly empty const (e.g. an unset
// ${RUN_ID}) must store "", never fall through to "store the raw file".
func TestParse_EmptyConstIsNotRawXML(t *testing.T) {
	mapping, err := Compile([]config.MappingField{
		{Column: "run", Const: ptr("")},
		{Column: "raw"}, // no xpath, no const => raw file
	})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := Parse("a.xml", []byte("<a/>"), mapping)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Columns[0].Value != "" || rec.Columns[1].Value != "<a/>" {
		t.Errorf("columns = %+v", rec.Columns)
	}
}
