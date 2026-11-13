// Command gen turns the pinned Chrome DevTools Protocol JSON into a typed Go
// client for the cdp package (types, commands, events). Run via
// `go generate ./...` from internal/cdp. Output is LLM-invisible plumbing.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/format"
	"os"
	"sort"
	"strconv"
	"strings"
)

// ---------- protocol JSON model ----------

type Protocol struct {
	Version struct {
		Major string `json:"major"`
		Minor string `json:"minor"`
	} `json:"version"`
	Domains []Domain `json:"domains"`
}

type Domain struct {
	Domain       string       `json:"domain"`
	Dependencies []string     `json:"dependencies"`
	Types        []TypeDef    `json:"types"`
	Commands     []CommandDef `json:"commands"`
	Events       []EventDef   `json:"events"`
}

type TypeDef struct {
	ID           string          `json:"id"`
	Description  string          `json:"description"`
	Type         string          `json:"type"`
	Ref          string          `json:"$ref"`
	Enum         json.RawMessage `json:"enum"`
	Properties   []PropertyDef   `json:"properties"`
	Items        *ItemsDef       `json:"items"`
	Experimental bool            `json:"experimental"`
	Deprecated   bool            `json:"deprecated"`
}

type CommandDef struct {
	Name         string        `json:"name"`
	Description  string        `json:"description"`
	Parameters   []PropertyDef `json:"parameters"`
	Returns      []PropertyDef `json:"returns"`
	Experimental bool          `json:"experimental"`
	Deprecated   bool          `json:"deprecated"`
}

type EventDef struct {
	Name         string        `json:"name"`
	Description  string        `json:"description"`
	Parameters   []PropertyDef `json:"parameters"`
	Experimental bool          `json:"experimental"`
}

type PropertyDef struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	Type         string          `json:"type"`
	Ref          string          `json:"$ref"`
	Optional     bool            `json:"optional"`
	Enum         json.RawMessage `json:"enum"`
	Items        *ItemsDef       `json:"items"`
	Properties   []PropertyDef   `json:"properties"` // when an inline object type
	Experimental bool            `json:"experimental"`
	Deprecated   bool            `json:"deprecated"`
}

type ItemsDef struct {
	Type string `json:"type"`
	Ref  string `json:"$ref"`
}

// ---------- main ----------

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
}

func run() error {
	proto, err := loadProtocol()
	if err != nil {
		return err
	}
	collectObjectTypes(proto)
	if err := emitTypes(proto); err != nil {
		return fmt.Errorf("types: %w", err)
	}
	if err := emitMethods(proto); err != nil {
		return fmt.Errorf("methods: %w", err)
	}
	if err := emitEvents(proto); err != nil {
		return fmt.Errorf("events: %w", err)
	}
	return nil
}

func loadProtocol() (*Protocol, error) {
	// cwd is internal/cdp when run via go:generate.
	merged := &Protocol{}
	for _, f := range []string{"protocol/browser_protocol.json", "protocol/js_protocol.json"} {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var p Protocol
		if err := json.Unmarshal(b, &p); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		merged.Domains = append(merged.Domains, p.Domains...)
	}
	sort.Slice(merged.Domains, func(i, j int) bool {
		return merged.Domains[i].Domain < merged.Domains[j].Domain
	})
	return merged, nil
}

// ---------- naming & type mapping ----------

// objectTypes holds the Go names of every CDP type that generates a struct.
// $ref to one of these emits a pointer (*Type) to break CDP's value-recursion
// (e.g. Node.contentDocument:Document <-> Document.node:Node, or
// StackTrace.parent:StackTrace).
var objectTypes = map[string]bool{}

func collectObjectTypes(proto *Protocol) {
	for _, d := range proto.Domains {
		for _, t := range d.Types {
			if t.Type == "object" && len(t.Properties) > 0 {
				objectTypes[d.Domain+t.ID] = true
			}
		}
	}
}

// refGoType resolves a $ref to its Go type name, as a pointer when it targets
// a struct (to break recursion) and a value otherwise.
func refGoType(domain, ref string) string {
	name := goTypeOfRef(domain, ref)
	if objectTypes[name] {
		return "*" + name
	}
	return name
}

