package executor

// subquery.go — Phase 16: uncorrelated subquery pre-execution.
//
// Strategy: "subquery flattening" — before the outer query acquires any lock
// or builds any plan, walk its WHERE / HAVING clause and pre-execute every
// subquery condition.  Replace each subquery condition with its resolved form:
//
//   col IN (SELECT …)      → col IN (v1, v2, …)    — InQuery → InList
//   col op (SELECT …)      → col op literal          — ScalarQuery → Val
//   EXISTS (SELECT …)      → always-true (drop cond) or AlwaysFalse
//   NOT EXISTS (SELECT …)  → always-true (drop cond) or AlwaysFalse
//
// This approach keeps the Volcano iterator model and evalPreds completely
// unaware of subqueries — they only see resolved InList / Val / AlwaysFalse.
//
// Limitation: correlated subqueries (where the inner SELECT references outer
// columns that are not in the inner table schema) are not supported.  Attempting
// one will produce a "column not found" error from the inner execution.

import (
	"fmt"

	"github.com/yahya/db-engine/catalog"
	"github.com/yahya/db-engine/mvcc"
	"github.com/yahya/db-engine/query"
)

// execSelectFlattened resolves subqueries in s, then executes via execSelectWithSnap.
// snap must be the MVCC snapshot for this query (outer tx snapshot or fresh).
// It does NOT hold db.mu — execSelectWithSnap acquires RLock internally.
func (db *DB) execSelectFlattened(s *query.SelectStmt, snap mvcc.Snapshot) (*Result, error) {
	if err := db.flattenSubqueries(s.Where, snap); err != nil {
		return nil, err
	}
	if err := db.flattenSubqueries(s.Having, snap); err != nil {
		return nil, err
	}
	return db.execSelectWithSnap(s, snap)
}

// flattenSubqueries pre-executes every subquery condition in wc and replaces it
// with a resolved form.  Called before acquiring any db.mu lock so that inner
// execSelectFlattened calls can each acquire their own RLock without deadlock.
func (db *DB) flattenSubqueries(wc *query.WhereClause, snap mvcc.Snapshot) error {
	if wc == nil {
		return nil
	}
	for gi, group := range wc.Groups {
		kept := group[:0:len(group)] // sub-slice, avoid aliasing
		groupFailed := false

		for _, c := range group {
			switch {

			case c.InQuery != nil:
				// col IN (SELECT …) → execute once, collect first-column values.
				res, err := db.execSelectFlattened(c.InQuery, snap)
				if err != nil {
					return fmt.Errorf("IN subquery: %w", err)
				}
				c.InList = firstColumnValues(res.Rows)
				c.InQuery = nil
				kept = append(kept, c)

			case c.ScalarQuery != nil:
				// col op (SELECT …) → must return exactly one row/column.
				res, err := db.execSelectFlattened(c.ScalarQuery, snap)
				if err != nil {
					return fmt.Errorf("scalar subquery: %w", err)
				}
				if len(res.Rows) == 0 {
					// NULL semantics: condition never passes.
					groupFailed = true
				} else if len(res.Rows) > 1 {
					return fmt.Errorf("scalar subquery returned more than one row")
				} else {
					c.Val = res.Rows[0][0]
					c.ScalarQuery = nil
					kept = append(kept, c)
				}

			case c.ExistsQuery != nil:
				// EXISTS / NOT EXISTS → execute once, check if any rows returned.
				// Column values are irrelevant, so replace the SELECT list with *
				// to avoid issues with constant expressions (SELECT 1, SELECT NULL…).
				innerQ := *c.ExistsQuery
				innerQ.Columns = []query.SelectExpr{{Col: "*"}}
				res, err := db.execSelectFlattened(&innerQ, snap)
				if err != nil {
					return fmt.Errorf("EXISTS subquery: %w", err)
				}
				hasRows := len(res.Rows) > 0
				// Condition passes when: EXISTS+rows OR NOT EXISTS+no rows.
				if hasRows == c.Negated {
					// EXISTS+no rows or NOT EXISTS+rows → whole AND group fails.
					groupFailed = true
				}
				// Condition passes → drop it (empty group = always true).

			default:
				kept = append(kept, c)
			}

			if groupFailed {
				break
			}
		}

		if groupFailed {
			// Replace the group with a sentinel that evalPreds will reject immediately.
			wc.Groups[gi] = []query.Condition{{AlwaysFalse: true}}
		} else {
			wc.Groups[gi] = kept
		}
	}
	return nil
}

// flattenDMLSubqueries resolves subqueries in a WHERE clause for DELETE/UPDATE.
// Must be called before acquiring db.mu.Lock so inner SELECTs can get RLock.
func (db *DB) flattenDMLSubqueries(wc *query.WhereClause) error {
	if wc == nil {
		return nil
	}
	var snap mvcc.Snapshot
	if tx := db.goroutineTx(); tx != nil {
		snap = tx.snap
	} else {
		snap = db.txMgr.TakeSnapshot(mvcc.XIDNone)
	}
	return db.flattenSubqueries(wc, snap)
}

// firstColumnValues collects the first column from each row.
func firstColumnValues(rows [][]catalog.Value) []catalog.Value {
	out := make([]catalog.Value, 0, len(rows))
	for _, row := range rows {
		if len(row) > 0 {
			out = append(out, row[0])
		}
	}
	return out
}
