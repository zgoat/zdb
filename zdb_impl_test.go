package zdb

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"zgo.at/zdb/testdata"
	"zgo.at/zstd/ztest"
)

func TestVersion(t *testing.T) {
	ctx := StartTest(t)
	db := MustGetDB(ctx)

	v, err := db.Version(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(v)

	for _, tt := range [][]Version{
		{"3", "4"},
		{"3.35.0", "4"},
		{"3.35.0", "4.1.0"},
		{"3.35.0", "3.35.1"},
	} {
		have, want := tt[0], tt[1]
		if have.AtLeast(want) {
			t.Errorf("is true: %s.AtLeast(%s)", have, want)
		}
	}

	for _, tt := range [][]Version{
		{"4.0.0", "4"},
		{"4.1.0", "4"},
		{"4.1", "4"},
		{"4.0.1", "4"},
	} {
		have, want := tt[0], tt[1]
		if !have.AtLeast(want) {
			t.Errorf("is false: %s.AtLeast(%s)", have, want)
		}
	}
}

func TestPrepare(t *testing.T) {
	date := time.Date(2020, 06, 18, 01, 02, 03, 04, time.UTC)

	tests := []struct {
		query string
		args  []interface{}

		wantQuery string
		wantArg   []interface{}
		wantErr   string
	}{
		// No arguments.
		{`select foo from bar`, nil,
			`select foo from bar`, nil, ""},

		// Single named param from map
		{`select :x`, L{P{"x": "Y"}},
			`select $1`, L{"Y"}, ""},

		// Single named param from struct
		{`select :x`, L{struct{ X string }{"Y"}},
			`select $1`, L{"Y"}, ""},

		// Both a map and struct → merge
		{`select :x, :y`, L{P{"x": "Y"}, struct{ Y int }{42}},
			`select $1, $2`, L{"Y", 42}, ""},

		// One positional
		{`select $1`, L{"A"},
			`select $1`, L{"A"}, ""},
		{`select ?`, L{"A"},
			`select $1`, L{"A"}, ""},

		// Two positional
		{`select $1, $2`, L{"A", "B"},
			`select $1, $2`, L{"A", "B"}, ""},
		{`select ?, ?`, L{"A", "B"},
			`select $1, $2`, L{"A", "B"}, ""},

		// time.Time shouldn't be seen as a named argument.
		{`select ?`, L{date},
			`select $1`, L{date}, ""},
		{`select ?, ?`, L{date, date},
			`select $1, $2`, L{date, date}, ""},

		// Neither should structs implementing sql.Scanner
		{`select ?`, L{sql.NullBool{Valid: true}},
			`select $1`, L{sql.NullBool{Valid: true}}, ""},
		{`select ?, ?`, L{sql.NullString{}, sql.NullString{}},
			`select $1, $2`, L{sql.NullString{}, sql.NullString{}}, ""},

		// True conditional from bool
		{`select {{:xxx cond}} where 1=1`, L{P{"xxx": true}},
			`select cond where 1=1`, L{}, ""},
		{`select {{:xxx cond}} where 1=1`, L{struct{ XXX bool }{true}},
			`select cond where 1=1`, L{}, ""},
		{`select {{:xxx cond}} where 1=1`, L{P{"a": true}, struct{ XXX bool }{true}},
			`select cond where 1=1`, L{}, ""},

		// Negation with !
		{`select {{:xxx! cond}} where 1=1`, L{P{"xxx": true}},
			`select  where 1=1`, L{}, ""},
		// Negation with !
		{`select {{:xxx! cond}} where 1=1`, L{P{"xxx": false}},
			`select cond where 1=1`, L{}, ""},

		// False conditional from bool
		{`select {{:xxx cond}} where 1=1`, L{P{"xxx": false}},
			`select  where 1=1`, L{}, ""},
		{`select {{:xxx cond}} where 1=1`, L{struct{ XXX bool }{false}},
			`select  where 1=1`, L{}, ""},
		{`select {{:xxx cond}} where 1=1`, L{P{"a": false}, struct{ XXX bool }{false}},
			`select  where 1=1`, L{}, ""},

		// Multiple conditionals
		{`select {{:a cond}} {{:b cond2}} `, L{P{"a": true, "b": true}},
			`select cond cond2 `, L{}, ""},
		{`select {{:a cond}} {{:b cond2}} `, L{P{"a": false, "b": false}},
			`select   `, L{}, ""},

		// Parameters inside conditionals
		{`select {{:a x like :foo}} {{:b y = :bar}}`, L{P{"foo": "qwe", "bar": "zxc", "a": true, "b": true}},
			`select x like $1 y = $2`, L{"qwe", "zxc"}, ""},
		{`select {{:a x like :foo}} {{:b y = :bar}}`, L{P{"foo": "qwe", "bar": "zxc", "a": false, "b": true}},
			`select  y = $1`, L{"zxc"}, ""},

		// Multiple conflicting params
		{`select :x`, L{P{"x": 1}, P{"x": 2}},
			``, nil, "more than once"},
		{`select {{:x cond}}`, L{P{"x": 1}, P{"x": 2}},
			``, nil, "more than once"},

		// Mixing positional and named
		{`select :x`, L{P{"x": 1}, 42},
			``, nil, "mix named and positional"},

		// Conditional not found
		{`select {{:x cond}}`, L{P{"z": 1}},
			``, nil, "could not find"},

		// Condtional with positional
		{`select {{:x cond}}`, L{"z", 1},
			`select {{:x cond}}`, L{"z", 1}, ""},

		// Invalid syntax for conditional; just leave it alone
		{`select {{cond}}`, L{P{"xxx": false}},
			`select {{cond}}`, L{}, ""},

		// Expand slice
		{`insert values (?)`, L{[]string{"a", "b"}},
			`insert values ($1, $2)`, L{"a", "b"}, ""},
		// TODO: this only works for "?"; sqlx.In() and named parameters.
		// {`insert values ($1)`, L{[]string{"a", "b"}},
		// 	`insert values ($1, $2)`, L{"a", "b"}, ""},
		{`insert values (:x)`, L{P{"x": []string{"a", "b"}}},
			`insert values ($1, $2)`, L{"a", "b"}, ""},
	}

	for _, tt := range tests {
		t.Run("", func(t *testing.T) {
			ctx := StartTest(t)

			query, args, err := Prepare(ctx, tt.query, tt.args...)
			query = sqlx.Rebind(sqlx.DOLLAR, query) // Always use $-binds for tests
			if !ztest.ErrorContains(err, tt.wantErr) {
				t.Fatal(err)
			}
			if query != tt.wantQuery {
				t.Errorf("wrong query\nout:  %q\nwant: %q", query, tt.wantQuery)
			}
			if !reflect.DeepEqual(args, tt.wantArg) {
				t.Errorf("wrong args\nout:  %#v\nwant: %#v", args, tt.wantArg)
			}
		})
	}
}

func TestPrepareDump(t *testing.T) {
	ctx := StartTest(t)

	err := Exec(ctx, `create table tbl (col1 varchar, col2 int);`)
	if err != nil {
		t.Fatal(err)
	}

	// Show just query.
	func() {
		defer func() { stderr = os.Stderr }()
		buf := new(bytes.Buffer)
		stderr = buf

		err = Exec(ctx, `insert into tbl values (:val, 1), {{:val2 (:val2, 2)}}`, map[string]interface{}{
			"val":  "hello",
			"val2": "world",
		}, DumpQuery)
		if err != nil {
			t.Fatal(err)
		}

		Dump(ctx, buf, `select * from tbl`)

		out := buf.String()
		want := "insert into tbl values ('hello', 1), ('world', 2);\n\ncol1   col2\nhello  1\nworld  2\n\n"
		if out != want {
			t.Errorf("wrong query\nout:  %q\nwant: %q", out, want)
		}
	}()

	// Show query and output.
	func() {
		defer func() { stderr = os.Stderr }()
		buf := new(bytes.Buffer)
		stderr = buf

		err = Exec(ctx, `select * from tbl where col1 = :val`, map[string]interface{}{
			"val": "hello",
		}, DumpResult)
		if err != nil {
			t.Fatal(err)
		}

		out := buf.String()
		want := "col1   col2\nhello  1\n\n"
		if out != want {
			t.Errorf("wrong query\nout:  %q\nwant: %q", out, want)
		}
	}()

	// Show explain
	func() {
		defer func() { stderr = os.Stderr }()
		buf := new(bytes.Buffer)
		stderr = buf

		err = Exec(ctx, `select * from tbl where col1 = :val`, map[string]interface{}{
			"val": "hello",
		}, DumpResult, DumpExplain)
		if err != nil {
			t.Fatal(err)
		}

		out := buf.String()
		want := `
			[1mEXPLAIN[0m:
			  SCAN TABLE tbl
			  Time: 0.016 ms
			[1mRESULT[0m:
			  col1   col2
			  hello  1`

		if Driver(ctx) == DriverPostgreSQL {
			want = `
			[1mEXPLAIN[0m:
			  Seq Scan on tbl  (cost=0.00..25.88 rows=6 width=36) (actual time=0.005..0.015 rows=1 loops=1)
			    Filter: ((col1)::text = 'hello'::text)
			  	Rows Removed by Filter: 1
			  Planning Time: 0.123 ms
			  Execution Time: 0.646 ms
			[1mRESULT[0m:
			  col1   col2
			  hello  1`
		}

		out, want = prep(ctx, out, want)

		if d := ztest.Diff(out, want, ztest.DiffNormalizeWhitespace); d != "" {
			t.Error(d)
		}
	}()
}

func prep(ctx context.Context, got, want string) (string, string) {
	re := []string{`([0-9]+.[0-9]+) ms`, `log_test\.go:(\d\d)`}
	if Driver(ctx) == DriverPostgreSQL {
		re = append(re, `(?:cost|time)=([0-9.]+)\.\.([0-9.]+) `)
	}

	got = ztest.Replace(got, re...)
	want = ztest.Replace(want, re...)
	return got, want

}

func TestInsertID(t *testing.T) {
	ctx := StartTest(t)

	tbl := `create table test (col_id integer primary key autoincrement, v varchar)`
	if Driver(ctx) == DriverPostgreSQL {
		tbl = `create table test (col_id serial primary key, v varchar)`
	}
	if Driver(ctx) == DriverMariaDB {
		tbl = `create table test (col_id integer auto_increment, v varchar(255), primary key(col_id))`
	}
	err := Exec(ctx, tbl, nil)
	if err != nil {
		t.Fatal(err)
	}

	{ // One row
		id, err := InsertID(ctx, `col_id`, `insert into test (v) values (:val)`, P{"val": "aa"})
		if err != nil {
			t.Error(err)
		}
		if id != 1 {
			t.Errorf("id is %d, not 1", id)
		}
	}

	{ // Multiple rows
		id, err := InsertID(ctx, `col_id`, `insert into test (v) values (:val), ('bb')`, P{"val": "aa"})
		if err != nil {
			t.Error(err)
		}
		if id != 3 {
			t.Errorf("id is %d, not 3\n%s", id, DumpString(ctx, `select * from test`))
		}
	}

	{
		id, err := InsertID(ctx, `col_id`, `insert into test (v) values (?), (?)`,
			"X", "Y")
		if err != nil {
			t.Error(err)
		}
		if id != 5 {
			t.Errorf("id is %d, not 5\n%s", id, DumpString(ctx, `select * from test`))
		}
	}

	{ // Invalid SQL

		id, err := InsertID(ctx, `col_id`, `insert into test (no_such_col) values ($1)`, nil)
		if err == nil {
			t.Error("err is nil")
		}
		if id != 0 {
			t.Errorf("id is not 0: %d", id)
		}
	}

	out := "\n" + DumpString(ctx, `select * from test`)
	want := `
col_id  v
1       aa
2       aa
3       bb
4       X
5       Y
`
	if out != want {
		t.Errorf("\nwant: %v\ngot:  %v", want, out)
	}
}

func TestQuery(t *testing.T) {
	ctx := StartTest(t)

	err := Exec(ctx, `
		create table tbl (
			s  varchar,
			i  int,
			t  timestamp,
			n  int null
		);
		insert into tbl values
			('Hello', 42,  '2020-06-18', null),
			('Hello', 42,  '2020-06-18', null),
			('Hello', 42,  '2020-06-18', null),
			('Hello', 42,  '2020-06-18', null);
	`)
	if err != nil {
		t.Fatal(err)
	}

	rows, err := Query(ctx, `select * from tbl`)
	if err != nil {
		t.Fatal(err)
	}

	i := 0
	for rows.Next() {
		switch i {
		case 0:
			var (
				s  string
				i  int
				ti time.Time
				n  *int
			)
			err := rows.Scan(&s, &i, &ti, &n)
			if err != nil {
				t.Fatal(err)
			}
			//fmt.Println("scan", s, i, ti, n)
		case 1:
			var r map[string]interface{}
			err := rows.Scan(&r)
			if err != nil {
				t.Fatal(err)
			}
			//fmt.Println("map", r)
		case 2:
			var r []interface{}
			err := rows.Scan(&r)
			if err != nil {
				t.Fatal(err)
			}
			//fmt.Println("slice", r)
		case 3:
			var r struct {
				S string
				I int
				T time.Time
				N *int
			}
			err := rows.Scan(&r)
			if err != nil {
				t.Fatal(err)
			}
			//fmt.Println("struct", r)
		}
		i++
	}
}

func TestLoad(t *testing.T) {
	// ctx := StartTest(t)

	// TODO: can't set Files from StartTest(); don't really want to add a
	// parameter for it. Would be nice if it could be set later?
	db, err := Connect(ConnectOptions{
		Connect: "sqlite3://:memory:",
		Files:   testdata.Files,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := WithDB(context.Background(), db)

	{
		got, err := Load(ctx, "select-1")
		if err != nil {
			t.Fatal(err)
		}
		want := "/* select-1 */\nselect * from t where col like :find\n"
		if got != want {
			t.Errorf("\ngot:  %q\nwant: %q", got, want)
		}
	}

	{
		for _, n := range []string{"comment", "comment.sql"} {
			got, err := Load(ctx, n)
			if err != nil {
				t.Fatal(err)
			}
			want := "/* comment */\nselect 1\n\nfrom x;  -- xx\n"
			if got != want {
				t.Errorf("\ngot:  %q\nwant: %q", got, want)
			}
		}
	}
}

func TestBegin(t *testing.T) {
	ctx := StartTest(t)

	txctx, tx, err := Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}

	err = tx.Rollback()
	if err != nil {
		t.Fatal(err)
	}

	t.Run("nested", func(t *testing.T) {
		txctx2, tx2, err := Begin(txctx)
		if err != ErrTransactionStarted {
			t.Fatal(err)
		}
		if tx2 != tx {
			t.Error("tx2 != tx")
		}
		if txctx2 != txctx {
			t.Error("txctx2 != txctx")
		}
	})
}

func TestTX(t *testing.T) {
	ctx := StartTest(t)

	err := TX(ctx, func(ctx context.Context) error {
		_, ok := MustGetDB(ctx).(*zTX)
		if !ok {
			t.Errorf("not a tx: %T", MustGetDB(ctx))
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("nested", func(t *testing.T) {
		err := TX(ctx, func(ctx context.Context) error {
			err := Exec(ctx, `create table test_tx (c varchar)`, nil)
			if err != nil {
				return err
			}
			err = Exec(ctx, `insert into test_tx values ('outer')`, nil)
			if err != nil {
				return err
			}

			return TX(ctx, func(ctx context.Context) error {
				err := Exec(ctx, `insert into test_tx values ('inner')`, nil)
				return err
			})
		})
		if err != nil {
			t.Fatal(err)
		}

		got := DumpString(ctx, `select * from test_tx`)
		want := "c\nouter\ninner\n"
		if got != want {
			t.Errorf("\ngot:  %q\nwant: %q", got, want)
		}
	})

	t.Run("nested_inner_error", func(t *testing.T) {
		Exec(ctx, `create table test_tx2 (c varchar)`, nil)
		err := TX(ctx, func(ctx context.Context) error {
			err := Exec(ctx, `insert into test_tx2 values ('outer')`, nil)
			if err != nil {
				return err
			}

			return TX(ctx, func(ctx context.Context) error {
				Exec(ctx, `insert into test_tx2 values ('inner')`, nil)
				return errors.New("oh noes")
			})
		})
		if err == nil {
			t.Fatal("err is nil")
		}

		got := DumpString(ctx, `select * from test_tx2`)
		want := "c\n"
		if got != want {
			t.Errorf("\ngot:  %q\nwant: %q", got, want)
		}
	})

	t.Run("nested_outer_error", func(t *testing.T) {
		Exec(ctx, `create table test_tx3 (c varchar)`, nil)

		err := TX(ctx, func(ctx context.Context) error {
			err := Exec(ctx, `insert into test_tx3 values ('outer')`, nil)
			if err != nil {
				return err
			}

			err = TX(ctx, func(ctx context.Context) error {
				Exec(ctx, `insert into test_tx3 values ('inner')`, nil)
				return nil
			})
			if err != nil {
				return err
			}

			return errors.New("oh noes")
		})
		if !ztest.ErrorContains(err, "oh noes") {
			t.Fatalf("wrong error: %v", err)
		}

		got := DumpString(ctx, `select * from test_tx3`)
		want := "c\n"
		if got != want {
			t.Errorf("\ngot:  %q\nwant: %q", got, want)
		}
	})
}

func TestPrepareIn(t *testing.T) {
	ctx := StartTest(t)

	tests := []struct {
		query  string
		params []interface{}
		want   string
	}{
		{``, nil, ` []interface {}(nil)`},
		{
			`select * from t where a=? and c in (?)`,
			[]interface{}{1, []string{"A", "B"}},
			`select * from t where a=? and c in (?, ?) []interface {}{1, "A", "B"}`,
		},
		{
			`select * from t where a=? and c in (?)`,
			[]interface{}{1, []int{1, 2}},
			`select * from t where a=? and c in (1, 2) []interface {}{1}`,
		},
		{
			`select * from t where a=? and c in (?)`,
			[]interface{}{1, []int64{1, 2}},
			`select * from t where a=? and c in (1, 2) []interface {}{1}`,
		},
		{
			`? ? ? ? ? ?`,
			[]interface{}{1, 2, 3, []int64{4}, 5, []int64{6}},
			`? ? ? 4 ? 6 []interface {}{1, 2, 3, 5}`,
		},

		// Note this is kinda wrong (or at least, unexpected), but this is how
		// sqlx.In() does it. There is no real way to know this is a []rune
		// rather than a []int32 :-/
		{
			`? (?) ?`,
			[]interface{}{[]byte("ABC"), []rune("ZXC"), "C"},
			`? (?, ?, ?) ? []interface {}{[]uint8{0x41, 0x42, 0x43}, 90, 88, 67, "C"}`,
		},
	}

	for _, tt := range tests {
		t.Run("", func(t *testing.T) {
			query, params, err := Prepare(ctx, tt.query, tt.params...)
			if err != nil {
				t.Fatal(err)
			}

			have := fmt.Sprintf("%s %#v", query, params)
			if have != tt.want {
				t.Errorf("\nhave: %#v\nwant: %#v", have, tt.want)
			}
		})
	}
}

func BenchmarkPrepare(b *testing.B) {
	query := `
		select foo from bar
		{{:join join x using (y)}}
		where site=:site and start=:start and end=:end
		{{:path and path like :path}}
		{{:psql returning id}}`
	arg := map[string]interface{}{
		"path":  "/XXXX",
		"site":  42,
		"start": "2020-01-01",
		"end":   "2020-05-05",
		"psql":  false,
		"join":  true,
	}

	db, err := Connect(ConnectOptions{
		Connect: "sqlite3://:memory:",
	})
	if err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		_, _, _ = Prepare(WithDB(context.Background(), db), query, arg)
	}
}

func BenchmarkLoad(b *testing.B) {
	db, err := Connect(ConnectOptions{
		Connect: "sqlite3://:memory:",
		Files:   testdata.Files,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	ctx := WithDB(context.Background(), db)

	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		_, _ = Load(ctx, "hit_list.GetTotalCount")
	}
}