// goTypeOfRef turns a CDP $ref into a Go type name. "Type" resolves in the
// current domain; "OtherDomain.Type" resolves in that domain. Names are
// domain-prefixed so they are unique package-wide.
func goTypeOfRef(domain, ref string) string {
	if i := strings.IndexByte(ref, '.'); i >= 0 {
		return ref[:i] + ref[i+1:] // OtherDomain.Type -> OtherDomainType
	}
	return domain + ref
}

func prim(t string) string {
	switch t {
	case "string":
		return "string"
	case "integer":
		return "int64"
	case "number":
		return "float64"
	case "boolean":
		return "bool"
	case "object":
		return "map[string]any"
	case "any":
		return "any"
	default:
		return "any"
	}
}

// goType returns the Go type for a property/type/return.
func goType(p *PropertyDef, domain string) string {
	switch {
	case p.Ref != "":
		return refGoType(domain, p.Ref)
	case p.Type == "array":
		return "[]" + itemsType(p.Items, domain)
	case p.Type == "object" && len(p.Properties) > 0:
		return inlineStruct(p.Properties, domain)
	default:
		return prim(p.Type)
	}
}

func itemsType(it *ItemsDef, domain string) string {
	if it == nil {
		return "any"
	}
	if it.Ref != "" {
		return refGoType(domain, it.Ref)
	}
	return prim(it.Type)
}

// inlineStruct builds an anonymous struct for an inline object type.
func inlineStruct(props []PropertyDef, domain string) string {
	if len(props) == 0 {
		return "map[string]any"
	}
	var b strings.Builder
	b.WriteString("struct {\n")
	for i := range props {
		p := &props[i]
		fmt.Fprintf(&b, "%s %s %s\n", goFieldName(p.Name), goType(p, domain), tag(p))
	}
	b.WriteString("}")
	return b.String()
}

// goFieldName capitalises the first letter of a camelCase CDP name.
func goFieldName(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = []rune(strings.ToUpper(string(r[0])))[0]
	if string(r) == "Id" {
		return "ID"
	}
	return string(r)
}

// tag emits a struct tag for a property, e.g. `json:"name,omitempty"`.
func tag(p *PropertyDef) string {
	t := `json:"` + p.Name
	if p.Optional {
		t += ",omitempty"
	}
	t += `"`
	return "`" + t + "`"
}

// docComment writes a Go doc comment, collapsing newlines.
func docComment(desc, prefix string) string {
	if desc == "" {
		return ""
	}
	desc = strings.ReplaceAll(desc, "\n", " ")
	return prefix + "// " + desc + "\n"
}

// ---------- emit ----------

func writeFile(path, header string, body *bytes.Buffer) error {
	out := append([]byte(header), body.Bytes()...)
	formatted, err := format.Source(out)
	if err != nil {
		return fmt.Errorf("format %s: %w", path, err)
	}
	return os.WriteFile(path, formatted, 0o644)
}

func emitTypes(proto *Protocol) error {
	var b bytes.Buffer
	for _, d := range proto.Domains {
		for i := range d.Types {
			t := &d.Types[i]
			name := d.Domain + t.ID
			b.WriteString(docComment(t.Description, ""))
			switch {
			case len(t.Enum) > 0:
				base := prim(t.Type) // string, int64, ...
				fmt.Fprintf(&b, "type %s %s\n\n", name, base)
				b.WriteString("const (\n")
				for _, e := range decodeEnum(t.Enum) {
					cname := name + exportIdent(e)
					fmt.Fprintf(&b, "%s %s = %s\n", cname, name, enumLiteral(e, t.Type))
				}
				b.WriteString(")\n\n")
			case t.Type == "object":
				fmt.Fprintf(&b, "type %s %s\n\n", name, inlineStruct(t.Properties, d.Domain))
			case t.Ref != "":
				fmt.Fprintf(&b, "type %s %s\n\n", name, refGoType(d.Domain, t.Ref))
			case t.Type == "array":
				fmt.Fprintf(&b, "type %s []%s\n\n", name, itemsType(t.Items, d.Domain))
			default:
				fmt.Fprintf(&b, "type %s %s\n\n", name, prim(t.Type))
			}
		}
	}
	return writeFile("cdp_types_gen.go",
		"// Code generated by gen (CDP protocol). DO NOT EDIT.\n\npackage cdp\n\n", &b)
}

