package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/zieludev/tapirpost/internal/content"
)

// frag is a piece of SQL with the arguments of its placeholders, in order.
type frag struct {
	sql  string
	args []any
}

// field is the expression a filter or a sort key reads.
func (s *DB) field(col string, ref content.FieldRef) frag {
	if ref.System {
		return frag{sql: ref.Name}
	}
	expr, arg := s.d.JSONField(col, ref.Kind, ref.Name)
	return frag{sql: expr, args: []any{arg}}
}

// value is the placeholder a field is compared with.
func (s *DB) value(ref content.FieldRef, v any) frag {
	if !ref.System {
		expr, arg := s.d.JSONValue(ref.Kind, v)
		return frag{sql: expr, args: []any{arg}}
	}
	if ts, ok := v.(time.Time); ok {
		return frag{sql: s.d.Int, args: []any{toMicros(ts)}}
	}
	return frag{sql: s.d.Text, args: []any{v}}
}

var comparisons = map[content.Op]string{
	content.OpEq:  "=",
	content.OpLt:  "<",
	content.OpLte: "<=",
	content.OpGt:  ">",
	content.OpGte: ">=",
}

func (s *DB) condition(col string, f content.Filter) (frag, error) {
	if len(f.Values) == 0 {
		return frag{}, fmt.Errorf("filter on %q has no value", f.Field.Name)
	}
	lhs := s.field(col, f.Field)
	rhs := s.value(f.Field, f.Values[0])

	switch f.Op {
	case content.OpNe:
		// An entry without the field is "not equal" too; a bare <> would
		// drop it, because comparing with NULL is never true.
		return frag{
			sql:  "(" + lhs.sql + " IS NULL OR " + lhs.sql + " <> " + rhs.sql + ")",
			args: concat(lhs.args, lhs.args, rhs.args),
		}, nil
	case content.OpContains:
		return frag{sql: fmt.Sprintf(s.d.Contains, lhs.sql, rhs.sql), args: concat(lhs.args, rhs.args)}, nil
	case content.OpIn:
		out := frag{sql: lhs.sql + " IN (", args: concat(lhs.args)}
		for i, v := range f.Values {
			if i > 0 {
				out.sql += ", "
			}
			one := s.value(f.Field, v)
			out.sql += one.sql
			out.args = append(out.args, one.args...)
		}
		out.sql += ")"
		return out, nil
	}
	cmp, ok := comparisons[f.Op]
	if !ok {
		return frag{}, fmt.Errorf("unknown filter operator %q", f.Op)
	}
	return frag{sql: lhs.sql + " " + cmp + " " + rhs.sql, args: concat(lhs.args, rhs.args)}, nil
}

func concat(parts ...[]any) []any {
	var out []any
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// Entries implements content.Store. With published set it reads, filters and
// sorts the published copies and skips entries that have none.
func (s *DB) Entries(ctx context.Context, typeName string, q content.Query, published bool) ([]content.Entry, int, error) {
	col, columns := "data", entryColumns
	where := frag{sql: "type = ?", args: []any{typeName}}
	if published {
		col, columns = "published_data", publishedColumns
		where.sql += " AND published_data IS NOT NULL"
	}
	for _, f := range q.Filters {
		cond, err := s.condition(col, f)
		if err != nil {
			return nil, 0, err
		}
		where.sql += " AND " + cond.sql
		where.args = append(where.args, cond.args...)
	}

	order := frag{}
	byID := false
	for _, key := range q.Sort {
		expr := s.field(col, key.Field)
		dir := " ASC"
		if key.Desc {
			dir = " DESC"
		}
		order.sql += expr.sql + dir + " NULLS LAST, "
		order.args = append(order.args, expr.args...)
		byID = byID || (key.Field.System && key.Field.Name == "id")
	}
	switch {
	case len(q.Sort) == 0:
		order.sql = "id DESC"
	case byID:
		order.sql = strings.TrimSuffix(order.sql, ", ")
	default:
		// Ties need a fixed order, or pages could overlap.
		order.sql += "id ASC"
	}

	// The window function counts the whole result while the page is read,
	// which spares a second pass over the same rows for the total.
	query := "SELECT " + columns + ", COUNT(*) OVER () FROM entries WHERE " + where.sql + " ORDER BY " + order.sql + " LIMIT ? OFFSET ?"
	args := concat(where.args, order.args, []any{q.Limit, q.Offset})
	rows, err := s.db.QueryContext(ctx, s.dynamic(query), args...)
	if err != nil {
		return nil, 0, fmt.Errorf("query entries: %w", err)
	}
	defer rows.Close()
	var total int
	entries := []content.Entry{}
	for rows.Next() {
		e, err := scanEntry(rows, &total)
		if err != nil {
			return nil, 0, err
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("read entries: %w", err)
	}

	// A page past the end has no row that could carry the total.
	if len(entries) == 0 && q.Offset > 0 {
		err = s.db.QueryRowContext(ctx, s.dynamic("SELECT COUNT(*) FROM entries WHERE "+where.sql), where.args...).Scan(&total)
		if err != nil {
			return nil, 0, fmt.Errorf("count entries: %w", err)
		}
	}
	return entries, total, nil
}
