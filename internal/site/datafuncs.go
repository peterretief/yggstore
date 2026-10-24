package site

import (
	"crypto/sha256"
	"errors"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/ncruces/go-sqlite3"
)

// What a site's SQL sees as the time and as chance while entries are
// replayed (see data.go). Every web node replays the same entries, and
// replays them all again when an entry comes late or the SQL changes, so
// what the SQL does may depend only on the entries. SQLite's own 'now'
// and random() don't, so on read.db's writing connections they're
// replaced: 'now' (and CURRENT_TIMESTAMP, datetime() and the rest) is the
// entry's own time, and random() and randomblob() are drawn from the
// entry's id. 'localtime' and 'utc' would depend on each web node's time
// zone, so they turn the entry down. Queries keep SQLite's own functions.

// dateFuncs are SQLite's date and time functions, which take 'now' or no
// time at all for the time now.
var dateFuncs = []string{"date", "time", "datetime", "julianday", "unixepoch", "strftime", "timediff"}

// replayEnv is a site's replay: the entry being applied, and SQLite's own
// date functions, in a connection of their own, for the replacements to
// hand on to.
type replayEnv struct {
	now int64 // the entry's time, unix ms; 0 between entries
	rng *rand.ChaCha8

	mu    sync.Mutex // own and its statements
	own   *sqlite3.Conn
	stmts map[string]*sqlite3.Stmt
}

// enter is for an entry's statements; enterSchema for schema.sql's, which
// have no time of their own.
func (r *replayEnv) enter(e Entry) { r.start(e.Time, "entry "+e.ID) }

func (r *replayEnv) enterSchema() { r.start(0, "schema") }

func (r *replayEnv) start(now int64, seed string) {
	r.now, r.rng = now, rand.NewChaCha8(sha256.Sum256([]byte(seed)))
}

func (r *replayEnv) leave() { r.now, r.rng = 0, nil }

func (r *replayEnv) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.stmts {
		s.Close()
	}
	if r.own != nil {
		r.own.Close()
	}
	r.own, r.stmts = nil, nil
}

// register puts the replacements on one of read.db's writing connections.
func (r *replayEnv) register(c *sqlite3.Conn) error {
	for _, name := range dateFuncs {
		if err := c.CreateFunction(name, -1, sqlite3.DETERMINISTIC|sqlite3.INNOCUOUS, func(ctx sqlite3.Context, arg ...sqlite3.Value) {
			r.date(ctx, name, arg)
		}); err != nil {
			return err
		}
	}
	for kw, name := range map[string]string{"current_timestamp": "datetime", "current_date": "date", "current_time": "time"} {
		if err := c.CreateFunction(kw, 0, sqlite3.DETERMINISTIC|sqlite3.INNOCUOUS, func(ctx sqlite3.Context, _ ...sqlite3.Value) {
			r.date(ctx, name, nil)
		}); err != nil {
			return err
		}
	}
	if err := c.CreateFunction("random", 0, sqlite3.INNOCUOUS, func(ctx sqlite3.Context, _ ...sqlite3.Value) {
		if r.rng == nil {
			ctx.ResultError(errors.New("random() is only for an entry's statements"))
			return
		}
		ctx.ResultInt64(int64(r.rng.Uint64()))
	}); err != nil {
		return err
	}
	return c.CreateFunction("randomblob", 1, sqlite3.INNOCUOUS, func(ctx sqlite3.Context, arg ...sqlite3.Value) {
		if r.rng == nil {
			ctx.ResultError(errors.New("randomblob() is only for an entry's statements"))
			return
		}
		b := make([]byte, min(max(arg[0].Int64(), 1), 1<<20))
		for i := range b {
			b[i] = byte(r.rng.Uint64())
		}
		ctx.ResultBlob(b)
	})
}

// date is SQLite's own date function name, with the entry's time for the
// time now.
func (r *replayEnv) date(ctx sqlite3.Context, name string, arg []sqlite3.Value) {
	args := make([]any, 0, len(arg)+1)
	usesNow := len(arg) == 0 || name == "strftime" && len(arg) == 1
	for _, a := range arg {
		switch a.Type() {
		case sqlite3.INTEGER:
			args = append(args, a.Int64())
		case sqlite3.FLOAT:
			args = append(args, a.Float())
		case sqlite3.TEXT:
			s := a.Text()
			switch strings.ToLower(s) {
			case "now":
				usesNow = true
				args = append(args, nil) // filled in below
				continue
			case "localtime", "utc":
				ctx.ResultError(errors.New("'" + s + "' depends on each web node's time zone: keep times in UTC"))
				return
			}
			args = append(args, s)
		case sqlite3.BLOB:
			args = append(args, append([]byte(nil), a.RawBlob()...))
		default:
			args = append(args, nil)
		}
	}
	if usesNow {
		if r.now == 0 {
			ctx.ResultError(errors.New("the time now only has a meaning in an entry's statements (or a trigger): in api/schema.sql, write the date itself"))
			return
		}
		now := time.UnixMilli(r.now).UTC().Format("2006-01-02 15:04:05.000")
		filled := false
		for i, a := range arg {
			if a.Type() == sqlite3.TEXT && strings.EqualFold(a.Text(), "now") {
				args[i], filled = now, true
			}
		}
		if !filled {
			args = append(args, now)
		}
	}
	r.call(ctx, name, args)
}

// call runs SQLite's own function name on args, in the connection of its
// own, and gives its answer.
func (r *replayEnv) call(ctx sqlite3.Context, name string, args []any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.own == nil {
		c, err := sqlite3.Open(":memory:")
		if err != nil {
			ctx.ResultError(err)
			return
		}
		r.own, r.stmts = c, map[string]*sqlite3.Stmt{}
	}
	q := "SELECT " + name + "(" + strings.TrimSuffix(strings.Repeat("?,", len(args)), ",") + ")"
	st := r.stmts[q]
	if st == nil {
		var err error
		if st, _, err = r.own.Prepare(q); err != nil {
			ctx.ResultError(err)
			return
		}
		r.stmts[q] = st
	}
	defer st.Reset()
	for i, a := range args {
		switch v := a.(type) {
		case int64:
			st.BindInt64(i+1, v)
		case float64:
			st.BindFloat(i+1, v)
		case string:
			st.BindText(i+1, v)
		case []byte:
			st.BindBlob(i+1, v)
		default:
			st.BindNull(i + 1)
		}
	}
	if !st.Step() {
		if err := st.Err(); err != nil {
			ctx.ResultError(err)
		} else {
			ctx.ResultNull()
		}
		return
	}
	switch st.ColumnType(0) {
	case sqlite3.INTEGER:
		ctx.ResultInt64(st.ColumnInt64(0))
	case sqlite3.FLOAT:
		ctx.ResultFloat(st.ColumnFloat(0))
	case sqlite3.TEXT:
		ctx.ResultText(st.ColumnText(0))
	default:
		ctx.ResultNull()
	}
}
