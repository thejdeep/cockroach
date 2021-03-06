// Copyright 2015 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.
//
// Author: Peter Mattis (peter@cockroachlabs.com)

package sql

import (
	"fmt"

	"github.com/cockroachdb/cockroach/pkg/sql/parser"
	"github.com/cockroachdb/cockroach/pkg/sql/privilege"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/util"
)

type insertNode struct {
	// The following fields are populated during makePlan.
	editNodeBase
	defaultExprs []parser.TypedExpr
	n            *parser.Insert
	checkHelper  checkHelper

	insertCols            []sqlbase.ColumnDescriptor
	insertColIDtoRowIndex map[sqlbase.ColumnID]int
	tw                    tableWriter

	run struct {
		// The following fields are populated during Start().
		editNodeRun

		rowIdxToRetIdx []int
		rowTemplate    parser.Datums
	}
}

// Insert inserts rows into the database.
// Privileges: INSERT on table. Also requires UPDATE on "ON DUPLICATE KEY UPDATE".
//   Notes: postgres requires INSERT. No "on duplicate key update" option.
//          mysql requires INSERT. Also requires UPDATE on "ON DUPLICATE KEY UPDATE".
func (p *planner) Insert(
	n *parser.Insert, desiredTypes []parser.Type, autoCommit bool,
) (planNode, error) {
	tn, err := p.getAliasedTableName(n.Table)
	if err != nil {
		return nil, err
	}

	en, err := p.makeEditNode(tn, autoCommit, privilege.INSERT)
	if err != nil {
		return nil, err
	}
	if n.OnConflict != nil {
		if !n.OnConflict.DoNothing {
			if err := p.checkPrivilege(en.tableDesc, privilege.UPDATE); err != nil {
				return nil, err
			}
		}
		// TODO(dan): Support RETURNING in UPSERTs.
		if n.Returning != nil {
			return nil, fmt.Errorf("RETURNING is not supported with UPSERT")
		}
	}

	var cols []sqlbase.ColumnDescriptor
	// Determine which columns we're inserting into.
	if n.DefaultValues() {
		cols = en.tableDesc.Columns
	} else {
		var err error
		if cols, err = p.processColumns(en.tableDesc, n.Columns); err != nil {
			return nil, err
		}
	}
	// Number of columns expecting an input. This doesn't include the
	// columns receiving a default value.
	numInputColumns := len(cols)

	cols, defaultExprs, err := ProcessDefaultColumns(cols, en.tableDesc, &p.parser, &p.evalCtx)
	if err != nil {
		return nil, err
	}
	if err != nil {
		return nil, err
	}

	var insertRows parser.SelectStatement
	if n.DefaultValues() {
		insertRows = getDefaultValuesClause(defaultExprs, cols)
	} else {
		src, values, err := extractInsertSource(n.Rows)
		if err != nil {
			return nil, err
		}
		if values != nil {
			src = fillDefaults(defaultExprs, cols, values)
		}
		insertRows = src
	}

	// Analyze the expressions for column information and typing.
	desiredTypesFromSelect := make([]parser.Type, len(cols))
	for i, col := range cols {
		desiredTypesFromSelect[i] = col.Type.ToDatumType()
	}

	// Create the plan for the data source.
	// This performs type checking on source expressions, collecting
	// types for placeholders in the process.
	rows, err := p.newPlan(insertRows, desiredTypesFromSelect, false)
	if err != nil {
		return nil, err
	}

	if expressions := len(rows.Columns()); expressions > numInputColumns {
		return nil, fmt.Errorf("INSERT error: table %s has %d columns but %d values were supplied", n.Table, numInputColumns, expressions)
	}

	fkTables := tablesNeededForFKs(*en.tableDesc, CheckInserts)
	if err := p.fillFKTableMap(fkTables); err != nil {
		return nil, err
	}
	ri, err := MakeRowInserter(p.txn, en.tableDesc, fkTables, cols, checkFKs)
	if err != nil {
		return nil, err
	}

	var tw tableWriter
	if n.OnConflict == nil {
		tw = &tableInserter{ri: ri, autoCommit: autoCommit}
	} else {
		updateExprs, conflictIndex, err := upsertExprsAndIndex(en.tableDesc, *n.OnConflict, ri.insertCols)
		if err != nil {
			return nil, err
		}

		if n.OnConflict.DoNothing {
			// TODO(dan): Postgres allows ON CONFLICT DO NOTHING without specifying a
			// conflict index, which means do nothing on any conflict. Support this if
			// someone needs it.
			tw = &tableUpserter{ri: ri, conflictIndex: *conflictIndex}
		} else {
			names, err := p.namesForExprs(updateExprs)
			if err != nil {
				return nil, err
			}
			// Also include columns that are inactive because they should be
			// updated.
			updateCols := make([]sqlbase.ColumnDescriptor, len(names))
			for i, n := range names {
				c, err := n.NormalizeUnqualifiedColumnItem()
				if err != nil {
					return nil, err
				}

				status, idx, err := en.tableDesc.FindColumnByName(c.ColumnName)
				if err != nil {
					return nil, err
				}
				if status == sqlbase.DescriptorActive {
					updateCols[i] = en.tableDesc.Columns[idx]
				} else {
					updateCols[i] = *en.tableDesc.Mutations[idx].GetColumn()
				}
			}

			helper, err := p.makeUpsertHelper(tn, en.tableDesc, ri.insertCols, updateCols, updateExprs, conflictIndex)
			if err != nil {
				return nil, err
			}

			fkTables := tablesNeededForFKs(*en.tableDesc, CheckUpdates)
			if err := p.fillFKTableMap(fkTables); err != nil {
				return nil, err
			}
			tw = &tableUpserter{ri: ri, fkTables: fkTables, updateCols: updateCols, conflictIndex: *conflictIndex, evaler: helper}
		}
	}

	in := &insertNode{
		n:                     n,
		editNodeBase:          en,
		defaultExprs:          defaultExprs,
		insertCols:            ri.insertCols,
		insertColIDtoRowIndex: ri.InsertColIDtoRowIndex,
		tw: tw,
	}

	if err := in.checkHelper.init(p, tn, en.tableDesc); err != nil {
		return nil, err
	}

	if err := in.run.initEditNode(&in.editNodeBase, rows, n.Returning, desiredTypes); err != nil {
		return nil, err
	}

	return in, nil
}

