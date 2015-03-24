package ccg

//go:generate myccg -u AstDecls.Filter -p ccg -o utils.go slice ast.Decl AstDecls
//go:generate myccg -u AstSpecs.Filter -p ccg -o utils.go slice ast.Spec AstSpecs
//go:generate myccg -u ObjectSet.Add,ObjectSet.In,NewObjectSet -p ccg -o utils.go set types.Object ObjectSet NewObjectSet

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/token"
	"io"
	"strings"

	"golang.org/x/tools/go/loader"
	"golang.org/x/tools/go/types"
	"golang.org/x/tools/imports"
)

var (
	pt = fmt.Printf
)

type Config struct {
	From    string
	Params  map[string]string
	Renames map[string]string
	Writer  io.Writer
	Package string
	Decls   []ast.Decl
	FileSet *token.FileSet
	Uses    []string
}

func Copy(config Config) error {
	// load package
	loadConf := loader.Config{
		Fset: config.FileSet,
	}
	loadConf.Import(config.From)
	program, err := loadConf.Load()
	if err != nil {
		return fmt.Errorf("ccg: load package %v", err)
	}
	info := program.Imported[config.From]

	// remove param declarations
	for _, f := range info.Files {
		f.Decls = filterDecls(f.Decls, func(node interface{}) bool {
			switch node := node.(type) {
			case *ast.TypeSpec:
				name := node.Name.Name
				_, exists := config.Params[name]
				return !exists
			case valueInfo:
				_, exists := config.Params[node.Name.Name]
				return !exists
			}
			return true
		})
	}

	// collect objects to rename
	renamed := map[string]string{}
	objects := make(map[types.Object]string)
	collectObjects := func(mapping map[string]string) error {
		for from, to := range mapping {
			obj := info.Pkg.Scope().Lookup(from)
			if obj == nil {
				return fmt.Errorf("ccg: name not found %s", from)
			}
			objects[obj] = to
			renamed[to] = from
		}
		return nil
	}
	if err := collectObjects(config.Params); err != nil {
		return err
	}
	if err := collectObjects(config.Renames); err != nil {
		return err
	}

	// rename
	rename := func(defs map[*ast.Ident]types.Object) {
		for id, obj := range defs {
			if to, ok := objects[obj]; ok {
				id.Name = to
			}
		}
	}
	rename(info.Defs)
	rename(info.Uses)

	// collect existing decls
	existingDecls := make(map[string]func(interface{}))
	var decls []ast.Decl
	for i, decl := range config.Decls {
		decls = append(decls, decl)
		switch decl := decl.(type) {
		case *ast.GenDecl:
			switch decl.Tok {
			case token.VAR, token.CONST:
				for _, spec := range decl.Specs {
					spec := spec.(*ast.ValueSpec)
					for i, name := range spec.Names {
						i := i
						spec := spec
						existingDecls[name.Name] = func(expr interface{}) {
							spec.Values[i] = expr.(ast.Expr)
						}
					}
				}
			case token.TYPE:
				for i, spec := range decl.Specs {
					spec := spec.(*ast.TypeSpec)
					i := i
					decl := decl
					existingDecls[spec.Name.Name] = func(expr interface{}) {
						decl.Specs[i].(*ast.TypeSpec).Type = expr.(ast.Expr)
					}
				}
			case token.IMPORT:
				for i, spec := range decl.Specs {
					spec := spec.(*ast.ImportSpec)
					i := i
					decl := decl
					var name string
					if spec.Name == nil {
						name = spec.Path.Value
					} else {
						name = spec.Name.Name
					}
					existingDecls[name] = func(path interface{}) {
						decl.Specs[i].(*ast.ImportSpec).Path = path.(*ast.BasicLit)
					}
				}
			}
		case *ast.FuncDecl:
			name := decl.Name.Name
			if decl.Recv != nil {
				name = decl.Recv.List[0].Type.(*ast.Ident).Name + "." + name
			}
			i := i
			existingDecls[name] = func(fndecl interface{}) {
				decls[i] = fndecl.(*ast.FuncDecl)
			}
		}
	}

	// collect output declarations
	var newDecls []ast.Decl
	for _, f := range info.Files {
		for _, decl := range f.Decls {
			switch decl := decl.(type) {
			case *ast.GenDecl:
				switch decl.Tok {
				case token.VAR, token.CONST:
					newDecl := &ast.GenDecl{
						Tok: decl.Tok,
					}
					for _, spec := range decl.Specs {
						spec := spec.(*ast.ValueSpec)
						for i, name := range spec.Names {
							if mutator, ok := existingDecls[name.Name]; ok {
								mutator(spec.Values[i])
							} else {
								newDecl.Specs = append(newDecl.Specs, spec)
							}
						}
					}
					if len(newDecl.Specs) > 0 {
						newDecls = append(newDecls, newDecl)
					}
				case token.TYPE:
					newDecl := &ast.GenDecl{
						Tok: token.TYPE,
					}
					for _, spec := range decl.Specs {
						spec := spec.(*ast.TypeSpec)
						name := spec.Name.Name
						if mutator, ok := existingDecls[name]; ok {
							mutator(spec.Type)
						} else {
							newDecl.Specs = append(newDecl.Specs, spec)
						}
					}
					if len(newDecl.Specs) > 0 {
						newDecls = append(newDecls, newDecl)
					}
				case token.IMPORT:
					newDecl := &ast.GenDecl{
						Tok: token.IMPORT,
					}
					for _, spec := range decl.Specs {
						spec := spec.(*ast.ImportSpec)
						var name string
						if spec.Name == nil {
							name = spec.Path.Value
						} else {
							name = spec.Name.Name
						}
						if mutator, ok := existingDecls[name]; ok {
							mutator(spec.Path)
						} else {
							newDecl.Specs = append(newDecl.Specs, spec)
						}
					}
					if len(newDecl.Specs) > 0 {
						newDecls = append(newDecls, newDecl)
					}
				}
			case *ast.FuncDecl:
				name := decl.Name.Name
				if decl.Recv != nil {
					name = decl.Recv.List[0].Type.(*ast.Ident).Name + "." + name
				}
				if mutator, ok := existingDecls[name]; ok {
					mutator(decl)
				} else {
					newDecls = append(newDecls, decl)
				}
			}
		}
	}

	// get function dependencies
	deps := make(map[types.Object]ObjectSet)
	for _, decl := range newDecls {
		switch decl := decl.(type) {
		case *ast.FuncDecl:
			obj := info.ObjectOf(decl.Name)
			set := NewObjectSet()
			var visitor astVisitor
			visitor = func(node ast.Node) astVisitor {
				switch node := node.(type) {
				case *ast.Ident:
					dep := info.ObjectOf(node)
					set.Add(dep)
				}
				return visitor
			}
			ast.Walk(visitor, decl)
			deps[obj] = set
		}
	}

	// get uses objects
	uses := NewObjectSet()
	for _, use := range config.Uses {
		parts := strings.Split(use, ".")
		switch len(parts) {
		case 2: // method
			var ty types.Object
			if from, ok := renamed[parts[0]]; ok { // renamed type, use original type name
				ty = info.Pkg.Scope().Lookup(from)
			} else {
				ty = info.Pkg.Scope().Lookup(parts[0])
			}
			typeName, ok := ty.(*types.TypeName)
			if !ok {
				return fmt.Errorf("%s is not a type", parts[0])
			}
			obj, _, _ := types.LookupFieldOrMethod(typeName.Type(), true, info.Pkg, parts[1])
			uses.Add(obj)
		case 1: // non-method
			var obj types.Object
			if from, ok := renamed[parts[0]]; ok { // renamed function
				obj = info.Pkg.Scope().Lookup(from)
			} else {
				obj = info.Pkg.Scope().Lookup(parts[0])
			}
			uses.Add(obj)
		default:
			return fmt.Errorf("invalid use spec: %s", use)
		}
	}

	// filter by uses
	if len(uses) > 0 {
		// calculate uses closure
		for {
			l := len(uses)
			for use := range uses {
				if deps, ok := deps[use]; ok {
					for dep := range deps {
						uses.Add(dep)
					}
				}
			}
			if len(uses) == l {
				break
			}
		}
		// filter
		newDecls = filterDecls(newDecls, func(node interface{}) bool {
			switch node := node.(type) {
			case *ast.FuncDecl:
				return uses.In(info.ObjectOf(node.Name))
			case *ast.TypeSpec:
				return uses.In(info.ObjectOf(node.Name))
			case valueInfo:
				return uses.In(info.ObjectOf(node.Name))
			}
			return true
		})
	}

	// merge new and existing decls
	decls = append(decls, newDecls...)

	// decls tidy ups
	newDecls = newDecls[0:0]
	var importDecls []ast.Decl
	for _, decl := range decls {
		// ensure linebreak between decls
		switch decl := decl.(type) {
		case *ast.FuncDecl:
			if decl.Doc == nil {
				decl.Doc = new(ast.CommentGroup)
			}
		case *ast.GenDecl:
			if decl.Doc == nil {
				decl.Doc = new(ast.CommentGroup)
			}
		}
		// move import decls to beginning
		if decl, ok := decl.(*ast.GenDecl); ok && decl.Tok == token.IMPORT {
			importDecls = append(importDecls, decl)
			continue
		}
		newDecls = append(newDecls, decl)
	}
	decls = append(importDecls, newDecls...)

	// output
	if config.Writer != nil {
		if config.Package != "" { // output complete file
			file := &ast.File{
				Name:  ast.NewIdent(config.Package),
				Decls: decls,
			}
			buf := new(bytes.Buffer)
			err = format.Node(buf, program.Fset, file)
			if err != nil { //NOCOVER
				return fmt.Errorf("ccg: format output %v", err)
			}
			bs, err := imports.Process("", buf.Bytes(), nil)
			if err != nil { //NOCOVER
				return fmt.Errorf("ccg: format output %v", err)
			}
			config.Writer.Write(bs)
		} else { // output decls only
			err = format.Node(config.Writer, program.Fset, decls)
			if err != nil { //NOCOVER
				return fmt.Errorf("ccg: format output %v", err)
			}
		}
	}

	return nil
}

