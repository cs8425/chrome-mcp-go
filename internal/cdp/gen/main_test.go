package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestGoFieldName(t *testing.T) {
	cases := map[string]string{
		"depth":             "Depth",
		"pierce":            "Pierce",
		"targetId":          "TargetId",
		"deviceScaleFactor": "DeviceScaleFactor",
		"URL":               "URL",
		"mobile":            "Mobile",
	}
	for in, want := range cases {
		if got := goFieldName(in); got != want {
			t.Errorf("goFieldName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExportIdent(t *testing.T) {
	cases := map[string]string{
		"image/webp": "image_webp",
		"text/css":   "text_css",
		"alert":      "alert",
		"a-b c":      "a_b_c",
	}
	for in, want := range cases {
		if got := exportIdent(in); got != want {
			t.Errorf("exportIdent(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDecodeEnum(t *testing.T) {
	if got := decodeEnum(json.RawMessage(`["a","b","c"]`)); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Errorf("string enum = %#v", got)
	}
	if got := decodeEnum(json.RawMessage(`[1,2,10]`)); !reflect.DeepEqual(got, []string{"1", "2", "10"}) {
		t.Errorf("integer enum = %#v", got)
	}
	if got := decodeEnum(nil); got != nil {
		t.Errorf("nil enum = %#v, want nil", got)
	}
}

func TestEnumLiteral(t *testing.T) {
	if got := enumLiteral("42", "integer"); got != "42" {
		t.Errorf("int literal = %q, want 42", got)
	}
	if got := enumLiteral("alert", "string"); got != `"alert"` {
		t.Errorf("string literal = %q, want \"alert\"", got)
	}
}

func TestGoTypePrimitives(t *testing.T) {
	cases := map[string]string{
		"string":  "string",
		"integer": "int64",
		"number":  "float64",
		"boolean": "bool",
		"any":     "any",
	}
	for in, want := range cases {
		p := &PropertyDef{Type: in}
		if got := goType(p, "DOM"); got != want {
			t.Errorf("goType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGoTypeRefPointer(t *testing.T) {
	// $ref to a struct type must emit a pointer (recursion-safe); to a
	// primitive alias must emit a value.
	objectTypes["DOMNode"] = true
	defer delete(objectTypes, "DOMNode")

	obj := &PropertyDef{Ref: "Node"}
	if got := goType(obj, "DOM"); got != "*DOMNode" {
		t.Errorf("$ref object = %q, want *DOMNode", got)
	}
	// Cross-domain ref.
	objectTypes["RuntimeRemoteObject"] = true
	defer delete(objectTypes, "RuntimeRemoteObject")
	xdom := &PropertyDef{Ref: "Runtime.RemoteObject"}
	if got := goType(xdom, "DOM"); got != "*RuntimeRemoteObject" {
		t.Errorf("cross-domain $ref object = %q, want *RuntimeRemoteObject", got)
	}
	// Ref to a primitive-named type (not registered as object) -> value.
	primitive := &PropertyDef{Ref: "NodeId"}
	if got := goType(primitive, "DOM"); got != "DOMNodeId" {
		t.Errorf("$ref primitive = %q, want DOMNodeId", got)
	}
}

func TestGoTypeArrayAndInline(t *testing.T) {
	arr := &PropertyDef{Type: "array", Items: &ItemsDef{Type: "string"}}
	if got := goType(arr, "DOM"); got != "[]string" {
		t.Errorf("array = %q, want []string", got)
	}
	inline := &PropertyDef{Type: "object", Properties: []PropertyDef{
		{Name: "x", Type: "integer"},
		{Name: "y", Type: "integer"},
	}}
	got := goType(inline, "DOM")
	if !strings.Contains(got, "X int64") || !strings.Contains(got, "Y int64") {
		t.Errorf("inline struct = %q", got)
	}
}

func TestTag(t *testing.T) {
	opt := &PropertyDef{Name: "depth", Optional: true}
	if got := tag(opt); got != "`json:\"depth,omitempty\"`" {
		t.Errorf("optional tag = %s", got)
	}
	req := &PropertyDef{Name: "url"}
	if got := tag(req); got != "`json:\"url\"`" {
		t.Errorf("required tag = %s", got)
	}
}