// ProcessDefaultColumns adds columns with DEFAULT to cols if not present
// and returns the defaultExprs for cols.
func ProcessDefaultColumns(
	cols []sqlbase.ColumnDescriptor,
	tableDesc *sqlbase.TableDescriptor,
	parse *parser.Parser,
	evalCtx *parser.EvalContext,
) ([]sqlbase.ColumnDescriptor, []parser.TypedExpr, error) {
	colIDSet := make(map[sqlbase.ColumnID]struct{}, len(cols))
	for _, col := range cols {
		colIDSet[col.ID] = struct{}{}
	}

	// Add the column if it has a DEFAULT expression.
	addIfDefault := func(col sqlbase.ColumnDescriptor) {
		if col.DefaultExpr != nil {
			if _, ok := colIDSet[col.ID]; !ok {
				colIDSet[col.ID] = struct{}{}
				cols = append(cols, col)
			}
		}
	}

	// Add any column that has a DEFAULT expression.
	for _, col := range tableDesc.Columns {
		addIfDefault(col)
	}
	// Also add any column in a mutation that is WRITE_ONLY and has
	// a DEFAULT expression.
	for _, m := range tableDesc.Mutations {
		if m.State != sqlbase.DescriptorMutation_WRITE_ONLY {
			continue
		}
		if col := m.GetColumn(); col != nil {
			addIfDefault(*col)
		}
	}

	defaultExprs, err := makeDefaultExprs(cols, parse, evalCtx)
	return cols, defaultExprs, err
}

func (n *insertNode) Start() error {
	// Prepare structures for building values to pass to rh.
	if n.rh.exprs != nil {
		// In some cases (e.g. `INSERT INTO t (a) ...`) rowVals does not contain all the table
		// columns. We need to pass values for all table columns to rh, in the correct order; we
		// will use rowTemplate for this. We also need a table that maps row indices to rowTemplate indices
		// to fill in the row values; any absent values will be NULLs.

		n.run.rowTemplate = make(parser.Datums, len(n.tableDesc.Columns))
		for i := range n.run.rowTemplate {
			n.run.rowTemplate[i] = parser.DNull
		}

		colIDToRetIndex := map[sqlbase.ColumnID]int{}
		for i, col := range n.tableDesc.Columns {
			colIDToRetIndex[col.ID] = i
		}

		n.run.rowIdxToRetIdx = make([]int, len(n.insertCols))
		for i, col := range n.insertCols {
			n.run.rowIdxToRetIdx[i] = colIDToRetIndex[col.ID]
		}
	}

	if err := n.run.startEditNode(&n.editNodeBase, n.tw); err != nil {
		return err
	}

	return n.run.tw.init(n.p.txn)
}

func (n *insertNode) Close() {
	n.run.rows.Close()
}