func emitMethods(proto *Protocol) error {
	var b bytes.Buffer
	for _, d := range proto.Domains {
		for i := range d.Commands {
			c := &d.Commands[i]
			paramsName := d.Domain + export(c.Name) + "Params"
			returnsName := d.Domain + export(c.Name) + "Returns"
			hasParams := len(c.Parameters) > 0
			hasReturns := len(c.Returns) > 0

			if hasParams {
				b.WriteString(docComment(c.Description, ""))
				fmt.Fprintf(&b, "type %s struct {\n", paramsName)
				for j := range c.Parameters {
					p := &c.Parameters[j]
					fmt.Fprintf(&b, "%s %s %s\n", goFieldName(p.Name), goType(p, d.Domain), tag(p))
				}
				b.WriteString("}\n\n")
			}
			if hasReturns {
				fmt.Fprintf(&b, "type %s struct {\n", returnsName)
				for j := range c.Returns {
					p := &c.Returns[j]
					fmt.Fprintf(&b, "%s %s %s\n", goFieldName(p.Name), goType(p, d.Domain), tag(p))
				}
				b.WriteString("}\n\n")
			}

			b.WriteString(docComment(c.Description, ""))
			fmt.Fprintf(&b, "func (c *Client) %s%s(ctx context.Context", d.Domain, export(c.Name))
			if hasParams {
				fmt.Fprintf(&b, ", p *%s", paramsName)
			}
			fmt.Fprintf(&b, ") (")
			if hasReturns {
				fmt.Fprintf(&b, "*%s, ", returnsName)
			}
			fmt.Fprintf(&b, "error) {\n")
			if hasReturns {
				fmt.Fprintf(&b, "var out %s\n", returnsName)
				fmt.Fprintf(&b, "if err := c.call(ctx, %q, ", d.Domain+"."+c.Name)
				if hasParams {
					b.WriteString("p")
				} else {
					b.WriteString("nil")
				}
				fmt.Fprintf(&b, ", &out); err != nil {\nreturn nil, err\n}\nreturn &out, nil\n}\n\n")
			} else {
				fmt.Fprintf(&b, "return c.call(ctx, %q, ", d.Domain+"."+c.Name)
				if hasParams {
					b.WriteString("p")
				} else {
					b.WriteString("nil")
				}
				b.WriteString(", nil)\n}\n\n")
			}
		}
	}
	return writeFile("cdp_methods_gen.go",
		"// Code generated by gen (CDP protocol). DO NOT EDIT.\n\npackage cdp\n\nimport \"context\"\n\n", &b)
}

func emitEvents(proto *Protocol) error {
	var b bytes.Buffer
	for _, d := range proto.Domains {
		for i := range d.Events {
			e := &d.Events[i]
			name := d.Domain + export(e.Name) + "Event"
			b.WriteString(docComment(e.Description, ""))
			fmt.Fprintf(&b, "type %s struct {\n", name)
			for j := range e.Parameters {
				p := &e.Parameters[j]
				fmt.Fprintf(&b, "%s %s %s\n", goFieldName(p.Name), goType(p, d.Domain), tag(p))
			}
			b.WriteString("}\n\n")
			fmt.Fprintf(&b, "func (e %s) CDPMethod() string { return %q }\n\n", name, d.Domain+"."+e.Name)
		}
	}
	return writeFile("cdp_events_gen.go",
		"// Code generated by gen (CDP protocol). DO NOT EDIT.\n\npackage cdp\n\n", &b)
}

// export camelCases a CDP identifier (lowerCamel -> UpperCamel).
func export(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = []rune(strings.ToUpper(string(r[0])))[0]
	return string(r)
}

// decodeEnum turns a CDP enum array (strings, and occasionally integers) into
// Go string literals.
func decodeEnum(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var arr []any
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil
	}
	out := make([]string, len(arr))
	for i, v := range arr {
		switch x := v.(type) {
		case string:
			out[i] = x
		case float64:
			out[i] = strconv.FormatInt(int64(x), 10)
		default:
			out[i] = fmt.Sprintf("%v", x)
		}
	}
	return out
}

// exportIdent makes a Go identifier fragment from an enum value by replacing
// non-alphanumeric characters (e.g. "image/webp" -> "image_webp").
func exportIdent(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// enumLiteral renders an enum value as a Go literal for its base type.
func enumLiteral(v, typ string) string {
	switch typ {
	case "integer", "number":
		return v
	default:
		return fmt.Sprintf("%q", v)
	}
}
