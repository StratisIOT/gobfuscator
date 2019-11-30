package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"golang.org/x/tools/refactor/importgraph"
	"golang.org/x/tools/refactor/rename"
)

var IgnoreMethods = map[string]bool{"main": true, "init": true}

type symbolRenameReq struct {
	OldName string
	NewName string
}

func ObfuscateSymbols(gopath string, n NameHasher) error {
	removeDoNotEdit(gopath)

	reverseLookupMap := map[string]string{}

	renames, err := topLevelRenames(gopath, n)
	if err != nil {
		return fmt.Errorf("top-level renames: %s", err)
	}
	if err := runRenames(gopath, renames, reverseLookupMap); err != nil {
		return fmt.Errorf("top-level renaming: %s", err)
	}
	renames, err = methodRenames(gopath, n)
	if err != nil {
		return fmt.Errorf("method renames: %s", err)
	}
	if err := runRenames(gopath, renames, reverseLookupMap); err != nil {
		return fmt.Errorf("method renaming: %s", err)
	}

	renames, err = topLevelParamRenames(gopath, n)
	if err != nil {
		return fmt.Errorf("top-level renames: %s", err)
	}
	if err := runRenames(gopath, renames, reverseLookupMap); err != nil {
		return fmt.Errorf("top-level renaming: %s", err)
	}

	jsonString, _ := json.Marshal(reverseLookupMap)
	fileErr := ioutil.WriteFile(gopath+"/"+"map.json", jsonString, 0777)
	if fileErr != nil {
		// TODO: Consider this case.
		return nil
	}

	return nil
}

func runRenames(gopath string, renames []symbolRenameReq, m map[string]string) error {
	ctx := build.Default
	ctx.GOPATH = gopath

	for _, r := range renames {
		fmt.Println(r.OldName)
		fmt.Println(r.NewName)

		oldName := strings.Split(r.OldName, ".")[1]

		if len(strings.Split(oldName, "::")) > 1 {
			oldName = strings.Split(oldName, "::")[1]
		}
		if len(strings.Split(r.OldName, ".")) == 3 {
			oldName = strings.Split(r.OldName, ".")[1] + "." + strings.Split(r.OldName, ".")[2]
		}
		m[r.NewName] = oldName
		if err := rename.Main(&ctx, "", r.OldName, r.NewName); err != nil {
			log.Println("Error running renames proceding...", err)
			continue
		}
	}

	return nil
}

func topLevelParamRenames(gopath string, n NameHasher) ([]symbolRenameReq, error) {
	srcDir := filepath.Join(gopath, "src")
	res := map[symbolRenameReq]int{}
	err := filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() && containsUnsupportedCode(path) {
			return filepath.SkipDir
		}
		if !isGoFile(path) {
			return nil
		}
		pkgPath, err := filepath.Rel(srcDir, filepath.Dir(path))
		if err != nil {
			return err
		}
		set := token.NewFileSet()
		file, err := parser.ParseFile(set, path, nil, 0)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if !IgnoreMethods[d.Name.Name] && d.Recv == nil {
					for _, field := range d.Type.Params.List {
						for _, name := range field.Names {
							prefix := "\"" + pkgPath + "\"."
							oldName := prefix + d.Name.Name + "::" + name.Name
							newName := n.Hash(name.Name)
							res[symbolRenameReq{oldName, newName}]++
						}
					}
				}
			}
		}
		return nil
	})
	return singleRenames(res), err
}

func topLevelRenames(gopath string, n NameHasher) ([]symbolRenameReq, error) {
	srcDir := filepath.Join(gopath, "src")
	res := map[symbolRenameReq]int{}
	addRes := func(pkgPath, name string) {
		prefix := "\"" + pkgPath + "\"."
		oldName := prefix + name
		newName := n.Hash(name)
		res[symbolRenameReq{oldName, newName}]++
	}
	err := filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() && containsUnsupportedCode(path) {
			return filepath.SkipDir
		}
		if !isGoFile(path) {
			return nil
		}
		pkgPath, err := filepath.Rel(srcDir, filepath.Dir(path))
		if err != nil {
			return err
		}
		set := token.NewFileSet()
		file, err := parser.ParseFile(set, path, nil, 0)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if !IgnoreMethods[d.Name.Name] && d.Recv == nil {
					addRes(pkgPath, d.Name.Name)
				}

			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch spec := spec.(type) {
					case *ast.TypeSpec:
						if reflect.TypeOf(spec.Type).String() == "*ast.StructType" {
							structType := spec.Type.(*ast.StructType)
							for _, f := range structType.Fields.List {
								for _, n := range f.Names {
									addRes(pkgPath, spec.Name.Name+"."+n.Name)
								}
							}
						}
						addRes(pkgPath, spec.Name.Name)
					case *ast.ValueSpec:
						for _, name := range spec.Names {
							addRes(pkgPath, name.Name)
						}
					}
				}
			}
		}
		return nil
	})
	return singleRenames(res), err
}