func (n *insertNode) Next() (bool, error) {
	ctx := n.editNodeBase.p.ctx()
	if next, err := n.run.rows.Next(); !next {
		if err == nil {
			// We're done. Finish the batch.
			err = n.tw.finalize(ctx)
		}
		return false, err
	}

	if n.run.explain == explainDebug {
		return true, nil
	}

	rowVals, err := GenerateInsertRow(n.defaultExprs, n.insertColIDtoRowIndex, n.insertCols, n.p.evalCtx, n.tableDesc, n.run.rows.Values())
	if err != nil {
		return false, err
	}

	n.checkHelper.loadRow(n.insertColIDtoRowIndex, rowVals, false)
	if err := n.checkHelper.check(&n.p.evalCtx); err != nil {
		return false, err
	}

	_, err = n.tw.row(ctx, rowVals)
	if err != nil {
		return false, err
	}

	for i, val := range rowVals {
		if n.run.rowTemplate != nil {
			n.run.rowTemplate[n.run.rowIdxToRetIdx[i]] = val
		}
	}

	resultRow, err := n.rh.cookResultRow(n.run.rowTemplate)
	if err != nil {
		return false, err
	}
	n.run.resultRow = resultRow

	return true, nil
}

// GenerateInsertRow prepares a row tuple for insertion. It fills in default
// expressions, verifies non-nullable columns, and checks column widths.
func GenerateInsertRow(
	defaultExprs []parser.TypedExpr,
	insertColIDtoRowIndex map[sqlbase.ColumnID]int,
	insertCols []sqlbase.ColumnDescriptor,
	evalCtx parser.EvalContext,
	tableDesc *sqlbase.TableDescriptor,
	rowVals parser.Datums,
) (parser.Datums, error) {
	// The values for the row may be shorter than the number of columns being
	// inserted into. Generate default values for those columns using the
	// default expressions.

	if len(rowVals) < len(insertCols) {
		// It's not cool to append to the slice returned by a node; make a copy.
		oldVals := rowVals
		rowVals = make(parser.Datums, len(insertCols))
		copy(rowVals, oldVals)

		for i := len(oldVals); i < len(insertCols); i++ {
			if defaultExprs == nil {
				rowVals[i] = parser.DNull
				continue
			}
			d, err := defaultExprs[i].Eval(&evalCtx)
			if err != nil {
				return nil, err
			}
			rowVals[i] = d
		}
	}

	// Check to see if NULL is being inserted into any non-nullable column.
	for _, col := range tableDesc.Columns {
		if !col.Nullable {
			if i, ok := insertColIDtoRowIndex[col.ID]; !ok || rowVals[i] == parser.DNull {
				return nil, sqlbase.NewNonNullViolationError(col.Name)
			}
		}
	}

	// Ensure that the values honor the specified column widths.
	for i := range rowVals {
		if err := sqlbase.CheckValueWidth(insertCols[i], rowVals[i]); err != nil {
			return nil, err
		}
	}
	return rowVals, nil
}

func (p *planner) processColumns(
	tableDesc *sqlbase.TableDescriptor, node parser.UnresolvedNames,
) ([]sqlbase.ColumnDescriptor, error) {
	if node == nil {
		// VisibleColumns is used here to prevent INSERT INTO <table> VALUES (...)
		// (as opposed to INSERT INTO <table> (...) VALUES (...)) from writing
		// hidden columns. At present, the only hidden column is the implicit rowid
		// primary key column.
		return tableDesc.VisibleColumns(), nil
	}

	cols := make([]sqlbase.ColumnDescriptor, len(node))
	colIDSet := make(map[sqlbase.ColumnID]struct{}, len(node))
	for i, n := range node {
		c, err := n.NormalizeUnqualifiedColumnItem()
		if err != nil {
			return nil, err
		}

		if len(c.Selector) > 0 {
			return nil, util.UnimplementedWithIssueErrorf(8318, "compound types not supported yet: %q", n)
		}

		col, err := tableDesc.FindActiveColumnByName(c.ColumnName)
		if err != nil {
			return nil, err
		}

		if _, ok := colIDSet[col.ID]; ok {
			return nil, fmt.Errorf("multiple assignments to the same column %q", n)
		}
		colIDSet[col.ID] = struct{}{}
		cols[i] = col
	}

	return cols, nil
}

