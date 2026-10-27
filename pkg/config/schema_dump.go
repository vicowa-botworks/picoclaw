//go:build ignore

// Extracts the JSON field map that pkg/config/diagnostics.go builds by
// reflection, so the Rust port can refuse the same keys without a reflect
// package.
//
// Run with:
//
//	go run pkg/config/schema_dump.go > /tmp/schema.json
//
// It is `//go:build ignore` on purpose: it is a maintenance tool, not part of the
// package or the test suite, and it must not be compiled into a release binary.
package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// node is one schema entry: the JSON names a document may use at this position,
// and what each of them leads to.
type node struct {
	Kind string `json:"kind"` // struct | map | slice | scalar
	// Fields is set for kind=struct: json name -> child.
	Fields map[string]*node `json:"fields,omitempty"`
	// Elem is set for kind=map and kind=slice.
	Elem *node `json:"elem,omitempty"`
	// Name is the Go type, for the error message and for a human reading the dump.
	Name string `json:"name,omitempty"`
	// Omit is the field's encoding option: "omitzero", "omitempty", or absent.
	// The validator does not need it -- an unknown key is unknown either way -- but
	// the *writer* does: a field carrying omitzero disappears from the document when
	// its value is zero, which is why a config with an empty session has no session
	// key at all. Without this the port has to guess, and a wrong guess writes a key
	// Go leaves out.
	Omit string `json:"omit,omitempty"`
}

// omitOption reads the encoding option out of a json tag: the part after the name.
func omitOption(tag *ast.BasicLit) string {
	if tag == nil {
		return ""
	}
	v, err := strconv.Unquote(tag.Value)
	if err != nil {
		return ""
	}
	parts := strings.Split(reflect.StructTag(v).Get("json"), ",")
	if len(parts) < 2 {
		return ""
	}
	switch parts[1] {
	case "omitzero", "omitempty":
		return parts[1]
	}
	return ""
}

type ctx struct {
	fset    *token.FileSet
	structs map[string]*ast.StructType
	// aliases is every other named type in the directory -- `type ChannelsConfig
	// map[string]*Channel` and friends. Without it a named map or slice looks like
	// a scalar, and the validator would then accept any key inside one: exactly the
	// class of hole this dump exists to close.
	aliases map[string]ast.Expr
	named   map[string]bool
	// cache stops a recursive type from being walked twice, and makes a cycle
	// terminate: a self-referential struct gets a scalar placeholder the second
	// time, which is what reflect would report as the same shape anyway.
	cache map[string]*node
}

func main() {
	dir := os.Args[1]
	fset := token.NewFileSet()
	c := &ctx{
		fset:    fset,
		structs: map[string]*ast.StructType{},
		aliases: map[string]ast.Expr{},
		named:   map[string]bool{},
		cache:   map[string]*node{},
	}

	files, err := os.ReadDir(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, entry := range files {
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, dir+"/"+entry.Name(), nil, 0)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					c.aliases[ts.Name.Name] = ts.Type
					continue
				}
				c.structs[ts.Name.Name] = st
				c.named[ts.Name.Name] = true
			}
		}
	}

	root, ok := c.structs[os.Args[2]]
	if !ok {
		fmt.Fprintf(os.Stderr, "no struct %s in %s\n", os.Args[2], dir)
		os.Exit(1)
	}
	out := c.structNode(os.Args[2], root, 0)
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", " ")
	_ = enc.Encode(out)
}

// structNode walks one struct the way populateJSONFieldTypeMap does: unexported
// fields skipped, `json:"-"` skipped, an anonymous field with no name inlined, an
// untagged field named by its Go name.
func (c *ctx) structNode(name string, st *ast.StructType, depth int) *node {
	if cached, ok := c.cache[name]; ok && depth > 0 {
		return cached
	}
	out := &node{Kind: "struct", Fields: map[string]*node{}, Name: name}
	if depth > 0 {
		c.cache[name] = out
	}
	for _, field := range st.Fields.List {
		if len(field.Names) == 0 {
			// embedded
			tn := typeString(field.Type)
			if !c.named[tn] {
				continue
			}
			// An embedded field with no json name is inlined. A tagged one is a
			// normal field; the diagnostics code checks the tag first, and no
			// embedded field in pkg/config carries one (verified: the dump has no
			// inlined type name as a key).
			if sub, ok := c.structs[tn]; ok {
				child := c.structNode(tn, sub, depth+1)
				for k, v := range child.Fields {
					out.Fields[k] = v
				}
			}
			continue
		}
		tag := tagJSONName(field.Tag)
		if tag == "-" {
			continue
		}
		for _, ident := range field.Names {
			if !ast.IsExported(ident.Name) {
				continue
			}
			fieldName := tag
			if fieldName == "" {
				fieldName = ident.Name
			}
			child := c.nodeFor(field.Type, depth+1)
			child.Omit = omitOption(field.Tag)
			out.Fields[fieldName] = child
		}
	}
	return out
}

func (c *ctx) nodeFor(expr ast.Expr, depth int) *node {
	switch e := strip(expr).(type) {
	case *ast.Ident:
		if st, ok := c.structs[e.Name]; ok {
			return c.structNode(e.Name, st, depth)
		}
		// A named map or slice (ChannelsConfig, ModelList, FlexibleStringSlice)
		// resolves through its declaration; reflect would see the same shape.
		if underlying, ok := c.aliases[e.Name]; ok && depth < 12 {
			return c.nodeFor(underlying, depth+1)
		}
		return &node{Kind: "scalar", Name: e.Name}
	case *ast.SelectorExpr:
		return &node{Kind: "scalar", Name: typeString(e)}
	case *ast.ArrayType:
		return &node{Kind: "slice", Elem: c.nodeFor(e.Elt, depth), Name: "[]" + typeString(e.Elt)}
	case *ast.MapType:
		return &node{Kind: "map", Elem: c.nodeFor(e.Value, depth), Name: "map[" + typeString(e.Key) + "]" + typeString(e.Value)}
	case *ast.StarExpr:
		return c.nodeFor(e.X, depth)
	case *ast.InterfaceType:
		return &node{Kind: "scalar", Name: "any"}
	default:
		return &node{Kind: "scalar", Name: typeString(expr)}
	}
}

func strip(expr ast.Expr) ast.Expr {
	for {
		star, ok := expr.(*ast.StarExpr)
		if !ok {
			return expr
		}
		expr = star.X
	}
}

func tagJSONName(tag *ast.BasicLit) string {
	if tag == nil {
		return ""
	}
	v, err := strconv.Unquote(tag.Value)
	if err != nil {
		return ""
	}
	return strings.Split(reflect.StructTag(v).Get("json"), ",")[0]
}

func typeString(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.StarExpr:
		return "*" + typeString(e.X)
	case *ast.ArrayType:
		return "[]" + typeString(e.Elt)
	case *ast.SelectorExpr:
		return typeString(e.X) + "." + e.Sel.Name
	case *ast.MapType:
		return "map[" + typeString(e.Key) + "]" + typeString(e.Value)
	case *ast.InterfaceType:
		return "any"
	default:
		return "?"
	}
}

var _ = sort.Strings
