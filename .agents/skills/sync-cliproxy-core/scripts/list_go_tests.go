package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

func main() {
	filePath := flag.String("file", "", "parse a Go source file")
	stdinName := flag.String("stdin-name", "", "parse Go source from stdin using this display name")
	flag.Parse()

	if (*filePath == "") == (*stdinName == "") {
		fmt.Fprintln(os.Stderr, "exactly one of -file or -stdin-name is required")
		os.Exit(2)
	}

	name := *filePath
	var source any
	if *stdinName != "" {
		name = *stdinName
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read stdin: %v\n", err)
			os.Exit(1)
		}
		source = data
	}

	parsed, err := parser.ParseFile(token.NewFileSet(), name, source, parser.SkipObjectResolution)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse %s: %v\n", name, err)
		os.Exit(1)
	}

	names := make([]string, 0)
	for _, declaration := range parsed.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Recv != nil || !isGoTestSymbol(function.Name.Name) {
			continue
		}
		names = append(names, function.Name.Name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Println(name)
	}
}

func isGoTestSymbol(name string) bool {
	for _, prefix := range []string{"Test", "Fuzz", "Example"} {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		rest := strings.TrimPrefix(name, prefix)
		if rest == "" {
			return true
		}
		next, _ := utf8.DecodeRuneInString(rest)
		return !unicode.IsLower(next)
	}
	return false
}