func methodRenames(gopath string, n NameHasher) ([]symbolRenameReq, error) {
	exclude, err := interfaceMethods(gopath)
	if err != nil {
		return nil, err
	}

	srcDir := filepath.Join(gopath, "src")
	res := map[symbolRenameReq]int{}
	err = filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() && containsUnsupportedCode(path) {
			return filepath.SkipDir
		}
		if !isGoFile(path) {
			return nil
		}
		pkgPath, err := filepath.Rel(srcDir, filepath.Dir(path))
		if err != nil {
			return err
		}
		set := token.NewFileSet()
		file, err := parser.ParseFile(set, path, nil, 0)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			d, ok := decl.(*ast.FuncDecl)

			if !ok || exclude[d.Name.Name] || d == nil {
				continue
			}

			if d.Body != nil {
				prefix := "\"" + pkgPath + "\"."
				for _, rec := range d.Body.List {
					var typeName = reflect.TypeOf(rec).String()
					if typeName == "*ast.AssignStmt" {
						var statement = rec.(*ast.AssignStmt)
						for _, ident := range statement.Lhs {
							if reflect.TypeOf(rec).String() == "*ast.Ident" {
								var name = ident.(*ast.Ident)
								if name.Name != "_" {
									oldName := prefix + d.Name.Name + "::" + name.Name
									newName := n.Hash(name.Name)
									res[symbolRenameReq{oldName, newName}]++
								}
							}
						}
					} else if typeName == "*ast.DeclStmt" {
						var statement = rec.(*ast.DeclStmt)
						var declaration = statement.Decl.(*ast.GenDecl)
						for _, spec := range declaration.Specs {
							var valueSpec = spec.(*ast.ValueSpec)
							for _, name := range valueSpec.Names {
								if name.Name != "_" {
									oldName := prefix + d.Name.Name + "::" + name.Name
									newName := n.Hash(name.Name)
									res[symbolRenameReq{oldName, newName}]++
								}
							}
						}
					} else if typeName == "*ast.ReturnStmt" {
						var statement = rec.(*ast.ReturnStmt)
						var results = statement.Results
						for _, expr := range results {
							// Ignore ast.BinaryExpr which is most likely created by this library as a string function.
							if reflect.TypeOf(expr).String() == "*ast.Ident" {
								var name = expr.(*ast.Ident)
								if name.Obj != nil {
									oldName := prefix + d.Name.Name + "::" + name.Name
									newName := n.Hash(name.Name)
									res[symbolRenameReq{oldName, newName}]++
								}
							} else {
								fmt.Println("%%%%%%%%%%%%%%%%%%%%%%%%%%%%%%%%%%%%%%%%%%%%%")
								fmt.Println(reflect.TypeOf(expr))
								fmt.Println(expr)
								fmt.Println("%%%%%%%%%%%%%%%%%%%%%%%%%%%%%%%%%%%%%%%%%%%%%")
							}
						}
					} else {
						fmt.Println("***********************************************")
						fmt.Println(reflect.TypeOf(rec))
						fmt.Println(rec)
						fmt.Println("***********************************************")
					}
				}
			}

			if d.Recv != nil {
				prefix := "\"" + pkgPath + "\"."
				for _, rec := range d.Recv.List {
					receiver := receiverString(prefix, rec)
					if receiver == "" {
						continue
					}
					oldName := receiver + "." + d.Name.Name
					newName := n.Hash(d.Name.Name)
					res[symbolRenameReq{oldName, newName}]++
				}
			}
		}
		return nil
	})
	return singleRenames(res), err
}

