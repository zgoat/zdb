package zdb

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"strings"
	"testing"
	"text/tabwriter"
	"text/template"
	"time"

	"zgo.at/zstd/zbyte"
	"zgo.at/zstd/zdebug"
	"zgo.at/zstd/ztest"
	"zgo.at/zstd/ztime"
)

type DumpArg int32

func (d DumpArg) has(flag DumpArg) bool { return d&flag != 0 }

func (d *DumpArg) extract(params []interface{}) []interface{} {
	var newParams = make([]interface{}, 0, len(params))
	for _, p := range params {
		b, ok := p.(DumpArg)
		if !ok {
			newParams = append(newParams, p)
			continue
		}

		if b.has(DumpAll) {
			*d |= DumpLocation | DumpQuery | DumpExplain | DumpResult
			continue
		}
		*d |= b
	}

	if !d.has(DumpLocation) && !d.has(DumpQuery) && !d.has(DumpExplain) && !d.has(DumpResult) {
		*d |= DumpResult
	}

	return newParams
}

const (
	dumpFromLogDB DumpArg = 1 << iota

	DumpLocation // Show location of Dump call.
	DumpQuery    // Show the query with placeholders substituted.
	DumpExplain  // Show the results of EXPLAIN (or EXPLAIN ANALYZE for PostgreSQL).
	DumpResult   // Show the query result.
	DumpVertical // Print query result in vertical columns instead of horizontal.
	DumpCSV      // Print query result as CSV.
	DumpJSON     // Print query result as JSON.
	DumpHTML     // Print query result as a HTML table.
	DumpAll      // Dump all we can.
)

