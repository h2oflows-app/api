// sqlcheck validates every SQL statement embedded in the Go source against a
// schema built from migrations/.
//
// Go does not typecheck embedded SQL, so a query referencing a column that a
// migration dropped compiles, passes vet, passes unit tests, and only fails
// when a real request runs it. That is not hypothetical: mig 000122 dropped
// gauges.reach_id while internal/handlers/gauges.go kept selecting it, and
// /api/v1/gauges/search and /api/v1/gauges/batch returned 500 in production
// for weeks before anyone noticed (api#189).
//
// How it works: parse each .go file, pull out every raw (backtick) string
// literal that looks like SQL, and PREPARE it. Postgres resolves all table and
// column references during PREPARE without executing anything, so a dropped
// column fails immediately and harmlessly.
//
// Errors are classified rather than treated alike:
//
//   - 42703 undefined_column / 42P01 undefined_table / 42883 undefined_function
//     → REAL FAILURE. This is the class that shipped api#189.
//   - 42P18 indeterminate_datatype ("could not determine data type of
//     parameter") → PASS. Parse and name resolution already succeeded; only
//     placeholder type inference failed, which is expected for $1 used without
//     a cast. A dropped column would have errored before this point.
//   - 42601 syntax_error → SKIPPED, not failed. Almost always a fmt.Sprintf
//     fragment whose %s/%d placeholders have not been substituted.
//
// Coverage is deliberately partial: only complete, single-literal statements
// can be checked. Queries assembled across several literals or built with
// Sprintf are counted and reported so the gap stays visible rather than
// reading as "everything passed".
//
// Usage:
//
//	DATABASE_URL=postgres://... sqlcheck [-v] [dir ...]
//
// Exits non-zero if any statement fails. Defaults to ./internal and ./cmd.
package main

import (
	"context"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type stmt struct {
	file string
	line int
	sql  string
}

func main() {
	verbose := flag.Bool("v", false, "list every statement checked")
	flag.Parse()

	dirs := flag.Args()
	if len(dirs) == 0 {
		dirs = []string{"internal", "cmd"}
	}

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		fmt.Fprintln(os.Stderr, "DATABASE_URL is required")
		os.Exit(2)
	}

	stmts, err := collect(dirs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "collect: %v\n", err)
		os.Exit(2)
	}

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect: %v\n", err)
		os.Exit(2)
	}
	defer conn.Close(ctx)

	var failures []string
	checked, skipped := 0, 0

	for i, s := range stmts {
		// Statements built with Sprintf cannot be prepared until their
		// placeholders are filled. Counted, never silently dropped.
		if strings.Contains(s.sql, "%s") || strings.Contains(s.sql, "%d") || strings.Contains(s.sql, "%v") {
			skipped++
			continue
		}

		name := fmt.Sprintf("sqlcheck_%d", i)
		_, err := conn.Prepare(ctx, name, s.sql)
		if err == nil {
			checked++
			if *verbose {
				fmt.Printf("ok    %s:%d\n", s.file, s.line)
			}
			continue
		}

		var pgErr *pgconn.PgError
		if !asPgError(err, &pgErr) {
			skipped++
			continue
		}

		switch pgErr.Code {
		case "42P18": // indeterminate_datatype — resolution succeeded, only $N typing failed
			checked++
			if *verbose {
				fmt.Printf("ok    %s:%d  (untyped parameter)\n", s.file, s.line)
			}
		case "42601": // syntax_error — almost certainly an unsubstituted fragment
			skipped++
			if *verbose {
				fmt.Printf("skip  %s:%d  (syntax — likely a Sprintf fragment)\n", s.file, s.line)
			}
		case "42703", "42P01", "42883": // undefined column / table / function
			failures = append(failures, fmt.Sprintf(
				"%s:%d\n    %s: %s\n    %s", s.file, s.line, pgErr.Code, pgErr.Message, firstLine(s.sql)))
		default:
			// Unknown classes are reported but do not fail the build — this
			// check is meant to catch schema drift, not to police every
			// dialect nuance it has not seen yet.
			skipped++
			if *verbose {
				fmt.Printf("skip  %s:%d  (%s: %s)\n", s.file, s.line, pgErr.Code, pgErr.Message)
			}
		}
	}

	fmt.Printf("\nsqlcheck: %d statements found, %d verified, %d skipped, %d FAILED\n",
		len(stmts), checked, skipped, len(failures))

	if len(failures) > 0 {
		fmt.Println("\nSchema mismatches — these reference columns, tables or functions that do not exist:")
		for _, f := range failures {
			fmt.Printf("\n  %s\n", f)
		}
		os.Exit(1)
	}
}

func asPgError(err error, target **pgconn.PgError) bool {
	for e := err; e != nil; {
		if pe, ok := e.(*pgconn.PgError); ok {
			*target = pe
			return true
		}
		un, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = un.Unwrap()
	}
	return false
}

func firstLine(sql string) string {
	for _, l := range strings.Split(sql, "\n") {
		t := strings.TrimSpace(l)
		if t != "" {
			if len(t) > 100 {
				t = t[:100] + "…"
			}
			return t
		}
	}
	return ""
}

// collect walks the given directories and returns every raw string literal
// that begins with a SQL verb.
func collect(dirs []string) ([]stmt, error) {
	var out []stmt
	fset := token.NewFileSet()

	for _, dir := range dirs {
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return fmt.Errorf("parse %s: %w", path, perr)
			}
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING || !strings.HasPrefix(lit.Value, "`") {
					return true
				}
				sql := strings.Trim(lit.Value, "`")
				if !looksLikeSQL(sql) {
					return true
				}
				out = append(out, stmt{
					file: path,
					line: fset.Position(lit.Pos()).Line,
					sql:  sql,
				})
				return true
			})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].file != out[j].file {
			return out[i].file < out[j].file
		}
		return out[i].line < out[j].line
	})
	return out, nil
}

// looksLikeSQL keeps literals whose first meaningful token is a statement
// verb. Leading comments are stripped first — many queries here open with a
// `-- …` line.
//
// Matching is on the first TOKEN, not on a "VERB " prefix. Queries in this
// codebase are commonly formatted with the verb alone on its own line:
//
//	SELECT
//	    t.start_cfs, …
//
// A prefix test for "SELECT " (with the trailing space) silently skips every
// one of those, which is how three broken trips.go queries went unreported on
// the first version of this tool.
func looksLikeSQL(s string) bool {
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "--") {
			continue
		}
		first, _, _ := strings.Cut(strings.ToUpper(t), " ")
		switch strings.TrimSuffix(first, "(") {
		case "SELECT", "INSERT", "UPDATE", "DELETE", "WITH":
			return true
		}
		return false
	}
	return false
}