type astVisitor func(ast.Node) astVisitor

func (v astVisitor) Visit(node ast.Node) ast.Visitor {
	return v(node)
}

type valueInfo struct {
	Name  *ast.Ident
	Value ast.Expr
	Type  ast.Expr
}

func filterDecls(decls []ast.Decl, fn func(interface{}) bool) []ast.Decl {
	decls = AstDecls(decls).Filter(func(decl ast.Decl) (ret bool) {
		switch decl := decl.(type) {
		case *ast.FuncDecl:
			ret = fn(decl)
		case *ast.GenDecl:
			switch decl.Tok {
			case token.VAR, token.CONST:
				decl.Specs = AstSpecs(decl.Specs).Filter(func(sp ast.Spec) bool {
					spec := sp.(*ast.ValueSpec)
					names := []*ast.Ident{}
					values := []ast.Expr{}
					for i, name := range spec.Names {
						var value ast.Expr
						if i < len(spec.Values) {
							value = spec.Values[i]
						}
						if fn(valueInfo{name, value, spec.Type}) {
							names = append(names, name)
							if value != nil {
								values = append(values, value)
							}
						}
					}
					spec.Names = names
					if len(values) == 0 {
						spec.Values = nil
					} else {
						spec.Values = values
					}
					return len(spec.Names) > 0
				})
				ret = len(decl.Specs) > 0
			case token.TYPE, token.IMPORT:
				decl.Specs = AstSpecs(decl.Specs).Filter(func(spec ast.Spec) bool {
					return fn(spec)
				})
				ret = len(decl.Specs) > 0
			}
		}
		return
	})
	return decls
}