// Dump the results of a query to a writer in an aligned table. This is a
// convenience function intended mostly for testing/debugging.
//
// Combined with ztest.Diff() it can be an easy way to test the database state.
//
// You can add some special sentinel values in the params to control the output
// (they're not sent as parameters to the DB):
//
//   DumpAll
//   DumpLocation   Show location of the Dump() cal.
//   DumpQuery      Show the query with placeholders substituted.
//   DumpExplain    Show the results of EXPLAIN (or EXPLAIN ANALYZE for PostgreSQL).
//   DumpResult     Show the query result (
//   DumpVertical   Show vertical output instead of horizontal columns.
//   DumpCSV        Show as CSV.
//   DumpJSON       Show as an array of JSON objects.
//   DumpHTML       Show as a HTML table.
func Dump(ctx context.Context, out io.Writer, query string, params ...interface{}) {
	var dump DumpArg
	params = dump.extract(params)

	var nsections int
	if dump.has(DumpQuery) {
		nsections++
	}
	if dump.has(DumpExplain) {
		nsections++
	}
	if dump.has(DumpResult) {
		nsections++
	}

	var (
		bold    = func(s string) string { return "\x1b[1m" + s + "\x1b[0m" }
		indent  = func(s string) string { return "  " + strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n  ") }
		section = func(name, s string) {
			r := strings.TrimRight(s, "\n")
			if nsections > 1 {
				r = bold(name) + ":\n" + indent(s)
			}
			if dump.has(DumpLocation) {
				r = indent(r)
			}
			fmt.Fprintln(out, r)
		}
	)

	if dump.has(DumpLocation) {
		if dump.has(dumpFromLogDB) {
			fmt.Fprintf(out, "zdb.LogDB: %s\n", bold(zdebug.Loc(5)))
		} else {
			fmt.Fprintf(out, "zdb.Dump: %s\n", bold(zdebug.Loc(4)))
		}
	}

	if dump.has(DumpQuery) {
		section("QUERY", ApplyParams(query, params...))
	}

	if dump.has(DumpExplain) {
		var (
			explain []string
			err     error
		)
		switch Driver(ctx) {
		default:
			err = errors.New("zdb.LogDB: unsupported driver for LogExplain " + MustGetDB(ctx).DriverName())
		case DriverPostgreSQL:
			err = Select(ctx, &explain, `explain analyze `+query, params...)
		case DriverMariaDB:
			// TODO
		case DriverSQLite:
			var sqe []struct {
				ID, Parent, Notused int
				Detail              string
			}
			t := ztime.Takes(func() {
				err = Select(ctx, &sqe, `explain query plan `+query, params...)
			})
			if len(sqe) > 0 {
				explain = make([]string, len(sqe)+1)
				for i := range sqe {
					explain[i] = sqe[i].Detail
				}
				explain[len(sqe)] = "Time: " + ztime.DurationAs(t.Round(time.Microsecond), time.Millisecond) + " ms"
			}
		}
		if err != nil {
			section("EXPLAIN", err.Error())
		} else {
			section("EXPLAIN", strings.Join(explain, "\n"))
		}
	}

	if dump.has(DumpResult) {
		buf := new(bytes.Buffer)
		err := func() error {
			rows, err := Query(ctx, query, params...)
			if err != nil {
				return err
			}
			cols, err := rows.Columns()
			if err != nil {
				return err
			}

			switch {
			default:
				return dumpHorizontal(buf, rows, cols)
			case dump.has(DumpVertical):
				return dumpVertical(buf, rows, cols)
			case dump.has(DumpCSV):
				return dumpCSV(buf, rows, cols)
			case dump.has(DumpJSON):
				return dumpJSON(buf, rows, cols)
			case dump.has(DumpHTML):
				return dumpHTML(buf, rows, cols)
			}
		}()
		if err != nil {
			section("RESULT", err.Error())
		} else {
			section("RESULT", buf.String())
		}
	}

	fmt.Fprintln(out)
}

func dumpHorizontal(buf io.Writer, rows *Rows, cols []string) error {
	t := tabwriter.NewWriter(buf, 4, 4, 2, ' ', 0)
	t.Write([]byte(strings.Join(cols, "\t") + "\n"))

	for rows.Next() {
		var row []interface{}
		err := rows.Scan(&row)
		if err != nil {
			return err
		}

		for i, c := range row {
			t.Write([]byte(fmt.Sprintf("%v", formatParam(c, false))))
			if i < len(row)-1 {
				t.Write([]byte("\t"))
			}
		}
		t.Write([]byte("\n"))
	}
	return t.Flush()
}

func dumpVertical(buf io.Writer, rows *Rows, cols []string) error {
	t := tabwriter.NewWriter(buf, 4, 4, 2, ' ', 0)

	for rows.Next() {
		var row []interface{}
		err := rows.Scan(&row)
		if err != nil {
			return err
		}

		for i, c := range row {
			t.Write([]byte(fmt.Sprintf("%s\t%v\n", cols[i], formatParam(c, false))))
		}
		t.Write([]byte("\n"))
	}
	return t.Flush()
}

func dumpCSV(buf io.Writer, rows *Rows, cols []string) error {
	cf := csv.NewWriter(buf)
	err := cf.Write(cols)
	if err != nil {
		return err
	}

	for rows.Next() {
		var row []interface{}
		err := rows.Scan(&row)
		if err != nil {
			return err
		}
		rr := make([]string, 0, len(row))
		for _, c := range row {
			rr = append(rr, formatParam(c, false))
		}
		err = cf.Write(rr)
		if err != nil {
			return err
		}
	}
	cf.Flush()
	return cf.Error()
}

func dumpJSON(buf *bytes.Buffer, rows *Rows, cols []string) error {
	var j []map[string]interface{}
	for rows.Next() {
		var row []interface{}
		err := rows.Scan(&row)
		if err != nil {
			return err
		}
		obj := make(map[string]interface{})
		for i, c := range row {
			obj[cols[i]] = c
		}
		j = append(j, obj)
	}

	out, err := json.MarshalIndent(j, "", "\t")
	if err != nil {
		return err
	}
	buf.Write(out)
	return nil
}

func dumpHTML(buf *bytes.Buffer, rows *Rows, cols []string) error {
	buf.WriteString("<table><thead><tr>\n")
	for _, c := range cols {
		buf.WriteString("  <th>")
		buf.WriteString(template.HTMLEscapeString(c))
		buf.WriteString("</th>\n")
	}
	buf.WriteString("</tr></thead><tbody>\n")

	for rows.Next() {
		var row []interface{}
		err := rows.Scan(&row)
		if err != nil {
			return err
		}

		buf.WriteString("<tr>\n")
		for _, r := range row {
			buf.WriteString("  <td>")
			buf.WriteString(template.HTMLEscapeString(formatParam(r, false)))
			buf.WriteString("</td>\n")
		}
		buf.WriteString("</tr>\n")
	}

	buf.WriteString("</tbody></table>\n")

	// out, err := json.MarshalIndent(j, "", "\t")
	// if err != nil {
	// 	return err
	// }
	// buf.Write(out)
	return nil
}

// DumpString is like Dump(), but returns the result as a string.
func DumpString(ctx context.Context, query string, params ...interface{}) string {
	b := new(bytes.Buffer)
	Dump(ctx, b, query, params...)
	return strings.TrimSpace(b.String()) + "\n"
}

// ApplyParams replaces parameter placeholders in query with the values.
//
// This is ONLY for printf-debugging, and NOT for actual usage. Security was NOT
// a consideration when writing this. Parameters in SQL are sent separately over
// the write and are not interpolated, so it's very different.
//
// This supports ? placeholders and $1 placeholders *in order* ($\d+ is simply
// replace with ?).
func ApplyParams(query string, params ...interface{}) string {
	query = regexp.MustCompile(`\$\d+`).ReplaceAllString(query, "?")
	for _, p := range params {
		query = strings.Replace(query, "?", formatParam(p, true), 1)
	}
	query = deIndent(query)
	if !strings.HasSuffix(query, ";") {
		return query + ";"
	}
	return query
}

func formatParam(a interface{}, quoted bool) string {
	if a == nil {
		return "NULL"
	}
	switch aa := a.(type) {
	case *string:
		if aa == nil {
			return "NULL"
		}
		a = *aa
	case *int:
		if aa == nil {
			return "NULL"
		}
		a = *aa
	case *int64:
		if aa == nil {
			return "NULL"
		}
		a = *aa
	case *time.Time:
		if aa == nil {
			return "NULL"
		}
		a = *aa
	}

	switch aa := a.(type) {
	case time.Time:
		// TODO: be a bit smarter about the precision, e.g. a date or time
		// column doesn't need the full date.
		return formatParam(aa.Format("2006-01-02 15:04:05"), quoted)
	case int, int64:
		return fmt.Sprintf("%v", aa)
	case []byte:
		if zbyte.Binary(aa) {
			return fmt.Sprintf("%x", aa)
		} else {
			return formatParam(string(aa), quoted)
		}
	case string:
		if quoted {
			return fmt.Sprintf("'%v'", strings.ReplaceAll(aa, "'", "''"))
		}
		return aa
	default:
		if quoted {
			return fmt.Sprintf("'%v'", aa)
		}
		return fmt.Sprintf("%v", aa)
	}
}

func deIndent(in string) string {
	// Ignore comment at the start for indentation as I often write:
	//     SelectContext(`/* Comment for PostgreSQL logs */
	//             select [..]
	//     `)
	in = strings.TrimLeft(in, "\n\t ")
	comment := 0
	if strings.HasPrefix(in, "/*") {
		comment = strings.Index(in, "*/")
	}

	indent := 0
	for _, c := range strings.TrimLeft(in[comment+2:], "\n") {
		if c != '\t' {
			break
		}
		indent++
	}

	r := ""
	for _, line := range strings.Split(in, "\n") {
		r += strings.Replace(line, "\t", "", indent) + "\n"
	}

	return strings.TrimSpace(r)
}

// Diff two strings, ignoring whitespace at the start of a line.
//
// This is useful in tests in combination with zdb.Dump():
//
//     got := DumpString(ctx, `select * from factions`)
//     want := `
//         faction_id  name
//         1           Peacekeepers
//         2           Moya`
//     if d := Diff(got, want); d != "" {
//        t.Error(d)
//     }
//
// It normalizes the leading whitespace in want, making "does my database match
// with what's expected?" fairly easy to test.
func Diff(out, want string) string {
	return ztest.Diff(out, want, ztest.DiffNormalizeWhitespace)
}

// TestQueries tests queries in the db/query directory.
//
// for every .sql file you can create a _test.sql file, similar to how Go's
// testing works; the following special comments are recognized:
//
//    -- params     Parameters for the query.
//    -- want       Expected result.
//
// Everything before the first special comment is run as a "setup". The
// "-- params" and "-- want" comments can be repeated for multiple tests.
//
// Example:
//
//    db/query/select-sites.sql:
//       select * from sites where site_id = :site and created_at > :start
//
//    db/query/select-sites_test.sql
//      insert into sites () values (...)
//
//      -- params
//      site_id:    1
//      created_at: 2020-01-01
//
//      -- want
//      1
//
//      -- params
//
//      -- want
func TestQueries(t *testing.T, files fs.FS) {
	t.Helper()

	// TODO
}