func interfaceMethods(gopath string) (map[string]bool, error) {
	ctx := build.Default
	ctx.GOPATH = gopath
	forward, backward, _ := importgraph.Build(&ctx)
	pkgs := map[string]bool{}
	for _, m := range []importgraph.Graph{forward, backward} {
		for x := range m {
			pkgs[x] = true
		}
	}
	res := map[string]bool{}
	for pkgName := range pkgs {
		pkg, err := ctx.Import(pkgName, gopath, 0)
		if err != nil {
			return nil, fmt.Errorf("import %s: %s", pkgName, err)
		}
		for _, fileName := range pkg.GoFiles {
			sourcePath := filepath.Join(pkg.Dir, fileName)
			set := token.NewFileSet()
			file, err := parser.ParseFile(set, sourcePath, nil, 0)
			if err != nil {
				return nil, err
			}
			for _, decl := range file.Decls {
				d, ok := decl.(*ast.GenDecl)
				if !ok {
					continue
				}
				for _, spec := range d.Specs {
					spec, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					t, ok := spec.Type.(*ast.InterfaceType)
					if !ok {
						continue
					}
					for _, field := range t.Methods.List {
						for _, name := range field.Names {
							res[name.Name] = true
						}
					}
				}
			}
		}
	}
	return res, nil
}

// singleRenames removes any rename requests which appear
// more than one time.
// This is necessary because of build constraints, which
// the refactoring API doesn't seem to properly support.
func singleRenames(multiset map[symbolRenameReq]int) []symbolRenameReq {
	var res []symbolRenameReq
	for x, count := range multiset {
		if count == 1 {
			res = append(res, x)
		}
	}

	// Ensure depth first renaming.
	sort.Slice(res, func(i, j int) bool {
		return strings.Count(res[i].OldName, ".") > strings.Count(res[j].OldName, ".")
	})
	return res
}

// containsUnsupportedCode checks if a source directory
// contains assembly or CGO code, neither of which are
// supported by the refactoring API.
func containsUnsupportedCode(dir string) bool {
	return containsAssembly(dir) || containsCGO(dir) || strings.Contains(dir, "github.com")
}

// containsAssembly checks if a source directory contains
// any assembly files.
// We cannot rename symbols in assembly-filled directories
// because of limitations of the refactoring API.
func containsAssembly(dir string) bool {
	contents, _ := ioutil.ReadDir(dir)
	for _, item := range contents {
		if filepath.Ext(item.Name()) == ".s" {
			return true
		}
	}
	return false
}

// containsCGO checks if a package relies on CGO.
// We cannot rename symbols in packages that use CGO due
// to limitations of the refactoring API.
func containsCGO(dir string) bool {
	listing, err := ioutil.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, item := range listing {
		if isGoFile(item.Name()) {
			path := filepath.Join(dir, item.Name())
			set := token.NewFileSet()
			file, err := parser.ParseFile(set, path, nil, 0)
			if err != nil {
				return false
			}
			for _, spec := range file.Imports {
				if spec.Path.Value == `"C"` {
					return true
				}
			}
		}
	}
	return false
}

// removeDoNotEdit removes comments that prevent gorename
// from working properly.
func removeDoNotEdit(dir string) error {
	srcDir := filepath.Join(dir, "src")
	return filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !isGoFile(path) {
			return nil
		}

		f, err := os.OpenFile(path, os.O_RDWR, 0755)
		if err != nil {
			return err
		}
		defer f.Close()

		content, err := ioutil.ReadAll(f)
		if err != nil {
			return err
		}

		set := token.NewFileSet()
		file, err := parser.ParseFile(set, path, content, parser.ParseComments)
		if err != nil {
			return err
		}

		for _, comment := range file.Comments {
			data := make([]byte, comment.End()-comment.Pos())
			start := int(comment.Pos())
			end := start + len(data)
			data = content[start:end]
			commentStr := string(data)
			if strings.Contains(commentStr, "DO NOT EDIT") {
				commentStr = strings.Replace(commentStr, "DO NOT EDIT", "XXXXXXXXXXX", -1)
				if _, err := f.WriteAt([]byte(commentStr), int64(comment.Pos()-1)); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// receiverString gets the string representation of a
// method receiver so that the method can be renamed.
func receiverString(prefix string, rec *ast.Field) string {
	if stringer, ok := rec.Type.(fmt.Stringer); ok {
		return prefix + stringer.String()
	} else if star, ok := rec.Type.(*ast.StarExpr); ok {
		if stringer, ok := star.X.(fmt.Stringer); ok {
			return "(*" + prefix + stringer.String() + ")"
		}
	}
	return ""
}