// extractInsertSource removes the parentheses around the data source of an INSERT statement.
// If the data source is a VALUES clause not further qualified with LIMIT/OFFSET and ORDER BY,
// the 2nd return value is a pre-casted pointer to the VALUES clause.
func extractInsertSource(s *parser.Select) (parser.SelectStatement, *parser.ValuesClause, error) {
	wrapped := s.Select
	limit := s.Limit
	orderBy := s.OrderBy

	for s, ok := wrapped.(*parser.ParenSelect); ok; s, ok = wrapped.(*parser.ParenSelect) {
		wrapped = s.Select.Select
		if s.Select.OrderBy != nil {
			if orderBy != nil {
				return nil, nil, fmt.Errorf("multiple ORDER BY clauses not allowed")
			}
			orderBy = s.Select.OrderBy
		}
		if s.Select.Limit != nil {
			if limit != nil {
				return nil, nil, fmt.Errorf("multiple LIMIT clauses not allowed")
			}
			limit = s.Select.Limit
		}
	}

	if orderBy == nil && limit == nil {
		values, _ := wrapped.(*parser.ValuesClause)
		return wrapped, values, nil
	}
	return &parser.ParenSelect{
		Select: &parser.Select{Select: wrapped, OrderBy: orderBy, Limit: limit},
	}, nil, nil
}

func getDefaultValuesClause(
	defaultExprs []parser.TypedExpr, cols []sqlbase.ColumnDescriptor,
) parser.SelectStatement {
	row := make(parser.Exprs, 0, len(cols))
	for i := range cols {
		if defaultExprs == nil {
			row = append(row, parser.DNull)
			continue
		}
		row = append(row, defaultExprs[i])
	}
	return &parser.ValuesClause{Tuples: []*parser.Tuple{{Exprs: row}}}
}

func fillDefaults(
	defaultExprs []parser.TypedExpr, cols []sqlbase.ColumnDescriptor, values *parser.ValuesClause,
) *parser.ValuesClause {
	ret := values
	for tIdx, tuple := range values.Tuples {
		tupleCopied := false
		for eIdx, val := range tuple.Exprs {
			switch val.(type) {
			case parser.DefaultVal:
				if !tupleCopied {
					if ret == values {
						ret = &parser.ValuesClause{Tuples: append([]*parser.Tuple(nil), values.Tuples...)}
					}
					ret.Tuples[tIdx] =
						&parser.Tuple{Exprs: append([]parser.Expr(nil), tuple.Exprs...)}
					tupleCopied = true
				}
				if defaultExprs == nil || eIdx >= len(defaultExprs) {
					// The case where eIdx is too large for defaultExprs will be
					// transformed into an error by the check on the number of
					// columns in Insert().
					ret.Tuples[tIdx].Exprs[eIdx] = parser.DNull
				} else {
					ret.Tuples[tIdx].Exprs[eIdx] = defaultExprs[eIdx]
				}
			}
		}
	}
	return ret
}

func makeDefaultExprs(
	cols []sqlbase.ColumnDescriptor, parse *parser.Parser, evalCtx *parser.EvalContext,
) ([]parser.TypedExpr, error) {
	// Check to see if any of the columns have DEFAULT expressions. If there
	// are no DEFAULT expressions, we don't bother with constructing the
	// defaults map as the defaults are all NULL.
	haveDefaults := false
	for _, col := range cols {
		if col.DefaultExpr != nil {
			haveDefaults = true
			break
		}
	}
	if !haveDefaults {
		return nil, nil
	}

	// Build the default expressions map from the parsed SELECT statement.
	defaultExprs := make([]parser.TypedExpr, 0, len(cols))
	exprStrings := make([]string, 0, len(cols))
	for _, col := range cols {
		if col.DefaultExpr != nil {
			exprStrings = append(exprStrings, *col.DefaultExpr)
		}
	}
	exprs, err := parser.ParseExprsTraditional(exprStrings)
	if err != nil {
		return nil, err
	}

	defExprIdx := 0
	for _, col := range cols {
		if col.DefaultExpr == nil {
			defaultExprs = append(defaultExprs, parser.DNull)
			continue
		}
		expr := exprs[defExprIdx]
		typedExpr, err := parser.TypeCheck(expr, nil, col.Type.ToDatumType())
		if err != nil {
			return nil, err
		}
		if typedExpr, err = parse.NormalizeExpr(evalCtx, typedExpr); err != nil {
			return nil, err
		}
		defaultExprs = append(defaultExprs, typedExpr)
		defExprIdx++
	}
	return defaultExprs, nil
}

func (n *insertNode) Columns() ResultColumns {
	return n.rh.columns
}

func (n *insertNode) Values() parser.Datums {
	return n.run.resultRow
}

func (n *insertNode) MarkDebug(mode explainMode) {
	if mode != explainDebug {
		panic(fmt.Sprintf("unknown debug mode %d", mode))
	}
	n.run.explain = mode
	n.run.rows.MarkDebug(mode)
}

func (n *insertNode) DebugValues() debugValues {
	return n.run.rows.DebugValues()
}

func (n *insertNode) Ordering() orderingInfo { return orderingInfo{} }
