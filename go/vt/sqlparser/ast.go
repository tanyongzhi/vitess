/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package sqlparser

//go:generate goyacc -o sql.go sql.y

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"unicode"

	"github.com/dolthub/vitess/go/sqltypes"
	"github.com/dolthub/vitess/go/vt/vterrors"

	querypb "github.com/dolthub/vitess/go/vt/proto/query"
	vtrpcpb "github.com/dolthub/vitess/go/vt/proto/vtrpc"
)

// parserPool is a pool for parser objects.
var parserPool = sync.Pool{}

// zeroParser is a zero-initialized parser to help reinitialize the parser for pooling.
var zeroParser = *(yyNewParser().(*yyParserImpl))

// yyParsePooled is a wrapper around yyParse that pools the parser objects. There isn't a
// particularly good reason to use yyParse directly, since it immediately discards its parser.  What
// would be ideal down the line is to actually pool the stacks themselves rather than the parser
// objects, as per https://github.com/cznic/goyacc/blob/master/main.go. However, absent an upstream
// change to goyacc, this is the next best option.
//
// N.B: Parser pooling means that you CANNOT take references directly to parse stack variables (e.g.
// $$ = &$4) in sql.y rules. You must instead add an intermediate reference like so:
//
//	showCollationFilterOpt := $4
//	$$ = &Show{Type: string($2), ShowCollationFilterOpt: &showCollationFilterOpt}
func yyParsePooled(yylex yyLexer) int {
	// Being very particular about using the base type and not an interface type b/c we depend on
	// the implementation to know how to reinitialize the parser.
	var parser *yyParserImpl

	i := parserPool.Get()
	if i != nil {
		parser = i.(*yyParserImpl)
	} else {
		parser = yyNewParser().(*yyParserImpl)
	}

	defer func() {
		*parser = zeroParser
		parserPool.Put(parser)
	}()
	return parser.Parse(yylex)
}

// Instructions for creating new types: If a type
// needs to satisfy an interface, declare that function
// along with that interface. This will help users
// identify the list of types to which they can assert
// those interfaces.
// If the member of a type has a string with a predefined
// list of values, declare those values as const following
// the type.
// For interfaces that define dummy functions to consolidate
// a set of types, define the function as iTypeName.
// This will help avoid name collisions.

// Parse parses the SQL in full and returns a Statement, which
// is the AST representation of the query. If a DDL statement
// is partially parsed but still contains a syntax error, the
// error is ignored and the DDL is returned anyway.
func Parse(sql string) (Statement, error) {
	tokenizer := NewStringTokenizer(sql)
	return parseTokenizer(sql, tokenizer)
}

// ParseOne parses the first SQL statement in the given string and returns the
// index of the start of the next statement in |sql|. If there was only one
// statement in |sql|, the value of the returned index will be |len(sql)|.
func ParseOne(sql string) (Statement, int, error) {
	tokenizer := NewStringTokenizer(sql)
	tokenizer.stopAfterFirstStmt = true
	tree, err := parseTokenizer(sql, tokenizer)
	if err != nil {
		if err == ErrEmpty {
			return nil, tokenizer.Position, err
		} else {
			return nil, 0, err
		}
	}

	return tree, tokenizer.Position, nil
}

func parseTokenizer(sql string, tokenizer *Tokenizer) (Statement, error) {
	if yyParsePooled(tokenizer) != 0 {
		if se, ok := tokenizer.LastError.(vterrors.SyntaxError); ok {
			return nil, vterrors.NewWithCause(vtrpcpb.Code_INVALID_ARGUMENT, tokenizer.LastError.Error(), se)
		} else {
			return nil, vterrors.New(vtrpcpb.Code_INVALID_ARGUMENT, tokenizer.LastError.Error())
		}
	}
	if tokenizer.ParseTree == nil {
		return nil, ErrEmpty
	}
	captureSelectExpressions(sql, tokenizer)
	adjustSubstatementPositions(sql, tokenizer)
	return tokenizer.ParseTree, nil
}

// For select statements, capture the verbatim select expressions from the original query text
func captureSelectExpressions(sql string, tokenizer *Tokenizer) {
	if s, ok := tokenizer.ParseTree.(SelectStatement); ok {
		s.walkSubtree(func(node SQLNode) (bool, error) {
			if node, ok := node.(*AliasedExpr); ok && node.EndParsePos > node.StartParsePos {
				_, ok := node.Expr.(*ColName)
				if ok {
					// column names don't need any special handling to capture the input expression
					return false, nil
				} else {
					node.InputExpression = trimQuotes(strings.TrimLeft(sql[node.StartParsePos:node.EndParsePos], " \n\t"))
				}
			}
			return true, nil
		})
	}
}

// For DDL statements that capture the position of a sub-statement (create view and others), we need to adjust these
// indexes if they occurred inside a MySQL special comment (/*! */) because we sometimes inappropriately capture the
// comment ending characters in such cases.
func adjustSubstatementPositions(sql string, tokenizer *Tokenizer) {
	if ddl, ok := tokenizer.ParseTree.(*DDL); ok {
		if ddl.SpecialCommentMode && ddl.SubStatementPositionStart > 0 &&
			ddl.SubStatementPositionEnd > ddl.SubStatementPositionStart {
			sub := sql[ddl.SubStatementPositionStart:ddl.SubStatementPositionEnd]

			// We don't actually capture the end of the comment in all cases, sometimes it's just *
			endCommentIdx := strings.LastIndex(sub, "*/") - 1
			if endCommentIdx < 0 {
				if sub[len(sub)-1] == '*' {
					endCommentIdx = len(sub) - 2
				} else {
					endCommentIdx = len(sub) - 1
				}
			}

			// Backtrack until we find a non-space character. That's the actual end of the substatement.
			for endCommentIdx > 0 && unicode.IsSpace(rune(sub[endCommentIdx])) {
				endCommentIdx--
			}

			if !unicode.IsSpace(rune(sub[endCommentIdx])) {
				endCommentIdx++
			}

			ddl.SubStatementPositionEnd = ddl.SubStatementPositionStart + endCommentIdx
		}
	}
}

func trimQuotes(s string) string {
	firstChar := s[0]
	lastChar := s[len(s)-1]
	if firstChar == lastChar {
		if firstChar == '`' || firstChar == '"' || firstChar == '\'' {
			// Some edge cases here: we have to be careful to not strip expressions like `"1" + "2"`
			if stringIsUnbrokenQuote(s, firstChar) {
				return s[1 : len(s)-1]
			}
		}
	}

	return s
}

func stringIsUnbrokenQuote(s string, quoteChar byte) bool {
	numConsecutiveQuotes := 0
	numConsecutiveEscapes := 0
	for _, c := range ([]byte)(s[1 : len(s)-1]) {
		if c == quoteChar && numConsecutiveEscapes%2 == 0 {
			numConsecutiveQuotes++
		} else {
			if numConsecutiveQuotes%2 != 0 {
				return false
			}
			numConsecutiveQuotes = 0
		}

		if c == '\\' {
			numConsecutiveEscapes++
		} else {
			numConsecutiveEscapes = 0
		}
	}
	return true
}

// ParseTokenizer is a raw interface to parse from the given tokenizer.
// This does not used pooled parsers, and should not be used in general.
func ParseTokenizer(tokenizer *Tokenizer) int {
	return yyParse(tokenizer)
}

// ParseNext parses a single SQL statement from the tokenizer
// returning a Statement which is the AST representation of the query.
// The tokenizer will always read up to the end of the statement, allowing for
// the next call to ParseNext to parse any subsequent SQL statements. When
// there are no more statements to parse, a error of io.EOF is returned.
func ParseNext(tokenizer *Tokenizer) (Statement, error) {
	if tokenizer.lastChar == ';' {
		tokenizer.next()
		tokenizer.skipBlank()
	}
	if tokenizer.lastChar == eofChar {
		return nil, io.EOF
	}

	tokenizer.reset()
	tokenizer.multi = true
	if yyParsePooled(tokenizer) != 0 {
		return nil, tokenizer.LastError
	}
	if tokenizer.ParseTree == nil {
		return ParseNext(tokenizer)
	}

	captureSelectExpressions((string)(tokenizer.queryBuf), tokenizer)

	return tokenizer.ParseTree, nil
}

// ErrEmpty is a sentinel error returned when parsing empty statements.
var ErrEmpty = errors.New("empty statement")

// SplitStatement returns the first sql statement up to either a ; or EOF
// and the remainder from the given buffer
func SplitStatement(blob string) (string, string, error) {
	tokenizer := NewStringTokenizer(blob)
	tkn := 0
	for {
		tkn, _ = tokenizer.Scan()
		if tkn == 0 || tkn == ';' || tkn == eofChar {
			break
		}
	}
	if tokenizer.LastError != nil {
		return "", "", tokenizer.LastError
	}
	if tkn == ';' {
		return blob[:tokenizer.Position-2], blob[tokenizer.Position-1:], nil
	}
	return blob, "", nil
}

// SplitStatementToPieces split raw sql statement that may have multi sql pieces to sql pieces
// returns the sql pieces blob contains; or error if sql cannot be parsed
func SplitStatementToPieces(blob string) (pieces []string, err error) {
	pieces = make([]string, 0, 16)
	tokenizer := NewStringTokenizer(blob)

	tkn := 0
	var stmt string
	stmtBegin := 0
	for {
		tkn, _ = tokenizer.Scan()
		if tkn == ';' {
			stmt = blob[stmtBegin : tokenizer.Position-2]
			pieces = append(pieces, stmt)
			stmtBegin = tokenizer.Position - 1

		} else if tkn == 0 || tkn == eofChar {
			blobTail := tokenizer.Position - 2

			if stmtBegin < blobTail {
				stmt = blob[stmtBegin : blobTail+1]
				if strings.TrimSpace(stmt) != "" {
					pieces = append(pieces, stmt)
				}
			}
			break
		}
	}

	err = tokenizer.LastError
	return
}

// SQLNode defines the interface for all nodes
// generated by the parser.
type SQLNode interface {
	Format(buf *TrackedBuffer)
}

// WalkableSQLNode represents an interface for nodes
// that have subnodes that may need to be traversed over.
type WalkableSQLNode interface {
	SQLNode
	// walkSubtree calls visit on all underlying nodes
	// of the subtree, but not the current one. Walking
	// must be interrupted if visit returns an error.
	walkSubtree(visit Visit) error
}

// Visit defines the signature of a function that
// can be used to visit all nodes of a parse tree.
type Visit func(node SQLNode) (kontinue bool, err error)

// Walk calls visit on every node.
// If visit returns true, the underlying nodes
// are also visited. If it returns an error, walking
// is interrupted, and the error is returned.
func Walk(visit Visit, nodes ...SQLNode) error {
	for _, node := range nodes {
		if node == nil {
			continue
		}
		kontinue, err := visit(node)
		if err != nil {
			return err
		}
		if kontinue {
			if node, ok := node.(WalkableSQLNode); ok {
				err = node.walkSubtree(visit)
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// String returns a string representation of an SQLNode.
func String(node SQLNode) string {
	if node == nil {
		return "<nil>"
	}

	buf := NewTrackedBuffer(nil)
	buf.Myprintf("%v", node)
	return buf.String()
}

// Append appends the SQLNode to the buffer.
func Append(buf *strings.Builder, node SQLNode) {
	tbuf := &TrackedBuffer{
		Builder: buf,
	}
	node.Format(tbuf)
}

// Statement represents a statement.
type Statement interface {
	iStatement()
	SQLNode
}

type Statements []Statement

func (*Union) iStatement()             {}
func (*Select) iStatement()            {}
func (*Stream) iStatement()            {}
func (*Insert) iStatement()            {}
func (*Update) iStatement()            {}
func (*Delete) iStatement()            {}
func (*Set) iStatement()               {}
func (*DBDDL) iStatement()             {}
func (*DDL) iStatement()               {}
func (*MultiAlterDDL) iStatement()     {}
func (*Explain) iStatement()           {}
func (*Show) iStatement()              {}
func (*Use) iStatement()               {}
func (*Begin) iStatement()             {}
func (*Commit) iStatement()            {}
func (*Rollback) iStatement()          {}
func (*Flush) iStatement()             {}
func (*OtherRead) iStatement()         {}
func (*OtherAdmin) iStatement()        {}
func (*BeginEndBlock) iStatement()     {}
func (*CaseStatement) iStatement()     {}
func (*IfStatement) iStatement()       {}
func (*Signal) iStatement()            {}
func (*Resignal) iStatement()          {}
func (*Declare) iStatement()           {}
func (*Call) iStatement()              {}
func (*Load) iStatement()              {}
func (*Savepoint) iStatement()         {}
func (*RollbackSavepoint) iStatement() {}
func (*ReleaseSavepoint) iStatement()  {}
func (*LockTables) iStatement()        {}
func (*UnlockTables) iStatement()      {}

// ParenSelect can actually not be a top level statement,
// but we have to allow it because it's a requirement
// of SelectStatement.
func (*ParenSelect) iStatement() {}

// SelectStatement any SELECT statement.
type SelectStatement interface {
	iSelectStatement()
	iStatement()
	iInsertRows()
	AddOrder(*Order)
	SetLimit(*Limit)
	SetLock(string)
	SetOrderBy(OrderBy)
	SetWith(*With)
	SetInto(*Into) error
	GetInto() *Into
	WalkableSQLNode
}

func (*Select) iSelectStatement()          {}
func (*Union) iSelectStatement()           {}
func (*ParenSelect) iSelectStatement()     {}
func (*ValuesStatement) iSelectStatement() {}

// Select represents a SELECT statement.
type Select struct {
	Cache         string
	CalcFoundRows bool
	Comments      Comments
	Distinct      string
	Hints         string
	With          *With
	SelectExprs   SelectExprs
	From          TableExprs
	Where         *Where
	GroupBy       GroupBy
	Having        *Where
	Window        Window
	OrderBy       OrderBy
	Limit         *Limit
	Lock          string
	Into          *Into
}

// Select.Distinct
const (
	DistinctStr      = "distinct "
	StraightJoinHint = "straight_join "
)

// Select.Lock
const (
	ForUpdateStr = " for update"
	ShareModeStr = " lock in share mode"
)

// Select.Cache
const (
	SQLCacheStr   = "sql_cache "
	SQLNoCacheStr = "sql_no_cache "
)

// AddOrder adds an order by element
func (node *Select) AddOrder(order *Order) {
	node.OrderBy = append(node.OrderBy, order)
}

func (node *Select) SetOrderBy(orderBy OrderBy) {
	node.OrderBy = orderBy
}

func (node *Select) SetWith(w *With) {
	node.With = w
}

func (node *Select) SetLock(lock string) {
	node.Lock = lock
}

func (node *Select) SetInto(into *Into) error {
	if into == nil {
		return nil
	}
	if node.Into != nil {
		return fmt.Errorf("Multiple INTO clauses in one query block")
	}
	node.Into = into
	return nil
}

func (node *Select) GetInto() *Into {
	return node.Into
}

// SetLimit sets the limit clause
func (node *Select) SetLimit(limit *Limit) {
	node.Limit = limit
}

// Format formats the node.
func (node *Select) Format(buf *TrackedBuffer) {
	calcFoundRows := ""
	if node.CalcFoundRows {
		calcFoundRows = "sql_calc_found_rows "
	}

	buf.Myprintf("%vselect %v%s%s%s%s%v",
		node.With,
		node.Comments, node.Cache, calcFoundRows, node.Distinct, node.Hints, node.SelectExprs,
	)

	if node.From != nil {
		buf.Myprintf(" from %v", node.From)
	}

	buf.Myprintf("%v%v%v%v%v%v%s%v",
		node.Where, node.GroupBy, node.Having, node.Window,
		node.OrderBy, node.Limit, node.Lock, node.Into)
}

func (node *Select) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Comments,
		node.SelectExprs,
		node.From,
		node.Where,
		node.GroupBy,
		node.Having,
		node.OrderBy,
		node.Limit,
		node.Into,
	)
}

// AddWhere adds the boolean expression to the
// WHERE clause as an AND condition. If the expression
// is an OR clause, it parenthesizes it.
// Both OR and XOR operators are lower precedence than AND.
func (node *Select) AddWhere(expr Expr) {
	switch expr.(type) {
	case *OrExpr, *XorExpr:
		expr = &ParenExpr{Expr: expr}
	}
	if node.Where == nil {
		node.Where = &Where{
			Type: WhereStr,
			Expr: expr,
		}
		return
	}
	node.Where.Expr = &AndExpr{
		Left:  node.Where.Expr,
		Right: expr,
	}
}

// AddHaving adds the boolean expression to the
// HAVING clause as an AND condition. If the expression
// is an OR clause, it parenthesizes it.
// Both OR and XOR operators are lower precedence than AND.
func (node *Select) AddHaving(expr Expr) {
	switch expr.(type) {
	case *OrExpr, *XorExpr:
		expr = &ParenExpr{Expr: expr}
	}
	if node.Having == nil {
		node.Having = &Where{
			Type: HavingStr,
			Expr: expr,
		}
		return
	}
	node.Having.Expr = &AndExpr{
		Left:  node.Having.Expr,
		Right: expr,
	}
}

// ParenSelect is a parenthesized SELECT statement.
type ParenSelect struct {
	Select SelectStatement
}

// AddOrder adds an order by element
func (node *ParenSelect) AddOrder(order *Order) {
	panic("unreachable")
}

func (node *ParenSelect) SetOrderBy(orders OrderBy) {
	panic("unreachable")
}

func (node *ParenSelect) SetWith(w *With) {
	panic("unreachable")
}

func (node *ParenSelect) SetLock(lock string) {
	panic("unreachable")
}

// SetLimit sets the limit clause
func (node *ParenSelect) SetLimit(limit *Limit) {
	panic("unreachable")
}

func (node *ParenSelect) SetInto(into *Into) error {
	panic("unreachable")
}

func (node *ParenSelect) GetInto() *Into {
	return node.Select.GetInto()
}

// Format formats the node.
func (node *ParenSelect) Format(buf *TrackedBuffer) {
	buf.Myprintf("(%v)", node.Select)
}

func (node *ParenSelect) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Select,
	)
}

// ValuesStatement is a VALUES ROW('1', '2'), ROW(3, 4) expression, which can be a table factor or a stand-alone
// statement
type ValuesStatement struct {
	Rows    Values
	Columns Columns
}

func (s *ValuesStatement) Format(buf *TrackedBuffer) {
	buf.Myprintf("values ")
	for i, row := range s.Rows {
		if i > 0 {
			buf.Myprintf(", ")
		}
		buf.Myprintf("row%v", row)
	}
}

func (s *ValuesStatement) walkSubtree(visit Visit) error {
	return Walk(visit, s.Rows)
}

// Union represents a UNION statement.
type Union struct {
	Type        string
	Left, Right SelectStatement
	OrderBy     OrderBy
	With        *With
	Limit       *Limit
	Lock        string
	Into        *Into
}

// Union.Type
const (
	UnionStr         = "union"
	UnionAllStr      = "union all"
	UnionDistinctStr = "union distinct"
)

// AddOrder adds an order by element
func (node *Union) AddOrder(order *Order) {
	node.OrderBy = append(node.OrderBy, order)
}

func (node *Union) SetOrderBy(orderBy OrderBy) {
	node.OrderBy = orderBy
}

func (node *Union) SetWith(w *With) {
	node.With = w
}

// SetLimit sets the limit clause
func (node *Union) SetLimit(limit *Limit) {
	node.Limit = limit
}

func (node *Union) SetLock(lock string) {
	node.Lock = lock
}

func (node *Union) SetInto(into *Into) error {
	if into == nil {
		if r, ok := node.Right.(*Select); ok {
			node.Into = r.Into
			r.Into = nil
		}
		return nil
	}
	if node.Into != nil {
		return fmt.Errorf("Multiple INTO clauses in one query block")
	}
	node.Into = into
	return nil
}

func (node *Union) GetInto() *Into {
	return node.Into
}

// Format formats the node.
func (node *Union) Format(buf *TrackedBuffer) {
	buf.Myprintf("%v%v %s %v%v%v%s%v", node.With, node.Left, node.Type, node.Right,
		node.OrderBy, node.Limit, node.Lock, node.Into)
}

func (node *Union) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Left,
		node.Right,
	)
}

// LoadStatement any LOAD statement.
type LoadStatement interface {
	iLoadStatement()
	iStatement()
	SQLNode
}

// Load represents a LOAD statement
type Load struct {
	Local     BoolVal
	Infile    string
	Table     TableName
	Partition Partitions
	Charset   string
	*Fields
	*Lines
	IgnoreNum *SQLVal
	Columns
}

func (*Load) iLoadStatement() {}

func (node *Load) Format(buf *TrackedBuffer) {
	local := ""
	if node.Local {
		local = "local "
	}
	charset := ""
	if node.Charset != "" {
		charset = " character set " + node.Charset
	}

	ignore := ""
	if node.IgnoreNum != nil {
		ignore = fmt.Sprintf(" ignore %v lines", node.IgnoreNum)
	}

	if node.IgnoreNum == nil && node.Columns != nil {
		ignore = " "
	} else if node.IgnoreNum != nil && node.Columns != nil {
		ignore += " "
	}

	buf.Myprintf("load data %sinfile '%s' into table %s%v%s%v%v%s%v", local, node.Infile, node.Table.String(),
		node.Partition, charset, node.Fields, node.Lines, ignore, node.Columns)
}

func (node *Load) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Local,
		node.Table,
		node.Partition,
		node.Fields,
		node.Lines,
		node.IgnoreNum,
		node.Columns,
	)
}

type Fields struct {
	TerminatedBy *SQLVal
	*EnclosedBy
	EscapedBy *SQLVal
	SQLNode
}

func (node *Fields) Format(buf *TrackedBuffer) {
	if node == nil {
		return
	}

	terminated := ""
	if node.TerminatedBy != nil {
		terminated = "terminated by " + "'" + string(node.TerminatedBy.Val) + "'"
	}

	escaped := ""
	if node.EscapedBy != nil {
		escaped = " escaped by " + "'" + string(node.EscapedBy.Val) + "'"
	}

	buf.Myprintf(" fields %s%v%s", terminated, node.EnclosedBy, escaped)
}

type EnclosedBy struct {
	Optionally BoolVal
	Delim      *SQLVal
	SQLNode
}

func (node *EnclosedBy) Format(buf *TrackedBuffer) {
	if node == nil {
		return
	}

	enclosed := "enclosed by " + "'" + string(node.Delim.Val) + "'"
	if node.Optionally {
		enclosed = " optionally " + enclosed
	} else {
		enclosed = " " + enclosed
	}

	buf.Myprintf(enclosed)
}

type Lines struct {
	StartingBy   *SQLVal
	TerminatedBy *SQLVal
	SQLNode
}

func (node *Lines) Format(buf *TrackedBuffer) {
	if node == nil {
		return
	}

	starting := ""
	if node.StartingBy != nil {
		starting = " starting by " + "'" + string(node.StartingBy.Val) + "'"
	}

	terminated := ""
	if node.TerminatedBy != nil {
		terminated = " terminated by " + "'" + string(node.TerminatedBy.Val) + "'"
	}

	buf.Myprintf(" lines%s%s", starting, terminated)
}

// BeginEndBlock represents a BEGIN .. END block with one or more statements nested within
type BeginEndBlock struct {
	Statements Statements
}

func (b *BeginEndBlock) Format(buf *TrackedBuffer) {
	buf.Myprintf("begin\n")
	for _, s := range b.Statements {
		buf.Myprintf("%v;\n", s)
	}
	buf.Myprintf("end")
}

func (b *BeginEndBlock) walkSubtree(visit Visit) error {
	if b == nil {
		return nil
	}
	for _, s := range b.Statements {
		if err := Walk(visit, s); err != nil {
			return err
		}
	}
	return nil
}

// CaseStatement represents a CASE .. WHEN .. ELSE statement in a stored procedure / trigger
type CaseStatement struct {
	Expr  Expr                // The case expression to switch on
	Cases []CaseStatementCase // The set of WHEN values and attached statements
	Else  Statements          // The set of statements for the ELSE clause
}

// CaseStatementCase represents a single WHEN .. THEN clause in a CaseStatement
type CaseStatementCase struct {
	Case       Expr       // The expression to match for this WHEN clause to match
	Statements Statements // The list of statements to execute in the case of a match with Case
}

func (c *CaseStatement) Format(buf *TrackedBuffer) {
	buf.Myprintf("case %v\n", c.Expr)
	for _, cas := range c.Cases {
		buf.Myprintf("when %v then ", cas.Case)
		for i, s := range cas.Statements {
			if i != 0 {
				buf.Myprintf("; ")
			}
			buf.Myprintf("%v", s)
		}
		buf.Myprintf(";\n")
	}

	if len(c.Else) > 0 {
		buf.Myprintf("else ")
		for i, s := range c.Else {
			if i != 0 {
				buf.Myprintf("; ")
			}
			buf.Myprintf("%v", s)
		}
		buf.Myprintf(";\n")
	}

	buf.Myprintf("end case")
}

func (c *CaseStatement) walkSubtree(visit Visit) error {
	if c == nil {
		return nil
	}
	for _, cas := range c.Cases {
		for _, s := range cas.Statements {
			if err := Walk(visit, s); err != nil {
				return err
			}
		}
	}
	for _, s := range c.Else {
		if err := Walk(visit, s); err != nil {
			return err
		}
	}
	return nil
}

// IfStatement represents an IF .. THEN .. ELSE statement in a stored procedure / trigger.
type IfStatement struct {
	Conditions []IfStatementCondition // The initial IF condition, followed by any ELSEIF conditions, in order.
	Else       Statements             // The statements of the ELSE clause, if any
}

// IfStatementCondition represents a single IF / ELSEIF branch in an IfStatement
type IfStatementCondition struct {
	Expr       Expr
	Statements Statements
}

func (i *IfStatement) Format(buf *TrackedBuffer) {
	for j, c := range i.Conditions {
		if j == 0 {
			buf.Myprintf("if %v then ", c.Expr)
		} else {
			buf.Myprintf("elseif %v then ", c.Expr)
		}
		for k, s := range c.Statements {
			if k > 0 {
				buf.Myprintf("; ")
			}
			buf.Myprintf("%v", s)
		}
		buf.Myprintf(";\n")
	}

	if len(i.Else) > 0 {
		buf.Myprintf("else ")
		for j, s := range i.Else {
			if j > 0 {
				buf.Myprintf("; ")
			}
			buf.Myprintf("%v", s)
		}
		buf.Myprintf(";\n")
	}

	buf.Myprintf("end if")
}

func (i *IfStatement) walkSubtree(visit Visit) error {
	if i == nil {
		return nil
	}

	for _, c := range i.Conditions {
		for _, s := range c.Statements {
			if err := Walk(visit, s); err != nil {
				return err
			}
		}
	}
	for _, s := range i.Else {
		if err := Walk(visit, s); err != nil {
			return err
		}
	}

	return nil
}

// Declare represents the DECLARE statement
type Declare struct {
	Condition *DeclareCondition
	Cursor    *DeclareCursor
	Handler   *DeclareHandler
	Variables *DeclareVariables
}

// DeclareHandlerAction represents the action for the handler
type DeclareHandlerAction string

const (
	DeclareHandlerAction_Continue DeclareHandlerAction = "continue"
	DeclareHandlerAction_Exit     DeclareHandlerAction = "exit"
	DeclareHandlerAction_Undo     DeclareHandlerAction = "undo"
)

// DeclareHandlerConditionValue represents the condition values for a handler
type DeclareHandlerConditionValue string

const (
	DeclareHandlerCondition_MysqlErrorCode DeclareHandlerConditionValue = "mysql_err_code"
	DeclareHandlerCondition_SqlState       DeclareHandlerConditionValue = "sqlstate"
	DeclareHandlerCondition_ConditionName  DeclareHandlerConditionValue = "condition_name"
	DeclareHandlerCondition_SqlWarning     DeclareHandlerConditionValue = "sqlwarning"
	DeclareHandlerCondition_NotFound       DeclareHandlerConditionValue = "not_found"
	DeclareHandlerCondition_SqlException   DeclareHandlerConditionValue = "sqlexception"
)

// DeclareHandlerCondition represents the conditions for a handler
type DeclareHandlerCondition struct {
	ValueType      DeclareHandlerConditionValue
	MysqlErrorCode *SQLVal
	String         string // May hold either the SqlState or condition name
}

// DeclareCondition represents the DECLARE CONDITION statement
type DeclareCondition struct {
	Name           string
	SqlStateValue  string
	MysqlErrorCode *SQLVal
}

// DeclareCursor represents the DECLARE CURSOR statement
type DeclareCursor struct {
	Name       string
	SelectStmt SelectStatement
}

// DeclareHandler represents the DECLARE HANDLER statement
type DeclareHandler struct {
	Action          DeclareHandlerAction
	ConditionValues []DeclareHandlerCondition
	Statement       Statement
}

// DeclareVariables represents the DECLARE statement for declaring variables
type DeclareVariables struct {
	Names   []ColIdent
	VarType ColumnType
}

func (d *Declare) Format(buf *TrackedBuffer) {
	if d.Condition != nil {
		buf.Myprintf("declare %s condition for ", d.Condition.Name)
		if d.Condition.SqlStateValue != "" {
			buf.Myprintf("sqlstate value '%s'", d.Condition.SqlStateValue)
		} else {
			buf.Myprintf("%v", d.Condition.MysqlErrorCode)
		}
	} else if d.Cursor != nil {
		buf.Myprintf("declare %s cursor for %v", d.Cursor.Name, d.Cursor.SelectStmt)
	} else if d.Handler != nil {
		buf.Myprintf("declare %s handler for", string(d.Handler.Action))
		for i, condition := range d.Handler.ConditionValues {
			if i > 0 {
				buf.Myprintf(",")
			}
			switch condition.ValueType {
			case DeclareHandlerCondition_MysqlErrorCode:
				buf.Myprintf(" %v", condition.MysqlErrorCode)
			case DeclareHandlerCondition_SqlState:
				buf.Myprintf(" sqlstate value '%s'", condition.String)
			case DeclareHandlerCondition_ConditionName:
				buf.Myprintf(" %s", condition.String)
			case DeclareHandlerCondition_SqlWarning:
				buf.Myprintf(" sqlwarning")
			case DeclareHandlerCondition_NotFound:
				buf.Myprintf(" not found")
			case DeclareHandlerCondition_SqlException:
				buf.Myprintf(" sqlexception")
			default:
				panic(fmt.Errorf("unknown DECLARE HANDLER condition: %s", string(condition.ValueType)))
			}
		}
		buf.Myprintf(" %v", d.Handler.Statement)
	} else if d.Variables != nil {
		buf.Myprintf("declare")
		for i, varName := range d.Variables.Names {
			if i > 0 {
				buf.Myprintf(",")
			}
			buf.Myprintf(" %s", varName.val)
		}
		buf.Myprintf(" %v", &d.Variables.VarType)
	}
}

func (d *Declare) walkSubtree(visit Visit) error {
	if d == nil {
		return nil
	}
	if d.Cursor != nil {
		if err := Walk(visit, d.Cursor.SelectStmt); err != nil {
			return err
		}
	}
	if d.Handler != nil {
		if err := Walk(visit, d.Handler.Statement); err != nil {
			return err
		}
	}
	if d.Variables != nil {
		for _, colIdent := range d.Variables.Names {
			if err := Walk(visit, colIdent); err != nil {
				return err
			}
		}
		if err := Walk(visit, &d.Variables.VarType); err != nil {
			return err
		}
	}
	return nil
}

// Signal represents the SIGNAL statement
type Signal struct {
	ConditionName string       // Previously declared condition name
	SqlStateValue string       // Always a 5-character string
	Info          []SignalInfo // The list of name-value pairs of signal information provided
}

// SignalInfo represents a piece of information for a SIGNAL statement
type SignalInfo struct {
	ConditionItemName SignalConditionItemName
	Value             *SQLVal
}

// SignalConditionItemName represents the item name for the set conditions of a SIGNAL statement.
type SignalConditionItemName string

const (
	SignalConditionItemName_ClassOrigin       SignalConditionItemName = "class_origin"
	SignalConditionItemName_SubclassOrigin    SignalConditionItemName = "subclass_origin"
	SignalConditionItemName_MessageText       SignalConditionItemName = "message_text"
	SignalConditionItemName_MysqlErrno        SignalConditionItemName = "mysql_errno"
	SignalConditionItemName_ConstraintCatalog SignalConditionItemName = "constraint_catalog"
	SignalConditionItemName_ConstraintSchema  SignalConditionItemName = "constraint_schema"
	SignalConditionItemName_ConstraintName    SignalConditionItemName = "constraint_name"
	SignalConditionItemName_CatalogName       SignalConditionItemName = "catalog_name"
	SignalConditionItemName_SchemaName        SignalConditionItemName = "schema_name"
	SignalConditionItemName_TableName         SignalConditionItemName = "table_name"
	SignalConditionItemName_ColumnName        SignalConditionItemName = "column_name"
	SignalConditionItemName_CursorName        SignalConditionItemName = "cursor_name"
)

func (s *Signal) Format(buf *TrackedBuffer) {
	if s.ConditionName != "" {
		buf.Myprintf("signal %s", s.ConditionName)
	} else {
		buf.Myprintf("signal sqlstate value '%s'", s.SqlStateValue)
	}
	if len(s.Info) > 0 {
		buf.Myprintf(" set ")
		for i, info := range s.Info {
			if i > 0 {
				buf.Myprintf(", ")
			}
			buf.Myprintf("%s = %v", string(info.ConditionItemName), info.Value)
		}
	}
}

func (s *Signal) walkSubtree(visit Visit) error {
	if s == nil {
		return nil
	}
	for _, info := range s.Info {
		if err := Walk(visit, info.Value); err != nil {
			return err
		}
	}
	return nil
}

// Resignal represents the RESIGNAL statement
type Resignal struct {
	Signal
}

func (s *Resignal) Format(buf *TrackedBuffer) {
	buf.Myprintf("resignal")
	if s.ConditionName != "" {
		buf.Myprintf(" %s", s.ConditionName)
	} else if s.SqlStateValue != "" {
		buf.Myprintf(" sqlstate value '%s'", s.SqlStateValue)
	}
	if len(s.Info) > 0 {
		buf.Myprintf(" set ")
		for i, info := range s.Info {
			if i > 0 {
				buf.Myprintf(", ")
			}
			buf.Myprintf("%s = %v", string(info.ConditionItemName), info.Value)
		}
	}
}

// Call represents the CALL statement
type Call struct {
	ProcName ProcedureName
	Params   []Expr
}

func (c *Call) Format(buf *TrackedBuffer) {
	buf.Myprintf("call %s", c.ProcName.String())
	if len(c.Params) > 0 {
		buf.Myprintf("(")
		for i, param := range c.Params {
			if i > 0 {
				buf.Myprintf(", ")
			}
			buf.Myprintf("%v", param)
		}
		buf.Myprintf(")")
	}
}

func (c *Call) walkSubtree(visit Visit) error {
	if c == nil {
		return nil
	}
	for _, expr := range c.Params {
		if err := Walk(visit, expr); err != nil {
			return err
		}
	}
	return nil
}

// Stream represents a SELECT statement.
type Stream struct {
	Comments   Comments
	SelectExpr SelectExpr
	Table      TableName
}

// Format formats the node.
func (node *Stream) Format(buf *TrackedBuffer) {
	buf.Myprintf("stream %v%v from %v",
		node.Comments, node.SelectExpr, node.Table)
}

func (node *Stream) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Comments,
		node.SelectExpr,
		node.Table,
	)
}

// Insert represents an INSERT or REPLACE statement.
// Per the MySQL docs, http://dev.mysql.com/doc/refman/5.7/en/replace.html
// Replace is the counterpart to `INSERT IGNORE`, and works exactly like a
// normal INSERT except if the row exists. In that case it first deletes
// the row and re-inserts with new values. For that reason we keep it as an Insert struct.
type Insert struct {
	Action     string
	Comments   Comments
	Ignore     string
	Table      TableName
	With       *With
	Partitions Partitions
	Columns    Columns
	Rows       InsertRows
	OnDup      OnDup
}

const (
	ReplaceStr = "replace"
)

// Format formats the node.
func (node *Insert) Format(buf *TrackedBuffer) {
	buf.Myprintf("%v%s %v%sinto %v%v%v %v%v",
		node.With,
		node.Action,
		node.Comments, node.Ignore,
		node.Table, node.Partitions, node.Columns, node.Rows, node.OnDup)
}

func (node *Insert) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Comments,
		node.Table,
		node.Columns,
		node.Rows,
		node.OnDup,
		node.With,
	)
}

// InsertRows represents the rows for an INSERT statement.
type InsertRows interface {
	iInsertRows()
	SQLNode
}

func (*Select) iInsertRows()      {}
func (*Union) iInsertRows()       {}
func (Values) iInsertRows()       {}
func (*ParenSelect) iInsertRows() {}

// Update represents an UPDATE statement.
// If you add fields here, consider adding them to calls to validateUnshardedRoute.
type Update struct {
	Comments   Comments
	Ignore     string
	TableExprs TableExprs
	With       *With
	Exprs      AssignmentExprs
	Where      *Where
	OrderBy    OrderBy
	Limit      *Limit
}

// Format formats the node.
func (node *Update) Format(buf *TrackedBuffer) {
	buf.Myprintf("%vupdate %v%s%v set %v%v%v%v",
		node.With, node.Comments, node.Ignore, node.TableExprs,
		node.Exprs, node.Where, node.OrderBy, node.Limit)
}

func (node *Update) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Comments,
		node.TableExprs,
		node.Exprs,
		node.Where,
		node.OrderBy,
		node.Limit,
		node.With,
	)
}

// Delete represents a DELETE statement.
// If you add fields here, consider adding them to calls to validateUnshardedRoute.
type Delete struct {
	Comments   Comments
	Targets    TableNames
	TableExprs TableExprs
	With       *With
	Partitions Partitions
	Where      *Where
	OrderBy    OrderBy
	Limit      *Limit
}

// Format formats the node.
func (node *Delete) Format(buf *TrackedBuffer) {
	buf.Myprintf("%vdelete %v", node.With, node.Comments)
	if node.Targets != nil {
		buf.Myprintf("%v ", node.Targets)
	}
	buf.Myprintf("from %v%v%v%v%v", node.TableExprs, node.Partitions, node.Where, node.OrderBy, node.Limit)
}

func (node *Delete) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Comments,
		node.Targets,
		node.TableExprs,
		node.Where,
		node.OrderBy,
		node.Limit,
		node.With,
	)
}

// Set represents a SET statement.
type Set struct {
	Comments Comments
	Exprs    SetVarExprs
}

// Show.Scope
const (
	SessionStr = "session"
	GlobalStr  = "global"
)

// Format formats the node.
func (node *Set) Format(buf *TrackedBuffer) {
	if len(node.Exprs) > 0 && node.Exprs[0].Name.String() == TransactionStr {
		switch node.Exprs[0].Scope {
		case SetScope_None:
			buf.Myprintf("set %vtransaction", node.Comments)
		case SetScope_Session:
			buf.Myprintf("set %vsession transaction", node.Comments)
		case SetScope_Global:
			buf.Myprintf("set %vglobal transaction", node.Comments)
		}
		for _, transaction := range node.Exprs {
			if sqlVal, ok := transaction.Expr.(*SQLVal); ok {
				buf.Myprintf(" %s", string(sqlVal.Val))
			} else {
				buf.Myprintf(" %v", transaction.Expr)
			}
		}
	} else {
		buf.Myprintf("set %v%v", node.Comments, node.Exprs)
	}
}

func (node *Set) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Comments,
		node.Exprs,
	)
}

type CharsetAndCollate struct {
	Type      string //Charset = true, Collate = false
	Value     string
	IsDefault bool
}

// DBDDL represents a CREATE, DROP database statement.
type DBDDL struct {
	Action         string
	DBName         string
	IfNotExists    bool
	IfExists       bool
	CharsetCollate []*CharsetAndCollate
}

// Format formats the node.
func (node *DBDDL) Format(buf *TrackedBuffer) {
	switch node.Action {
	case CreateStr, AlterStr:
		exists := ""
		if node.IfNotExists {
			exists = " if not exists"
		}
		dbname := ""
		if len(node.DBName) > 0 {
			dbname = fmt.Sprintf(" %s", node.DBName)
		}
		charsetCollateStr := ""
		for _, obj := range node.CharsetCollate {
			typeStr := strings.ToLower(obj.Type)
			charsetDef := ""
			if obj.IsDefault {
				charsetDef = " default"
			}
			charsetCollateStr += fmt.Sprintf("%s %s %s", charsetDef, typeStr, obj.Value)
		}

		buf.WriteString(fmt.Sprintf("%s database%s%s%s", node.Action, exists, dbname, charsetCollateStr))
	case DropStr:
		exists := ""
		if node.IfExists {
			exists = " if exists"
		}
		buf.WriteString(fmt.Sprintf("%s database%s %v", node.Action, exists, node.DBName))
	}
}

type ViewSpec struct {
	ViewName  TableName
	Algorithm string
	Definer   string
	Security  string
	ViewExpr  SelectStatement
}

type TriggerSpec struct {
	TrigName TriggerName
	Definer  string
	Time     string // BeforeStr, AfterStr
	Event    string // UpdateStr, InsertStr, DeleteStr
	Order    *TriggerOrder
	Body     Statement
}

type TriggerOrder struct {
	PrecedesOrFollows string // PrecedesStr, FollowsStr
	OtherTriggerName  string
}

type ProcedureSpec struct {
	ProcName        ProcedureName
	Definer         string
	Params          []ProcedureParam
	Characteristics []Characteristic
	Body            Statement
}

type ProcedureParamDirection string

const (
	ProcedureParamDirection_In    ProcedureParamDirection = "in"
	ProcedureParamDirection_Inout ProcedureParamDirection = "inout"
	ProcedureParamDirection_Out   ProcedureParamDirection = "out"
)

type ProcedureParam struct {
	Direction ProcedureParamDirection
	Name      string
	Type      ColumnType
}

type CharacteristicValue string

const (
	CharacteristicValue_Comment            CharacteristicValue = "comment"
	CharacteristicValue_LanguageSql        CharacteristicValue = "language sql"
	CharacteristicValue_Deterministic      CharacteristicValue = "deterministic"
	CharacteristicValue_NotDeterministic   CharacteristicValue = "not deterministic"
	CharacteristicValue_ContainsSql        CharacteristicValue = "contains sql"
	CharacteristicValue_NoSql              CharacteristicValue = "no sql"
	CharacteristicValue_ReadsSqlData       CharacteristicValue = "reads sql data"
	CharacteristicValue_ModifiesSqlData    CharacteristicValue = "modifies sql data"
	CharacteristicValue_SqlSecurityDefiner CharacteristicValue = "sql security definer"
	CharacteristicValue_SqlSecurityInvoker CharacteristicValue = "sql security invoker"
)

type Characteristic struct {
	Type    CharacteristicValue
	Comment string
}

func (c Characteristic) String() string {
	if c.Type == CharacteristicValue_Comment {
		return fmt.Sprintf("comment '%s'", c.Comment)
	}
	return string(c.Type)
}

// MultiAlterDDL represents multiple ALTER statements on a single table.
type MultiAlterDDL struct {
	Table      TableName
	Statements []*DDL
}

var _ SQLNode = (*MultiAlterDDL)(nil)

// Format implements SQLNode.
func (m *MultiAlterDDL) Format(buf *TrackedBuffer) {
	buf.Myprintf("alter table %v", m.Table)
	for i, ddl := range m.Statements {
		if i > 0 {
			buf.Myprintf(",")
		}
		ddl.alterFormat(buf)
	}
}

// walkSubtree implements SQLNode.
func (m *MultiAlterDDL) walkSubtree(visit Visit) error {
	for _, ddl := range m.Statements {
		err := ddl.walkSubtree(visit)
		if err != nil {
			return err
		}
	}
	return nil
}

// DDL represents a CREATE, ALTER, DROP, RENAME, TRUNCATE or ANALYZE statement.
type DDL struct {
	Action string

	// Set for column alter statements
	ColumnAction string

	// Set for constraint alter statements
	ConstraintAction string

	// Set for column add / drop / rename statements
	Column ColIdent

	// Set for column add / drop / modify statements that specify a column order
	ColumnOrder *ColumnOrder

	// Set for column rename
	ToColumn ColIdent

	// FromTables is set if Action is RenameStr or DropStr.
	FromTables TableNames

	// ToTables is set if Action is RenameStr.
	ToTables TableNames

	// Table is set if Action is other than RenameStr or DropStr.
	Table TableName

	// ViewSpec is set for CREATE VIEW operations.
	ViewSpec *ViewSpec

	// This exposes the start and end index of the string that makes up the sub statement of the query given.
	// Meaning is specific to the different kinds of statements with sub statements, e.g. views, trigger definitions.
	// For statements defined within a MySQL special comment (/*! */), we have to fudge the offset a bit because we won't
	// get the final lexer position token until after the comment close.
	SpecialCommentMode        bool
	SubStatementPositionStart int
	SubStatementPositionEnd   int

	// FromViews is set if Action is DropStr.
	FromViews TableNames

	// The following fields are set if a DDL was fully analyzed.
	IfExists    bool
	IfNotExists bool
	OrReplace   bool

	// TableSpec contains the full table spec in case of a create, or a single column in case of an add, drop, or alter.
	TableSpec     *TableSpec
	OptLike       *OptLike
	PartitionSpec *PartitionSpec

	// AutoIncSpec is set for AddAutoIncStr.
	AutoIncSpec *AutoIncSpec

	// IndexSpec is set for all ALTER operations on an index
	IndexSpec *IndexSpec

	// DefaultSpec is set for SET / DROP DEFAULT operations
	DefaultSpec *DefaultSpec

	// TriggerSpec is set for CREATE / ALTER / DROP trigger operations
	TriggerSpec *TriggerSpec

	// ProcedureSpec is set for CREATE PROCEDURE operations
	ProcedureSpec *ProcedureSpec

	// Temporary is set for CREATE TEMPORARY TABLE operations.
	Temporary bool

	// OptSelect is set for CREATE TABLE <> AS SELECT operations.
	OptSelect *OptSelect
}

// ColumnOrder is used in some DDL statements to specify or change the order of a column in a schema.
type ColumnOrder struct {
	// First is true if this column should be first in the schema
	First bool
	// AfterColumn is set if this column should be after the one named
	AfterColumn ColIdent
}

// DDL strings.
const (
	CreateStr     = "create"
	AlterStr      = "alter"
	AddStr        = "add"
	DropStr       = "drop"
	RenameStr     = "rename"
	ModifyStr     = "modify"
	ChangeStr     = "change"
	TruncateStr   = "truncate"
	FlushStr      = "flush"
	IndexStr      = "index"
	BeforeStr     = "before"
	AfterStr      = "after"
	InsertStr     = "insert"
	UpdateStr     = "update"
	DeleteStr     = "delete"
	FollowsStr    = "follows"
	PrecedesStr   = "precedes"
	AddAutoIncStr = "add auto_increment"
	UniqueStr     = "unique"
	SpatialStr    = "spatial"
	FulltextStr   = "fulltext"
	SetStr        = "set"
	TemporaryStr  = "temporary"
	PrimaryStr    = "primary"
)

// Format formats the node.
// TODO: add newly added fields here
func (node *DDL) Format(buf *TrackedBuffer) {
	switch node.Action {
	case CreateStr:
		if node.ViewSpec != nil {
			view := node.ViewSpec
			afterCreate := ""
			if node.OrReplace {
				afterCreate = "or replace "
			}
			if view.Algorithm != "" {
				afterCreate = fmt.Sprintf("%salgorithm = %s ", afterCreate, strings.ToLower(view.Algorithm))
			}
			if view.Definer != "" {
				afterCreate = fmt.Sprintf("%sdefiner = %s ", afterCreate, view.Definer)
			}
			if view.Security != "" {
				afterCreate = fmt.Sprintf("%ssql security %s ", afterCreate, strings.ToLower(view.Security))
			}
			buf.Myprintf("%s %sview %v as %v", node.Action, afterCreate, view.ViewName, view.ViewExpr)
		} else if node.TriggerSpec != nil {
			trigger := node.TriggerSpec
			triggerDef := ""
			if trigger.Definer != "" {
				triggerDef = fmt.Sprintf("%sdefiner = %s ", triggerDef, trigger.Definer)
			}
			triggerOrder := ""
			if trigger.Order != nil {
				triggerOrder = fmt.Sprintf("%s %s ", trigger.Order.PrecedesOrFollows, trigger.Order.OtherTriggerName)
			}
			triggerName := fmt.Sprintf("%s", trigger.TrigName)
			buf.Myprintf("%s %strigger %s %s %s on %v for each row %s%v",
				node.Action, triggerDef, triggerName, trigger.Time, trigger.Event, node.Table, triggerOrder, trigger.Body)
		} else if node.ProcedureSpec != nil {
			proc := node.ProcedureSpec
			sb := strings.Builder{}
			sb.WriteString("create ")
			if proc.Definer != "" {
				sb.WriteString(fmt.Sprintf("definer = %s ", proc.Definer))
			}
			sb.WriteString(fmt.Sprintf("procedure %s (", proc.ProcName))
			for i, param := range proc.Params {
				if i > 0 {
					sb.WriteString(", ")
				}
				sb.WriteString(string(param.Direction) + " ")
				sb.WriteString(fmt.Sprintf("%s %s", param.Name, param.Type.String()))
			}
			sb.WriteString(")")
			for _, characteristic := range proc.Characteristics {
				sb.WriteString(" " + characteristic.String())
			}
			buf.Myprintf("%s %v", sb.String(), proc.Body)
		} else {
			notExists := ""
			if node.IfNotExists {
				notExists = " if not exists"
			}

			temporary := ""
			if node.Temporary {
				temporary = " " + TemporaryStr
			}

			if node.OptLike != nil {
				buf.Myprintf("%s%s table%s %v %v", node.Action, temporary, notExists, node.Table, node.OptLike)
			} else if node.TableSpec != nil {
				if node.OptSelect == nil {
					buf.Myprintf("%s%s table%s %v %v", node.Action, temporary, notExists, node.Table, node.TableSpec)
				} else {
					buf.Myprintf("%s%s table%s %v %v%v", node.Action, temporary, notExists, node.Table, node.TableSpec, node.OptSelect)
				}
			} else if node.OptSelect != nil {
				buf.Myprintf("%s%s table%s %v %v", node.Action, temporary, notExists, node.Table, node.OptSelect)
			} else {
				buf.Myprintf("%s%s table%s %v", node.Action, temporary, notExists, node.Table)
			}
		}
	case DropStr:
		exists := ""
		if node.IfExists {
			exists = " if exists"
		}
		if len(node.FromViews) > 0 {
			buf.Myprintf("%s view%s %v", node.Action, exists, node.FromViews)
		} else if node.TriggerSpec != nil {
			exists := ""
			if node.IfExists {
				exists = " if exists"
			}
			buf.Myprintf(fmt.Sprintf("%s trigger%s %v", node.Action, exists, node.TriggerSpec.TrigName))
		} else if node.ProcedureSpec != nil {
			exists := ""
			if node.IfExists {
				exists = " if exists"
			}
			buf.Myprintf(fmt.Sprintf("%s procedure%s %v", node.Action, exists, node.ProcedureSpec.ProcName))
		} else {
			buf.Myprintf("%s table%s %v", node.Action, exists, node.FromTables)
		}
	case RenameStr:
		buf.Myprintf("%s table %v to %v", node.Action, node.FromTables[0], node.ToTables[0])
		for i := 1; i < len(node.FromTables); i++ {
			buf.Myprintf(", %v to %v", node.FromTables[i], node.ToTables[i])
		}
	case AlterStr:
		buf.Myprintf("%s table %v", node.Action, node.Table)
		node.alterFormat(buf)
	case FlushStr:
		buf.Myprintf("%s", node.Action)
	case AddAutoIncStr:
		buf.Myprintf("alter vschema on %v add auto_increment %v", node.Table, node.AutoIncSpec)
	default:
		buf.Myprintf("%s table %v", node.Action, node.Table)
	}
}

func (node *DDL) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	for _, t := range node.AffectedTables() {
		if err := Walk(visit, t); err != nil {
			return err
		}
	}
	return nil
}

func (node *DDL) alterFormat(buf *TrackedBuffer) {
	if node.Action == RenameStr {
		buf.Myprintf(" %s to %v", node.Action, node.ToTables[0])
		for i := 1; i < len(node.FromTables); i++ {
			buf.Myprintf(", %v to %v", node.FromTables[i], node.ToTables[i])
		}
	} else if node.PartitionSpec != nil {
		buf.Myprintf(" %v", node.PartitionSpec)
	} else if node.ColumnAction == AddStr {
		after := ""
		if node.ColumnOrder != nil {
			if node.ColumnOrder.First {
				after = " first"
			} else {
				after = " after " + node.ColumnOrder.AfterColumn.String()
			}
		}
		buf.Myprintf(" %s column %v%s", node.ColumnAction, node.TableSpec, after)
	} else if node.ColumnAction == ModifyStr || node.ColumnAction == ChangeStr {
		after := ""
		if node.ColumnOrder != nil {
			if node.ColumnOrder.First {
				after = " first"
			} else {
				after = " after " + node.ColumnOrder.AfterColumn.String()
			}
		}
		buf.Myprintf(" %s column %v %v%s", node.ColumnAction, node.Column, node.TableSpec, after)
	} else if node.ColumnAction == DropStr {
		buf.Myprintf(" %s column %v", node.ColumnAction, node.Column)
	} else if node.ColumnAction == RenameStr {
		buf.Myprintf(" %s column %v to %v", node.ColumnAction, node.Column, node.ToColumn)
	} else if node.IndexSpec != nil {
		buf.Myprintf(" %v", node.IndexSpec)
	} else if node.ConstraintAction == AddStr && node.TableSpec != nil && len(node.TableSpec.Constraints) == 1 {
		switch node.TableSpec.Constraints[0].Details.(type) {
		case *ForeignKeyDefinition, *CheckConstraintDefinition:
			buf.Myprintf(" add %v", node.TableSpec.Constraints[0])
		}
	} else if node.ConstraintAction == DropStr && node.TableSpec != nil && len(node.TableSpec.Constraints) == 1 {
		switch node.TableSpec.Constraints[0].Details.(type) {
		case *ForeignKeyDefinition:
			buf.Myprintf(" drop foreign key %s", node.TableSpec.Constraints[0].Name)
		case *CheckConstraintDefinition:
			buf.Myprintf(" drop check %s", node.TableSpec.Constraints[0].Name)
		default:
			buf.Myprintf(" drop constraint %s", node.TableSpec.Constraints[0].Name)
		}
	} else if node.DefaultSpec != nil {
		buf.Myprintf(" %v", node.DefaultSpec)
	}
}

// AffectedTables returns the list table names affected by the DDL.
func (node *DDL) AffectedTables() TableNames {
	if node.Action == RenameStr || node.Action == DropStr {
		list := make(TableNames, 0, len(node.FromTables)+len(node.ToTables))
		list = append(list, node.FromTables...)
		list = append(list, node.ToTables...)
		return list
	}
	return TableNames{node.Table}
}

// Partition strings
const (
	ReorganizeStr = "reorganize partition"
)

// OptLike works for create table xxx like xxx
type OptLike struct {
	LikeTable TableName
}

// Format formats the node.
func (node *OptLike) Format(buf *TrackedBuffer) {
	buf.Myprintf("like %v", node.LikeTable)
}

func (node *OptLike) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(visit, node.LikeTable)
}

type OptSelect struct {
	Select SelectStatement
}

// Format formats the node.
func (node *OptSelect) Format(buf *TrackedBuffer) {
	buf.Myprintf("as %v", node.Select) // purposely display the AS
}

func (node *OptSelect) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(visit, node.Select)
}

// PartitionSpec describe partition actions (for alter and create)
type PartitionSpec struct {
	Action      string
	Name        ColIdent
	Definitions []*PartitionDefinition
}

// Format formats the node.
func (node *PartitionSpec) Format(buf *TrackedBuffer) {
	switch node.Action {
	case ReorganizeStr:
		buf.Myprintf("%s %v into (", node.Action, node.Name)
		var prefix string
		for _, pd := range node.Definitions {
			buf.Myprintf("%s%v", prefix, pd)
			prefix = ", "
		}
		buf.Myprintf(")")
	default:
		panic("unimplemented")
	}
}

func (node *PartitionSpec) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	if err := Walk(visit, node.Name); err != nil {
		return err
	}
	for _, def := range node.Definitions {
		if err := Walk(visit, def); err != nil {
			return err
		}
	}
	return nil
}

// PartitionDefinition describes a very minimal partition definition
type PartitionDefinition struct {
	Name     ColIdent
	Limit    Expr
	Maxvalue bool
}

// Format formats the node
func (node *PartitionDefinition) Format(buf *TrackedBuffer) {
	if !node.Maxvalue {
		buf.Myprintf("partition %v values less than (%v)", node.Name, node.Limit)
	} else {
		buf.Myprintf("partition %v values less than (maxvalue)", node.Name)
	}
}

func (node *PartitionDefinition) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Name,
		node.Limit,
	)
}

// TableSpec describes the structure of a table from a CREATE TABLE statement
type TableSpec struct {
	Columns     []*ColumnDefinition
	Indexes     []*IndexDefinition
	Constraints []*ConstraintDefinition
	Options     string
}

// Format formats the node.
func (ts *TableSpec) Format(buf *TrackedBuffer) {
	buf.Myprintf("(\n")
	for i, col := range ts.Columns {
		if i == 0 {
			buf.Myprintf("\t%v", col)
		} else {
			buf.Myprintf(",\n\t%v", col)
		}
	}
	for _, idx := range ts.Indexes {
		buf.Myprintf(",\n\t%v", idx)
	}
	for _, c := range ts.Constraints {
		buf.Myprintf(",\n\t%v", c)
	}

	buf.Myprintf("\n)%s", strings.Replace(ts.Options, ", ", ",\n  ", -1))
}

// AddColumn appends the given column to the list in the spec
func (ts *TableSpec) AddColumn(cd *ColumnDefinition) {
	ts.Columns = append(ts.Columns, cd)
}

// AddIndex appends the given index to the list in the spec
func (ts *TableSpec) AddIndex(id *IndexDefinition) {
	ts.Indexes = append(ts.Indexes, id)
}

// AddConstraint appends the given index to the list in the spec
func (ts *TableSpec) AddConstraint(cd *ConstraintDefinition) {
	ts.Constraints = append(ts.Constraints, cd)
}

func (ts *TableSpec) walkSubtree(visit Visit) error {
	if ts == nil {
		return nil
	}

	for _, n := range ts.Columns {
		if err := Walk(visit, n); err != nil {
			return err
		}
	}

	for _, n := range ts.Indexes {
		if err := Walk(visit, n); err != nil {
			return err
		}
	}

	for _, n := range ts.Constraints {
		if err := Walk(visit, n); err != nil {
			return err
		}
	}

	return nil
}

// ColumnDefinition describes a column in a CREATE TABLE statement
type ColumnDefinition struct {
	Name ColIdent
	Type ColumnType
}

// Format formats the node.
func (col *ColumnDefinition) Format(buf *TrackedBuffer) {
	buf.Myprintf("%v %v", col.Name, &col.Type)
}

func (col *ColumnDefinition) walkSubtree(visit Visit) error {
	if col == nil {
		return nil
	}
	return Walk(
		visit,
		col.Name,
		&col.Type,
	)
}

// ColumnType represents a sql type in a CREATE TABLE or ALTER TABLE statement
// All optional fields are nil if not specified
type ColumnType struct {
	// The base type string
	Type string

	// Generic field options.
	Null          BoolVal
	NotNull       BoolVal
	Autoincrement BoolVal
	Default       Expr
	OnUpdate      Expr
	Comment       *SQLVal
	sawnull       bool
	sawai         bool

	// Numeric field options
	Length   *SQLVal
	Unsigned BoolVal
	Zerofill BoolVal
	Scale    *SQLVal

	// Text field options
	Charset       string
	Collate       string
	BinaryCollate bool

	// Enum values
	EnumValues []string

	// Key specification
	KeyOpt ColumnKeyOption

	// Generated columns
	GeneratedExpr Expr    // The expression used to generate this column
	Stored        BoolVal // Default is Virtual (not stored)

	// For spatial types
	SRID *SQLVal

	// For json_table
	Path    string
	Exists  bool
	OnEmpty Expr
}

func (ct *ColumnType) merge(other ColumnType) error {
	if other.sawnull {
		ct.sawnull = true
		ct.Null = other.Null
		ct.NotNull = other.NotNull
	}

	if other.Default != nil {
		if ct.Default != nil {
			return errors.New("cannot include DEFAULT more than once")
		}
		ct.Default = other.Default
	}

	if other.OnUpdate != nil {
		if ct.OnUpdate != nil {
			return errors.New("cannot include ON UPDATE more than once")
		}
		ct.OnUpdate = other.OnUpdate
	}

	if other.sawai {
		if ct.sawai {
			return errors.New("cannot include AUTO_INCREMENT more than once")
		}
		ct.sawai = true
		ct.Autoincrement = other.Autoincrement
	}

	if other.KeyOpt != colKeyNone {
		if ct.KeyOpt != colKeyNone {
			return errors.New("cannot include more than one key option for a column definition")
		}
		ct.KeyOpt = other.KeyOpt
	}

	if other.Comment != nil {
		if ct.Comment != nil {
			return errors.New("cannot include more than one comment for a column definition")
		}
		ct.Comment = other.Comment
	}

	if other.GeneratedExpr != nil {
		// Generated expression already defined for column
		if ct.GeneratedExpr != nil {
			return errors.New("cannot defined GENERATED expression more than once")
		}
		ct.GeneratedExpr = other.GeneratedExpr
	}

	if other.Stored {
		ct.Stored = true
	}

	if other.SRID != nil {
		if ct.SRID != nil {
			return errors.New("cannot include SRID more than once")
		}
		if ct.Type != "" && ct.SQLType() != sqltypes.Geometry {
			return errors.New("cannot define SRID for non spatial types")
		}
		ct.SRID = other.SRID
	}

	if other.Path != "" {
		if ct.Path != "" {
			return errors.New("cannot include PATH more than once")
		}
		ct.Path = other.Path
	}

	if other.Charset != "" {
		if ct.Charset != "" {
			return errors.New("cannot include CHARACTER SET more than once")
		}
		ct.Charset = other.Charset
	}

	if other.Collate != "" {
		if ct.Collate != "" {
			return errors.New("cannot include COLLATE more than once")
		}
		ct.Collate = other.Collate
	}

	if other.BinaryCollate == true {
		if ct.BinaryCollate == true {
			return errors.New("cannot include BINARY more than once")
		}
		ct.BinaryCollate = other.BinaryCollate
	}

	return nil
}

// Format returns a canonical string representation of the type and all relevant options
func (ct *ColumnType) Format(buf *TrackedBuffer) {
	buf.Myprintf("%s", ct.Type)

	if ct.Length != nil && ct.Scale != nil {
		buf.Myprintf("(%v,%v)", ct.Length, ct.Scale)

	} else if ct.Length != nil {
		buf.Myprintf("(%v)", ct.Length)
	}

	if len(ct.EnumValues) > 0 {
		buf.Myprintf("('%s')", strings.Join(ct.EnumValues, "', '"))
	}

	opts := make([]string, 0, 16)
	if ct.Unsigned {
		opts = append(opts, keywordStrings[UNSIGNED])
	}
	if ct.Zerofill {
		opts = append(opts, keywordStrings[ZEROFILL])
	}
	if ct.Charset != "" {
		opts = append(opts, keywordStrings[CHARACTER], keywordStrings[SET], ct.Charset)
	}
	if ct.BinaryCollate {
		opts = append(opts, keywordStrings[BINARY])
	}
	if ct.Collate != "" {
		opts = append(opts, keywordStrings[COLLATE], ct.Collate)
	}
	if ct.NotNull {
		opts = append(opts, keywordStrings[NOT], keywordStrings[NULL])
	}
	if ct.SRID != nil {
		opts = append(opts, keywordStrings[SRID], String(ct.SRID))
	}
	if ct.Default != nil {
		opts = append(opts, keywordStrings[DEFAULT], String(ct.Default))
	}
	if ct.OnUpdate != nil {
		opts = append(opts, keywordStrings[ON], keywordStrings[UPDATE], String(ct.OnUpdate))
	}
	if ct.Autoincrement {
		opts = append(opts, keywordStrings[AUTO_INCREMENT])
	}
	if ct.Comment != nil {
		opts = append(opts, keywordStrings[COMMENT_KEYWORD], String(ct.Comment))
	}
	if ct.KeyOpt == colKeyPrimary {
		opts = append(opts, keywordStrings[PRIMARY], keywordStrings[KEY])
	}
	if ct.KeyOpt == colKeyUnique {
		opts = append(opts, keywordStrings[UNIQUE])
	}
	if ct.KeyOpt == colKeyUniqueKey {
		opts = append(opts, keywordStrings[UNIQUE], keywordStrings[KEY])
	}
	if ct.KeyOpt == colKeySpatialKey {
		opts = append(opts, keywordStrings[SPATIAL], keywordStrings[KEY])
	}
	if ct.KeyOpt == colKey {
		opts = append(opts, keywordStrings[KEY])
	}
	if ct.KeyOpt == colKeyFulltextKey {
		opts = append(opts, keywordStrings[FULLTEXT])
	}
	if ct.GeneratedExpr != nil {
		opts = append(opts, keywordStrings[GENERATED], keywordStrings[ALWAYS], keywordStrings[AS], "("+String(ct.GeneratedExpr)+")")
		if ct.Stored {
			opts = append(opts, keywordStrings[STORED])
		} else {
			opts = append(opts, keywordStrings[VIRTUAL])
		}
	}
	if ct.Path != "" {
		opts = append(opts, keywordStrings[PATH], `"`+ct.Path+`"`)
	}

	if len(opts) != 0 {
		buf.Myprintf(" %s", strings.Join(opts, " "))
	}
}

// String returns a canonical string representation of the type and all relevant options
func (ct *ColumnType) String() string {
	buf := NewTrackedBuffer(nil)
	ct.Format(buf)
	return buf.String()
}

// DescribeType returns the abbreviated type information as required for
// describe table
func (ct *ColumnType) DescribeType() string {
	buf := NewTrackedBuffer(nil)
	buf.Myprintf("%s", ct.Type)
	if ct.Length != nil && ct.Scale != nil {
		buf.Myprintf("(%v,%v)", ct.Length, ct.Scale)
	} else if ct.Length != nil {
		buf.Myprintf("(%v)", ct.Length)
	}

	opts := make([]string, 0, 16)
	if ct.Unsigned {
		opts = append(opts, keywordStrings[UNSIGNED])
	}
	if ct.Zerofill {
		opts = append(opts, keywordStrings[ZEROFILL])
	}
	if len(opts) != 0 {
		buf.Myprintf(" %s", strings.Join(opts, " "))
	}
	return buf.String()
}

// SQLType returns the sqltypes type code for the given column
func (ct *ColumnType) SQLType() querypb.Type {
	switch strings.ToLower(ct.Type) {
	case keywordStrings[TINYINT]:
		if ct.Unsigned {
			return sqltypes.Uint8
		}
		return sqltypes.Int8
	case keywordStrings[SMALLINT]:
		if ct.Unsigned {
			return sqltypes.Uint16
		}
		return sqltypes.Int16
	case keywordStrings[MEDIUMINT]:
		if ct.Unsigned {
			return sqltypes.Uint24
		}
		return sqltypes.Int24
	case keywordStrings[INT]:
		fallthrough
	case keywordStrings[INTEGER]:
		if ct.Unsigned {
			return sqltypes.Uint32
		}
		return sqltypes.Int32
	case keywordStrings[BIGINT]:
		if ct.Unsigned {
			return sqltypes.Uint64
		}
		return sqltypes.Int64
	case keywordStrings[BOOL], keywordStrings[BOOLEAN]:
		return sqltypes.Uint8
	case keywordStrings[TEXT]:
		return sqltypes.Text
	case keywordStrings[TINYTEXT]:
		return sqltypes.Text
	case keywordStrings[MEDIUMTEXT]:
		return sqltypes.Text
	case keywordStrings[LONGTEXT]:
		return sqltypes.Text
	case keywordStrings[BLOB]:
		return sqltypes.Blob
	case keywordStrings[TINYBLOB]:
		return sqltypes.Blob
	case keywordStrings[MEDIUMBLOB]:
		return sqltypes.Blob
	case keywordStrings[LONGBLOB]:
		return sqltypes.Blob
	case keywordStrings[CHAR]:
		return sqltypes.Char
	case keywordStrings[VARCHAR]:
		return sqltypes.VarChar
	case keywordStrings[BINARY]:
		return sqltypes.Binary
	case keywordStrings[VARBINARY]:
		return sqltypes.VarBinary
	case keywordStrings[DATE]:
		return sqltypes.Date
	case keywordStrings[TIME]:
		return sqltypes.Time
	case keywordStrings[DATETIME]:
		return sqltypes.Datetime
	case keywordStrings[TIMESTAMP]:
		return sqltypes.Timestamp
	case keywordStrings[YEAR]:
		return sqltypes.Year
	case keywordStrings[FLOAT_TYPE]:
		return sqltypes.Float32
	case keywordStrings[DOUBLE]:
		return sqltypes.Float64
	case keywordStrings[DECIMAL]:
		return sqltypes.Decimal
	case keywordStrings[BIT]:
		return sqltypes.Bit
	case keywordStrings[ENUM]:
		return sqltypes.Enum
	case keywordStrings[SET]:
		return sqltypes.Set
	case keywordStrings[JSON]:
		return sqltypes.TypeJSON
	case keywordStrings[GEOMETRY]:
		return sqltypes.Geometry
	case keywordStrings[POINT]:
		return sqltypes.Geometry
	case keywordStrings[LINESTRING]:
		return sqltypes.Geometry
	case keywordStrings[POLYGON]:
		return sqltypes.Geometry
	case keywordStrings[GEOMETRYCOLLECTION]:
		return sqltypes.Geometry
	case keywordStrings[MULTIPOINT]:
		return sqltypes.Geometry
	case keywordStrings[MULTILINESTRING]:
		return sqltypes.Geometry
	case keywordStrings[MULTIPOLYGON]:
		return sqltypes.Geometry
	}
	panic("unimplemented type " + ct.Type)
}

// IndexSpec describes an index operation in an ALTER statement
type IndexSpec struct {
	// Action states whether it's a CREATE, DROP, or RENAME
	Action string
	// FromName states the old name when renaming
	FromName ColIdent
	// ToName states the name to set when renaming or references the target table
	ToName ColIdent
	// Using states whether you're using BTREE, HASH, or none
	Using ColIdent
	// Type specifies whether this is UNIQUE, FULLTEXT, SPATIAL, or normal (nothing)
	Type string
	// Columns contains the column names when creating an index
	Columns []*IndexColumn
	// Options contains the index options when creating an index
	Options []*IndexOption
}

func (idx *IndexSpec) Format(buf *TrackedBuffer) {
	switch strings.ToLower(idx.Action) {
	case "create":
		buf.Myprintf("add ")
		if idx.Type != "" {
			if idx.Type == PrimaryStr {
				if idx.ToName.val == "" {
					buf.Myprintf("primary key ")
				} else {
					buf.Myprintf("constraint %s primary key ", idx.ToName.val)
				}
			} else {
				buf.Myprintf("%s ", idx.Type)
			}
		}

		if idx.Type != PrimaryStr {
			buf.Myprintf("index %s ", idx.ToName.val)
		}

		if idx.Using.val != "" {
			buf.Myprintf("using %s ", idx.Using.val)
		}

		buf.Myprintf("(")
		for i, col := range idx.Columns {
			if i != 0 {
				buf.Myprintf(", %s", col.Column.val)
			} else {
				buf.Myprintf("%s", col.Column.val)
			}
			if col.Length != nil {
				buf.Myprintf("(%v)", col.Length)
			}
			if col.Order != AscScr {
				buf.Myprintf(" %s", col.Order)
			}
		}
		buf.Myprintf(")")
		for _, opt := range idx.Options {
			buf.Myprintf(" %s", opt.Name)
			if opt.Using != "" {
				buf.Myprintf(" %s", opt.Using)
			} else {
				buf.Myprintf(" %v", opt.Value)
			}
		}
	case "drop":
		if idx.Type == PrimaryStr {
			buf.Myprintf("drop primary key")
		} else {
			buf.Myprintf("drop index %s", idx.ToName.val)
		}
	case "rename":
		buf.Myprintf("rename index %s to %s", idx.FromName.val, idx.ToName.val)
	case "disable":
		buf.Myprintf("disable keys")
	case "enable":
		buf.Myprintf("enable keys")
	}
}

func (idx *IndexSpec) walkSubtree(visit Visit) error {
	if idx == nil {
		return nil
	}
	for _, n := range idx.Columns {
		if err := Walk(visit, n.Column); err != nil {
			return err
		}
	}

	return nil
}

// IndexDefinition describes an index in a CREATE TABLE statement
type IndexDefinition struct {
	Info    *IndexInfo
	Columns []*IndexColumn
	Options []*IndexOption
}

// Format formats the node.
func (idx *IndexDefinition) Format(buf *TrackedBuffer) {
	buf.Myprintf("%v (", idx.Info)
	for i, col := range idx.Columns {
		if i != 0 {
			buf.Myprintf(", %v", col.Column)
		} else {
			buf.Myprintf("%v", col.Column)
		}
		if col.Length != nil {
			buf.Myprintf("(%v)", col.Length)
		}
	}
	buf.Myprintf(")")

	for _, opt := range idx.Options {
		buf.Myprintf(" %s", opt.Name)
		if opt.Using != "" {
			buf.Myprintf(" %s", opt.Using)
		} else {
			buf.Myprintf(" %v", opt.Value)
		}
	}
}

func (idx *IndexDefinition) walkSubtree(visit Visit) error {
	if idx == nil {
		return nil
	}

	for _, n := range idx.Columns {
		if err := Walk(visit, n.Column); err != nil {
			return err
		}
	}

	return nil
}

// IndexInfo describes the name and type of an index in a CREATE TABLE statement
type IndexInfo struct {
	Type     string
	Name     ColIdent
	Primary  bool
	Spatial  bool
	Unique   bool
	Fulltext bool
}

// Format formats the node.
func (ii *IndexInfo) Format(buf *TrackedBuffer) {
	if ii.Primary {
		buf.Myprintf("%s", ii.Type)
	} else {
		buf.Myprintf("%s", ii.Type)
		if !ii.Name.IsEmpty() {
			buf.Myprintf(" %v", ii.Name)
		}
	}
}

func (ii *IndexInfo) walkSubtree(visit Visit) error {
	return Walk(visit, ii.Name)
}

// IndexColumn describes a column in an index definition with optional length and direction
type IndexColumn struct {
	Column ColIdent
	Length *SQLVal
	Order  string
}

// LengthScaleOption is used for types that have an optional length
// and scale
type LengthScaleOption struct {
	Length *SQLVal
	Scale  *SQLVal
}

// IndexOption is used for trailing options for indexes: COMMENT, KEY_BLOCK_SIZE, USING
type IndexOption struct {
	Name  string
	Value *SQLVal
	Using string
}

// ColumnKeyOption indicates whether or not the given column is defined as an
// index element and contains the type of the option
type ColumnKeyOption int

const (
	colKeyNone ColumnKeyOption = iota
	colKeyPrimary
	colKeySpatialKey
	colKeyUnique
	colKeyUniqueKey
	colKey
	colKeyFulltextKey
)

// AutoIncSpec defines an autoincrement value for a ADD AUTO_INCREMENT statement
type AutoIncSpec struct {
	Column   ColIdent
	Sequence TableName
	Value    Expr
}

// Format formats the node.
func (node *AutoIncSpec) Format(buf *TrackedBuffer) {
	buf.Myprintf("%v ", node.Column)
	buf.Myprintf("using %v", node.Sequence)
}

func (node *AutoIncSpec) walkSubtree(visit Visit) error {
	err := Walk(visit, node.Sequence, node.Column)
	return err
}

// DefaultSpec defines a SET / DROP on a column for its default value.
type DefaultSpec struct {
	Action string
	Column ColIdent
	Value  Expr
}

var _ SQLNode = (*DefaultSpec)(nil)

// Format implements SQLNode.
func (node *DefaultSpec) Format(buf *TrackedBuffer) {
	switch node.Action {
	case SetStr:
		buf.Myprintf("alter column %v set default %v", node.Column, node.Value)
	case DropStr:
		buf.Myprintf("alter column %v drop default", node.Column)
	}
}

// walkSubtree implements SQLNode.
func (node *DefaultSpec) walkSubtree(visit Visit) error {
	return Walk(visit, node.Column, node.Value)
}

// ConstraintDefinition describes a constraint in a CREATE TABLE statement
type ConstraintDefinition struct {
	Name    string
	Details ConstraintInfo
}

// ConstraintInfo details a constraint in a CREATE TABLE statement
type ConstraintInfo interface {
	SQLNode
	constraintInfo()
}

// Format formats the node.
func (c *ConstraintDefinition) Format(buf *TrackedBuffer) {
	if c.Name != "" {
		buf.Myprintf("constraint %s ", c.Name)
	}
	c.Details.Format(buf)
}

func (c *ConstraintDefinition) walkSubtree(visit Visit) error {
	return Walk(visit, c.Details)
}

// ReferenceAction indicates the action takes by a referential constraint e.g.
// the `CASCADE` in a `FOREIGN KEY .. ON DELETE CASCADE` table definition.
type ReferenceAction int

// These map to the SQL-defined reference actions.
// See https://dev.mysql.com/doc/refman/8.0/en/create-table-foreign-keys.html#foreign-keys-referential-actions
const (
	// DefaultAction indicates no action was explicitly specified.
	DefaultAction ReferenceAction = iota
	Restrict
	Cascade
	NoAction
	SetNull
	SetDefault
)

// Format formats the node.
func (a ReferenceAction) Format(buf *TrackedBuffer) {
	switch a {
	case Restrict:
		buf.WriteString("restrict")
	case Cascade:
		buf.WriteString("cascade")
	case NoAction:
		buf.WriteString("no action")
	case SetNull:
		buf.WriteString("set null")
	case SetDefault:
		buf.WriteString("set default")
	}
}

// ForeignKeyDefinition describes a foreign key
type ForeignKeyDefinition struct {
	Source            Columns
	ReferencedTable   TableName
	ReferencedColumns Columns
	OnDelete          ReferenceAction
	OnUpdate          ReferenceAction
}

var _ ConstraintInfo = &ForeignKeyDefinition{}

// Format formats the node.
func (f *ForeignKeyDefinition) Format(buf *TrackedBuffer) {
	buf.Myprintf("foreign key %v references %v %v", f.Source, f.ReferencedTable, f.ReferencedColumns)
	if f.OnDelete != DefaultAction {
		buf.Myprintf(" on delete %v", f.OnDelete)
	}
	if f.OnUpdate != DefaultAction {
		buf.Myprintf(" on update %v", f.OnUpdate)
	}
}

func (f *ForeignKeyDefinition) constraintInfo() {}

func (f *ForeignKeyDefinition) walkSubtree(visit Visit) error {
	if err := Walk(visit, f.Source); err != nil {
		return err
	}
	if err := Walk(visit, f.ReferencedTable); err != nil {
		return err
	}
	return Walk(visit, f.ReferencedColumns)
}

type CheckConstraintDefinition struct {
	Expr     Expr
	Enforced bool
}

var _ ConstraintInfo = &CheckConstraintDefinition{}

// Format formats the node.
func (c *CheckConstraintDefinition) Format(buf *TrackedBuffer) {
	buf.Myprintf("check (%v)", c.Expr)
	if !c.Enforced {
		buf.Myprintf(" not enforced")
	}
}

func (f *CheckConstraintDefinition) walkSubtree(visit Visit) error {
	return Walk(visit, f.Expr)
}

func (f *CheckConstraintDefinition) constraintInfo() {}

// Format strings for explain statements
const (
	TraditionalStr = "traditional"
	TreeStr        = "tree"
	JsonStr        = "json"
)

// Explain represents an explain statement
type Explain struct {
	Statement     Statement
	Analyze       bool
	ExplainFormat string
}

// Format formats the node.
func (node *Explain) Format(buf *TrackedBuffer) {
	analyzeOpt := ""
	if node.Analyze {
		analyzeOpt = "analyze "
	}
	formatOpt := ""
	if !node.Analyze && node.ExplainFormat != "" {
		formatOpt = fmt.Sprintf("format = %s ", node.ExplainFormat)
	}
	buf.Myprintf("explain %s%s%v", analyzeOpt, formatOpt, node.Statement)
}

const (
	CreateTriggerStr   = "create trigger"
	CreateProcedureStr = "create procedure"
)

// Show represents a show statement.
type Show struct {
	Type                   string
	Table                  TableName
	Database               string
	IfNotExists            bool
	ShowTablesOpt          *ShowTablesOpt
	Scope                  string
	ShowCollationFilterOpt *Expr
	ShowIndexFilterOpt     Expr
	Filter                 *ShowFilter
	Limit                  *Limit
	CountStar              bool
	Full                   bool
}

// Format formats the node.
func (node *Show) Format(buf *TrackedBuffer) {
	if (node.Type == "tables" || node.Type == "columns" || node.Type == "fields" || node.Type == "triggers") && node.ShowTablesOpt != nil {
		opt := node.ShowTablesOpt
		buf.Myprintf("show ")
		if node.Full {
			buf.Myprintf("full ")
		}
		buf.Myprintf("%s", node.Type)
		if (node.Type == "columns" || node.Type == "fields") && node.HasTable() {
			buf.Myprintf(" from %v", node.Table)
		}
		if opt.DbName != "" {
			buf.Myprintf(" from %s", opt.DbName)
		}
		if opt.AsOf != nil {
			buf.Myprintf(" as of %v", opt.AsOf)
		}
		buf.Myprintf("%v", opt.Filter)
		return
	}
	if node.Type == "index" {
		buf.Myprintf("show index from %v", node.Table)
		if node.Database != "" {
			buf.Myprintf(" from %s", node.Database)
		}
		if node.ShowIndexFilterOpt != nil {
			buf.Myprintf(" where %v", node.ShowIndexFilterOpt)
		}
		return
	}
	if node.Type == CreateTriggerStr {
		buf.Myprintf("show create trigger %v", node.Table)
		return
	}
	if node.Type == CreateProcedureStr {
		buf.Myprintf("show create procedure %v", node.Table)
		return
	}
	if node.Type == "processlist" {
		buf.Myprintf("show ")
		if node.Full {
			buf.Myprintf("full ")
		}
		buf.Myprintf("processlist")
		return
	}
	if node.Type == "procedure status" {
		buf.Myprintf("show procedure status")
		if node.Filter != nil {
			buf.Myprintf("%v", node.Filter)
		}
		return
	}
	if node.Type == "function status" {
		buf.Myprintf("show function status")
		if node.Filter != nil {
			buf.Myprintf("%v", node.Filter)
		}
		return
	}
	if strings.ToLower(node.Type) == "table status" {
		buf.Myprintf("show table status")
		if node.Database != "" {
			buf.Myprintf(" from %s", node.Database)
		}
		if node.Filter != nil {
			buf.Myprintf("%v", node.Filter)
		}
		return
	}
	if strings.ToLower(node.Type) == "create table" && node.HasTable() {
		buf.Myprintf("show %s %v", node.Type, node.Table)

		if node.ShowTablesOpt != nil {
			if node.ShowTablesOpt.AsOf != nil {
				buf.Myprintf(" as of %v", node.ShowTablesOpt.AsOf)
			}
		}
		return
	}

	if node.Database != "" {
		notExistsOpt := ""
		if node.IfNotExists {
			notExistsOpt = "if not exists "
		}
		buf.Myprintf("show %s %s%s", node.Type, notExistsOpt, node.Database)
	} else {
		if node.Scope != "" {
			buf.Myprintf("show %s %s%v", node.Scope, node.Type, node.Filter)
		} else {
			buf.Myprintf("show ")
			if node.CountStar {
				buf.Myprintf("count(*) ")
			}
			buf.Myprintf("%s%v%v", node.Type, node.Filter, node.Limit)
		}
	}

	if node.Type == "collation" && node.ShowCollationFilterOpt != nil {
		buf.Myprintf(" where %v", *node.ShowCollationFilterOpt)
	}
	if node.HasTable() {
		buf.Myprintf(" %v", node.Table)
	}
}

// HasTable returns true if the show statement has a parsed table name.
// Not all show statements parse table names.
func (node *Show) HasTable() bool {
	return node.Table.Name.v != ""
}

func (node *Show) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Table,
		node.ShowIndexFilterOpt,
		node.Filter,
	)
}

// ShowTablesOpt is show tables option
type ShowTablesOpt struct {
	DbName string
	Filter *ShowFilter
	AsOf   Expr
}

// ShowFilter is show tables filter
type ShowFilter struct {
	Like   string
	Filter Expr
}

// Format formats the node.
func (node *ShowFilter) Format(buf *TrackedBuffer) {
	if node == nil {
		return
	}
	if node.Like != "" {
		buf.Myprintf(" like '%s'", node.Like)
	} else {
		buf.Myprintf(" where %v", node.Filter)
	}
}

// Use represents a use statement.
type Use struct {
	DBName TableIdent
}

// Format formats the node.
func (node *Use) Format(buf *TrackedBuffer) {
	if node.DBName.v != "" {
		buf.Myprintf("use %v", node.DBName)
	} else {
		buf.Myprintf("use")
	}
}

func (node *Use) walkSubtree(visit Visit) error {
	return Walk(visit, node.DBName)
}

// Begin represents a Begin statement.
type Begin struct {
	TransactionCharacteristic string
}

// Format formats the node.
func (node *Begin) Format(buf *TrackedBuffer) {
	buf.WriteString("begin")

	if node.TransactionCharacteristic != "" {
		buf.Myprintf(" %s", node.TransactionCharacteristic)
	}
}

// Commit represents a Commit statement.
type Commit struct{}

// Format formats the node.
func (node *Commit) Format(buf *TrackedBuffer) {
	buf.WriteString("commit")
}

// Rollback represents a Rollback statement.
type Rollback struct{}

// Format formats the node.
func (node *Rollback) Format(buf *TrackedBuffer) {
	buf.WriteString("rollback")
}

// FlushOption is used for trailing options for flush statement
type FlushOption struct {
	Name    string
	Channel string
}

// Flush represents a Flush statement.
type Flush struct {
	Type   string
	Option *FlushOption
}

// Format formats the node.
func (node *Flush) Format(buf *TrackedBuffer) {
	buf.WriteString("flush")

	if node.Type != "" {
		buf.Myprintf(" %s", strings.ToLower(node.Type))
	}

	if node.Option.Name == "RELAY LOGS" && node.Option.Channel != "" {
		buf.Myprintf(" %s for channel %s", strings.ToLower(node.Option.Name), strings.ToLower(node.Option.Channel))
	} else {
		buf.Myprintf(" %s", strings.ToLower(node.Option.Name))
	}
}

// OtherRead represents a DESCRIBE, or EXPLAIN statement.
// It should be used only as an indicator. It does not contain
// the full AST for the statement.
type OtherRead struct{}

// Format formats the node.
func (node *OtherRead) Format(buf *TrackedBuffer) {
	buf.WriteString("otherread")
}

// OtherAdmin represents a misc statement that relies on ADMIN privileges,
// such as REPAIR, OPTIMIZE, or TRUNCATE statement.
// It should be used only as an indicator. It does not contain
// the full AST for the statement.
type OtherAdmin struct{}

// Format formats the node.
func (node *OtherAdmin) Format(buf *TrackedBuffer) {
	buf.WriteString("otheradmin")
}

// Comments represents a list of comments.
type Comments [][]byte

// Format formats the node.
func (node Comments) Format(buf *TrackedBuffer) {
	for _, c := range node {
		buf.Myprintf("%s ", c)
	}
}

// SelectExprs represents SELECT expressions.
type SelectExprs []SelectExpr

// Format formats the node.
func (node SelectExprs) Format(buf *TrackedBuffer) {
	var prefix string
	for _, n := range node {
		buf.Myprintf("%s%v", prefix, n)
		prefix = ", "
	}
}

func (node SelectExprs) walkSubtree(visit Visit) error {
	for _, n := range node {
		if err := Walk(visit, n); err != nil {
			return err
		}
	}
	return nil
}

// SelectExpr represents a SELECT expression.
type SelectExpr interface {
	iSelectExpr()
	SQLNode
}

func (*StarExpr) iSelectExpr()    {}
func (*AliasedExpr) iSelectExpr() {}
func (Nextval) iSelectExpr()      {}

// StarExpr defines a '*' or 'table.*' expression.
type StarExpr struct {
	TableName TableName
}

// Format formats the node.
func (node *StarExpr) Format(buf *TrackedBuffer) {
	if !node.TableName.IsEmpty() {
		buf.Myprintf("%v.", node.TableName)
	}
	buf.Myprintf("*")
}

func (node *StarExpr) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.TableName,
	)
}

// AliasedExpr defines an aliased SELECT expression.
type AliasedExpr struct {
	Expr            Expr
	As              ColIdent
	StartParsePos   int
	EndParsePos     int
	InputExpression string
}

// Format formats the node.
func (node *AliasedExpr) Format(buf *TrackedBuffer) {
	if len(node.InputExpression) > 0 {
		if !node.As.IsEmpty() {
			// The AS is omitted here because it gets captured by the InputExpression. A bug, but not a major one since
			// we use the alias expression for the column in the return schema.
			buf.Myprintf("%s %v", node.InputExpression, node.As)
		} else {
			buf.Myprintf("%s", node.InputExpression)
		}
	} else if !node.As.IsEmpty() {
		buf.Myprintf("%v as %v", node.Expr, node.As)
	} else {
		buf.Myprintf("%v", node.Expr)
	}
}

func (node *AliasedExpr) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Expr,
		node.As,
	)
}

// Over defines an OVER expression in a select
type Over WindowDef

// Format formats the node.
func (node *Over) Format(buf *TrackedBuffer) {
	if node == nil {
		return
	}

	if node.isSimpleRef() {
		buf.Myprintf("over %v", node.NameRef)
	} else {
		buf.Myprintf("over (")
		buf.Myprintf("%v", (*WindowDef)(node))
		buf.Myprintf(")")
	}
}

func (node *Over) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(visit, node.PartitionBy, node.OrderBy, node.Name)
}

func (node *Over) isSimpleRef() bool {
	return !node.NameRef.IsEmpty() && len(node.PartitionBy) == 0 && len(node.OrderBy) == 0 && node.Frame == nil
}

// Nextval defines the NEXT VALUE expression.
type Nextval struct {
	Expr Expr
}

// Format formats the node.
func (node Nextval) Format(buf *TrackedBuffer) {
	buf.Myprintf("next %v values", node.Expr)
}

func (node Nextval) walkSubtree(visit Visit) error {
	return Walk(visit, node.Expr)
}

// Columns represents an insert column list.
type Columns []ColIdent

// Format formats the node.
func (node Columns) Format(buf *TrackedBuffer) {
	if node == nil {
		return
	}
	buf.WriteString("(")
	prefix := ""
	for _, n := range node {
		buf.Myprintf("%s%v", prefix, n)
		prefix = ", "
	}
	buf.WriteString(")")
}

func (node Columns) walkSubtree(visit Visit) error {
	for _, n := range node {
		if err := Walk(visit, n); err != nil {
			return err
		}
	}
	return nil
}

// FindColumn finds a column in the column list, returning
// the index if it exists or -1 otherwise
func (node Columns) FindColumn(col ColIdent) int {
	for i, colName := range node {
		if colName.Equal(col) {
			return i
		}
	}
	return -1
}

// Partitions is a type alias for Columns so we can handle printing efficiently
type Partitions Columns

// Format formats the node
func (node Partitions) Format(buf *TrackedBuffer) {
	if node == nil {
		return
	}
	prefix := " partition ("
	for _, n := range node {
		buf.Myprintf("%s%v", prefix, n)
		prefix = ", "
	}
	buf.WriteString(")")
}

func (node Partitions) walkSubtree(visit Visit) error {
	for _, n := range node {
		if err := Walk(visit, n); err != nil {
			return err
		}
	}
	return nil
}

// Variables represents an into variable list.
type Variables []ColIdent

// Format formats the node.
func (node Variables) Format(buf *TrackedBuffer) {
	if node == nil {
		return
	}
	prefix := ""
	for _, n := range node {
		buf.Myprintf("%s%v", prefix, n)
		prefix = ", "
	}
}

func (node Variables) walkSubtree(visit Visit) error {
	for _, n := range node {
		if err := Walk(visit, n); err != nil {
			return err
		}
	}
	return nil
}

// TableExprs represents a list of table expressions.
type TableExprs []TableExpr

// Format formats the node.
func (node TableExprs) Format(buf *TrackedBuffer) {
	var prefix string
	for _, n := range node {
		buf.Myprintf("%s%v", prefix, n)
		prefix = ", "
	}
}

func (node TableExprs) walkSubtree(visit Visit) error {
	for _, n := range node {
		if err := Walk(visit, n); err != nil {
			return err
		}
	}
	return nil
}

// TableExpr represents a table expression.
type TableExpr interface {
	iTableExpr()
	SQLNode
}

func (*AliasedTableExpr) iTableExpr() {}
func (*ParenTableExpr) iTableExpr()   {}
func (*JoinTableExpr) iTableExpr()    {}
func (*JSONTableExpr) iTableExpr()    {}
func (*CommonTableExpr) iTableExpr()  {}
func (*ValuesStatement) iTableExpr()  {}
func (TableFuncExpr) iTableExpr()     {}

// AliasedTableExpr represents a table expression
// coupled with an optional alias, AS OF expression, and index hints.
// If As is empty, no alias was used.
type AliasedTableExpr struct {
	Expr       SimpleTableExpr
	Partitions Partitions
	As         TableIdent
	Hints      *IndexHints
	AsOf       *AsOf
}

type AsOf struct {
	Time Expr
}

func (node *AsOf) Format(buf *TrackedBuffer) {
	buf.Myprintf("as of %v", node.Time)
}

func (node *AsOf) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(visit, node.Time)
}

// Format formats the node.
func (node *AliasedTableExpr) Format(buf *TrackedBuffer) {
	switch node.Expr.(type) {
	case *ValuesStatement:
		buf.Myprintf("(%v)", node.Expr)
	default:
		buf.Myprintf("%v%v", node.Expr, node.Partitions)
	}
	if node.AsOf != nil {
		buf.Myprintf(" %v", node.AsOf)
	}
	if !node.As.IsEmpty() {
		buf.Myprintf(" as %v", node.As)
	}
	switch node := node.Expr.(type) {
	case *ValuesStatement:
		if len(node.Columns) > 0 {
			buf.Myprintf(" %v", node.Columns)
		}
	case *Subquery:
		if len(node.Columns) > 0 {
			buf.Myprintf(" %v", node.Columns)
		}
	}
	if node.Hints != nil {
		// Hint node provides the space padding.
		buf.Myprintf("%v", node.Hints)
	}
}

func (node *AliasedTableExpr) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Expr,
		node.AsOf,
		node.As,
		node.Hints,
	)
}

// RemoveHints returns a new AliasedTableExpr with the hints removed.
func (node *AliasedTableExpr) RemoveHints() *AliasedTableExpr {
	noHints := *node
	noHints.Hints = nil
	return &noHints
}

type With struct {
	Ctes      []TableExpr
	Recursive bool
}

func (w *With) Format(buf *TrackedBuffer) {
	if w == nil {
		return
	}

	buf.Myprintf("with ")
	if w.Recursive {
		buf.Myprintf("recursive ")
	}
	var prefix string
	for _, n := range w.Ctes {
		buf.Myprintf("%s%v", prefix, n)
		prefix = ", "
	}
	buf.Myprintf(" ")
}

func (w *With) walkSubtree(visit Visit) error {
	if w == nil {
		return nil
	}

	for _, n := range w.Ctes {
		if err := Walk(visit, n); err != nil {
			return err
		}
	}
	return nil
}

type Into struct {
	Variables Variables
	Outfile   string
	Dumpfile  string
}

func (i *Into) Format(buf *TrackedBuffer) {
	if i == nil {
		return
	}

	buf.Myprintf(" into ")
	if i.Variables != nil {
		buf.Myprintf("%v", i.Variables)
	}
	if i.Outfile != "" {
		buf.Myprintf("outfile '%s'", i.Outfile)
	}
	if i.Dumpfile != "" {
		buf.Myprintf("dumpfile '%s'", i.Dumpfile)
	}
}

func (i *Into) walkSubtree(visit Visit) error {
	if i == nil {
		return nil
	}
	return Walk(
		visit,
		i.Variables,
	)
}

type CommonTableExpr struct {
	*AliasedTableExpr
	Columns Columns
}

func (e *CommonTableExpr) Format(buf *TrackedBuffer) {
	sq := e.AliasedTableExpr.Expr.(*Subquery)
	as := e.AliasedTableExpr.As

	var cols strings.Builder
	if len(e.Columns) > 0 {
		cols.WriteRune('(')
		for i, col := range e.Columns {
			if i > 0 {
				cols.WriteString(", ")
			}
			cols.WriteString(col.String())
		}
		cols.WriteString(") ")
	}

	buf.Myprintf("%v %sas %v", as, cols.String(), sq)
}

func (e *CommonTableExpr) walkSubtree(visit Visit) error {
	return Walk(
		visit,
		e.AliasedTableExpr,
		e.Columns,
	)
}

// SimpleTableExpr represents a simple table expression.
type SimpleTableExpr interface {
	iSimpleTableExpr()
	SQLNode
}

func (TableName) iSimpleTableExpr()        {}
func (*Subquery) iSimpleTableExpr()        {}
func (*ValuesStatement) iSimpleTableExpr() {}

// TableNames is a list of TableName.
type TableNames []TableName

// Format formats the node.
func (node TableNames) Format(buf *TrackedBuffer) {
	var prefix string
	for _, n := range node {
		buf.Myprintf("%s%v", prefix, n)
		prefix = ", "
	}
}

func (node TableNames) walkSubtree(visit Visit) error {
	for _, n := range node {
		if err := Walk(visit, n); err != nil {
			return err
		}
	}
	return nil
}

// ProcedureName represents a procedure name.
// Qualifier, if specified, represents a database name.
// ProcedureName is a value struct whose fields are case-sensitive,
// so TableIdent struct is used for fields
type ProcedureName struct {
	Name      ColIdent
	Qualifier TableIdent
}

// Format formats the node.
func (node ProcedureName) Format(buf *TrackedBuffer) {
	if node.IsEmpty() {
		return
	}
	if !node.Qualifier.IsEmpty() {
		buf.Myprintf("%v.", node.Qualifier)
	}
	buf.Myprintf("%v", node.Name)
}

// Format formats the node.
func (node ProcedureName) String() string {
	if node.IsEmpty() {
		return ""
	}
	if !node.Qualifier.IsEmpty() {
		return fmt.Sprintf("%s.%s", node.Qualifier.String(), node.Name)
	}
	return node.Name.String()
}

func (node ProcedureName) walkSubtree(visit Visit) error {
	return Walk(
		visit,
		node.Name,
		node.Qualifier,
	)
}

// IsEmpty returns true if TableName is nil or empty.
func (node ProcedureName) IsEmpty() bool {
	// If Name is empty, Qualifier is also empty.
	return node.Name.IsEmpty()
}

// TableName represents a table  name.
// Qualifier, if specified, represents a database or keyspace.
// TableName is a value struct whose fields are case sensitive.
// This means two TableName vars can be compared for equality
// and a TableName can also be used as key in a map.
type TableName struct {
	Name, Qualifier TableIdent
}

// Format formats the node.
func (node TableName) Format(buf *TrackedBuffer) {
	if node.IsEmpty() {
		return
	}
	if !node.Qualifier.IsEmpty() {
		buf.Myprintf("%v.", node.Qualifier)
	}
	buf.Myprintf("%v", node.Name)
}

// Format formats the node.
func (node TableName) String() string {
	if node.IsEmpty() {
		return ""
	}
	if !node.Qualifier.IsEmpty() {
		return fmt.Sprintf("%s.%s", node.Qualifier.String(), node.Name)
	}
	return node.Name.String()
}

func (node TableName) walkSubtree(visit Visit) error {
	return Walk(
		visit,
		node.Name,
		node.Qualifier,
	)
}

// IsEmpty returns true if TableName is nil or empty.
func (node TableName) IsEmpty() bool {
	// If Name is empty, Qualifier is also empty.
	return node.Name.IsEmpty()
}

// ToViewName returns a TableName acceptable for use as a VIEW. VIEW names are
// always lowercase, so ToViewName lowercasese the name. Databases are case-sensitive
// so Qualifier is left untouched.
func (node TableName) ToViewName() TableName {
	return TableName{
		Qualifier: node.Qualifier,
		Name:      NewTableIdent(strings.ToLower(node.Name.v)),
	}
}

// TriggerName represents a trigger name.
// Qualifier, if specified, represents a database name.
// TriggerName is a value struct whose fields are case-sensitive,
// so TableIdent struct is used for fields
type TriggerName struct {
	Name      ColIdent
	Qualifier TableIdent
}

// Format formats the node.
func (node TriggerName) Format(buf *TrackedBuffer) {
	if node.IsEmpty() {
		return
	}
	if !node.Qualifier.IsEmpty() {
		buf.Myprintf("%v.", node.Qualifier)
	}
	buf.Myprintf("%v", node.Name)
}

// Format formats the node.
func (node TriggerName) String() string {
	if node.IsEmpty() {
		return ""
	}
	if !node.Qualifier.IsEmpty() {
		return fmt.Sprintf("%s.%s", node.Qualifier.String(), node.Name)
	}
	return node.Name.String()
}

func (node TriggerName) walkSubtree(visit Visit) error {
	return Walk(
		visit,
		node.Name,
		node.Qualifier,
	)
}

// IsEmpty returns true if TableName is nil or empty.
func (node TriggerName) IsEmpty() bool {
	// If Name is empty, Qualifier is also empty.
	return node.Name.IsEmpty()
}

// ParenTableExpr represents a parenthesized list of TableExpr.
type ParenTableExpr struct {
	Exprs TableExprs
}

// Format formats the node.
func (node *ParenTableExpr) Format(buf *TrackedBuffer) {
	buf.Myprintf("(%v)", node.Exprs)
}

func (node *ParenTableExpr) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Exprs,
	)
}

// JoinCondition represents the join conditions (either a ON or USING clause)
// of a JoinTableExpr.
type JoinCondition struct {
	On    Expr
	Using Columns
}

// Format formats the node.
func (node JoinCondition) Format(buf *TrackedBuffer) {
	if node.On != nil {
		buf.Myprintf(" on %v", node.On)
	}
	if node.Using != nil {
		buf.Myprintf(" using %v", node.Using)
	}
}

func (node JoinCondition) walkSubtree(visit Visit) error {
	return Walk(
		visit,
		node.On,
		node.Using,
	)
}

// JoinTableExpr represents a TableExpr that's a JOIN operation.
type JoinTableExpr struct {
	LeftExpr  TableExpr
	Join      string
	RightExpr TableExpr
	Condition JoinCondition
}

// JoinTableExpr.Join
const (
	JoinStr             = "join"
	StraightJoinStr     = "straight_join"
	LeftJoinStr         = "left join"
	RightJoinStr        = "right join"
	NaturalJoinStr      = "natural join"
	NaturalLeftJoinStr  = "natural left join"
	NaturalRightJoinStr = "natural right join"
	FullOuterJoinStr    = "full outer join"
)

// Format formats the node.
func (node *JoinTableExpr) Format(buf *TrackedBuffer) {
	buf.Myprintf("%v %s %v%v", node.LeftExpr, node.Join, node.RightExpr, node.Condition)
}

func (node *JoinTableExpr) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.LeftExpr,
		node.RightExpr,
		node.Condition,
	)
}

// JSONTableExpr represents a TableExpr that's a json_table operation.
type JSONTableExpr struct {
	Data  Expr
	Path  string
	Spec  *TableSpec
	Alias TableIdent
}

// Format formats the node.
func (node *JSONTableExpr) Format(buf *TrackedBuffer) {
	buf.Myprintf(`JSON_TABLE(%v, "%s" COLUMNS%v) as %v`, node.Data, node.Path, node.Spec, node.Alias)

}

func (node *JSONTableExpr) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(visit)
}

// IndexHints represents a list of index hints.
type IndexHints struct {
	Type    string
	Indexes []ColIdent
}

// Index hints.
const (
	UseStr    = "use "
	IgnoreStr = "ignore "
	ForceStr  = "force "
)

// Format formats the node.
func (node *IndexHints) Format(buf *TrackedBuffer) {
	buf.Myprintf(" %sindex ", node.Type)
	prefix := "("
	for _, n := range node.Indexes {
		buf.Myprintf("%s%v", prefix, n)
		prefix = ", "
	}
	buf.Myprintf(")")
}

func (node *IndexHints) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	for _, n := range node.Indexes {
		if err := Walk(visit, n); err != nil {
			return err
		}
	}
	return nil
}

// Where represents a WHERE or HAVING clause.
type Where struct {
	Type string
	Expr Expr
}

// Where.Type
const (
	WhereStr  = "where"
	HavingStr = "having"
)

// NewWhere creates a WHERE or HAVING clause out
// of a Expr. If the expression is nil, it returns nil.
func NewWhere(typ string, expr Expr) *Where {
	if expr == nil {
		return nil
	}
	return &Where{Type: typ, Expr: expr}
}

// Format formats the node.
func (node *Where) Format(buf *TrackedBuffer) {
	if node == nil || node.Expr == nil {
		return
	}
	buf.Myprintf(" %s %v", node.Type, node.Expr)
}

func (node *Where) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Expr,
	)
}

// Expr represents an expression.
type Expr interface {
	iExpr()
	// replace replaces any subexpression that matches
	// from with to. The implementation can use the
	// replaceExprs convenience function.
	replace(from, to Expr) bool
	SQLNode
}

func (*AndExpr) iExpr()           {}
func (*OrExpr) iExpr()            {}
func (*XorExpr) iExpr()           {}
func (*NotExpr) iExpr()           {}
func (*ParenExpr) iExpr()         {}
func (*ComparisonExpr) iExpr()    {}
func (*RangeCond) iExpr()         {}
func (*IsExpr) iExpr()            {}
func (*ExistsExpr) iExpr()        {}
func (*SQLVal) iExpr()            {}
func (*NullVal) iExpr()           {}
func (BoolVal) iExpr()            {}
func (*ColName) iExpr()           {}
func (ValTuple) iExpr()           {}
func (*Subquery) iExpr()          {}
func (ListArg) iExpr()            {}
func (*BinaryExpr) iExpr()        {}
func (*UnaryExpr) iExpr()         {}
func (*IntervalExpr) iExpr()      {}
func (*CollateExpr) iExpr()       {}
func (*FuncExpr) iExpr()          {}
func (*TimestampFuncExpr) iExpr() {}
func (*CurTimeFuncExpr) iExpr()   {}
func (*CaseExpr) iExpr()          {}
func (*ValuesFuncExpr) iExpr()    {}
func (*ConvertExpr) iExpr()       {}
func (*SubstrExpr) iExpr()        {}
func (*TrimExpr) iExpr()          {}
func (*ConvertUsingExpr) iExpr()  {}
func (*MatchExpr) iExpr()         {}
func (*GroupConcatExpr) iExpr()   {}
func (*Default) iExpr()           {}

// ReplaceExpr finds the from expression from root
// and replaces it with to. If from matches root,
// then to is returned.
func ReplaceExpr(root, from, to Expr) Expr {
	if root == from {
		return to
	}
	root.replace(from, to)
	return root
}

// replaceExprs is a convenience function used by implementors
// of the replace method.
func replaceExprs(from, to Expr, exprs ...*Expr) bool {
	for _, expr := range exprs {
		if *expr == nil {
			continue
		}
		if *expr == from {
			*expr = to
			return true
		}
		if (*expr).replace(from, to) {
			return true
		}
	}
	return false
}

// Exprs represents a list of value expressions.
// It's not a valid expression because it's not parenthesized.
type Exprs []Expr

// Format formats the node.
func (node Exprs) Format(buf *TrackedBuffer) {
	var prefix string
	for _, n := range node {
		buf.Myprintf("%s%v", prefix, n)
		prefix = ", "
	}
}

func (node Exprs) walkSubtree(visit Visit) error {
	for _, n := range node {
		if err := Walk(visit, n); err != nil {
			return err
		}
	}
	return nil
}

// AndExpr represents an AND expression.
type AndExpr struct {
	Left, Right Expr
}

// Format formats the node.
func (node *AndExpr) Format(buf *TrackedBuffer) {
	buf.Myprintf("%v and %v", node.Left, node.Right)
}

func (node *AndExpr) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Left,
		node.Right,
	)
}

func (node *AndExpr) replace(from, to Expr) bool {
	return replaceExprs(from, to, &node.Left, &node.Right)
}

// OrExpr represents an OR expression.
type OrExpr struct {
	Left, Right Expr
}

// Format formats the node.
func (node *OrExpr) Format(buf *TrackedBuffer) {
	buf.Myprintf("%v or %v", node.Left, node.Right)
}

func (node *OrExpr) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Left,
		node.Right,
	)
}

func (node *OrExpr) replace(from, to Expr) bool {
	return replaceExprs(from, to, &node.Left, &node.Right)
}

// XorExpr represents an XOR expression.
type XorExpr struct {
	Left, Right Expr
}

// Format formats the node.
func (node *XorExpr) Format(buf *TrackedBuffer) {
	buf.Myprintf("%v xor %v", node.Left, node.Right)
}

func (node *XorExpr) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Left,
		node.Right,
	)
}

func (node *XorExpr) replace(from, to Expr) bool {
	return replaceExprs(from, to, &node.Left, &node.Right)
}

// NotExpr represents a NOT expression.
type NotExpr struct {
	Expr Expr
}

// Format formats the node.
func (node *NotExpr) Format(buf *TrackedBuffer) {
	buf.Myprintf("not %v", node.Expr)
}

func (node *NotExpr) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Expr,
	)
}

func (node *NotExpr) replace(from, to Expr) bool {
	return replaceExprs(from, to, &node.Expr)
}

// ParenExpr represents a parenthesized boolean expression.
type ParenExpr struct {
	Expr Expr
}

// Format formats the node.
func (node *ParenExpr) Format(buf *TrackedBuffer) {
	buf.Myprintf("(%v)", node.Expr)
}

func (node *ParenExpr) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Expr,
	)
}

func (node *ParenExpr) replace(from, to Expr) bool {
	return replaceExprs(from, to, &node.Expr)
}

// ComparisonExpr represents a two-value comparison expression.
type ComparisonExpr struct {
	Operator    string
	Left, Right Expr
	Escape      Expr
}

// ComparisonExpr.Operator
const (
	EqualStr             = "="
	LessThanStr          = "<"
	GreaterThanStr       = ">"
	LessEqualStr         = "<="
	GreaterEqualStr      = ">="
	NotEqualStr          = "!="
	NullSafeEqualStr     = "<=>"
	InStr                = "in"
	NotInStr             = "not in"
	LikeStr              = "like"
	NotLikeStr           = "not like"
	RegexpStr            = "regexp"
	NotRegexpStr         = "not regexp"
	JSONExtractOp        = "->"
	JSONUnquoteExtractOp = "->>"
)

// Format formats the node.
func (node *ComparisonExpr) Format(buf *TrackedBuffer) {
	buf.Myprintf("%v %s %v", node.Left, node.Operator, node.Right)
	if node.Escape != nil {
		buf.Myprintf(" escape %v", node.Escape)
	}
}

func (node *ComparisonExpr) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Left,
		node.Right,
		node.Escape,
	)
}

func (node *ComparisonExpr) replace(from, to Expr) bool {
	return replaceExprs(from, to, &node.Left, &node.Right, &node.Escape)
}

// IsImpossible returns true if the comparison in the expression can never evaluate to true.
// Note that this is not currently exhaustive to ALL impossible comparisons.
func (node *ComparisonExpr) IsImpossible() bool {
	var left, right *SQLVal
	var ok bool
	if left, ok = node.Left.(*SQLVal); !ok {
		return false
	}
	if right, ok = node.Right.(*SQLVal); !ok {
		return false
	}
	if node.Operator == NotEqualStr && left.Type == right.Type {
		if len(left.Val) != len(right.Val) {
			return false
		}

		for i := range left.Val {
			if left.Val[i] != right.Val[i] {
				return false
			}
		}
		return true
	}
	return false
}

// RangeCond represents a BETWEEN or a NOT BETWEEN expression.
type RangeCond struct {
	Operator string
	Left     Expr
	From, To Expr
}

// RangeCond.Operator
const (
	BetweenStr    = "between"
	NotBetweenStr = "not between"
)

// Format formats the node.
func (node *RangeCond) Format(buf *TrackedBuffer) {
	buf.Myprintf("%v %s %v and %v", node.Left, node.Operator, node.From, node.To)
}

func (node *RangeCond) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Left,
		node.From,
		node.To,
	)
}

func (node *RangeCond) replace(from, to Expr) bool {
	return replaceExprs(from, to, &node.Left, &node.From, &node.To)
}

// IsExpr represents an IS ... or an IS NOT ... expression.
type IsExpr struct {
	Operator string
	Expr     Expr
}

// IsExpr.Operator
const (
	IsNullStr     = "is null"
	IsNotNullStr  = "is not null"
	IsTrueStr     = "is true"
	IsNotTrueStr  = "is not true"
	IsFalseStr    = "is false"
	IsNotFalseStr = "is not false"
)

// Format formats the node.
func (node *IsExpr) Format(buf *TrackedBuffer) {
	buf.Myprintf("%v %s", node.Expr, node.Operator)
}

func (node *IsExpr) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Expr,
	)
}

func (node *IsExpr) replace(from, to Expr) bool {
	return replaceExprs(from, to, &node.Expr)
}

// ExistsExpr represents an EXISTS expression.
type ExistsExpr struct {
	Subquery *Subquery
}

// Format formats the node.
func (node *ExistsExpr) Format(buf *TrackedBuffer) {
	buf.Myprintf("exists %v", node.Subquery)
}

func (node *ExistsExpr) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Subquery,
	)
}

func (node *ExistsExpr) replace(from, to Expr) bool {
	return false
}

// ExprFromValue converts the given Value into an Expr or returns an error.
func ExprFromValue(value sqltypes.Value) (Expr, error) {
	// The type checks here follow the rules defined in sqltypes/types.go.
	switch {
	case value.Type() == sqltypes.Null:
		return &NullVal{}, nil
	case value.IsIntegral():
		return NewIntVal(value.ToBytes()), nil
	case value.IsFloat() || value.Type() == sqltypes.Decimal:
		return NewFloatVal(value.ToBytes()), nil
	case value.IsQuoted():
		return NewStrVal(value.ToBytes()), nil
	default:
		// We cannot support sqltypes.Expression, or any other invalid type.
		return nil, fmt.Errorf("cannot convert value %v to AST", value)
	}
}

// ValType specifies the type for SQLVal.
type ValType int

// These are the possible Valtype values.
// HexNum represents a 0x... value. It cannot
// be treated as a simple value because it can
// be interpreted differently depending on the
// context.
const (
	StrVal = ValType(iota)
	IntVal
	FloatVal
	HexNum
	HexVal
	ValArg
	BitVal
)

// SQLVal represents a single value.
type SQLVal struct {
	Type ValType
	Val  []byte
}

// NewStrVal builds a new StrVal.
func NewStrVal(in []byte) *SQLVal {
	return &SQLVal{Type: StrVal, Val: in}
}

// NewIntVal builds a new IntVal.
func NewIntVal(in []byte) *SQLVal {
	return &SQLVal{Type: IntVal, Val: in}
}

// NewFloatVal builds a new FloatVal.
func NewFloatVal(in []byte) *SQLVal {
	return &SQLVal{Type: FloatVal, Val: in}
}

// NewHexNum builds a new HexNum.
func NewHexNum(in []byte) *SQLVal {
	return &SQLVal{Type: HexNum, Val: in}
}

// NewHexVal builds a new HexVal.
func NewHexVal(in []byte) *SQLVal {
	return &SQLVal{Type: HexVal, Val: in}
}

// NewBitVal builds a new BitVal containing a bit literal.
func NewBitVal(in []byte) *SQLVal {
	return &SQLVal{Type: BitVal, Val: in}
}

// NewValArg builds a new ValArg.
func NewValArg(in []byte) *SQLVal {
	return &SQLVal{Type: ValArg, Val: in}
}

// Format formats the node.
func (node *SQLVal) Format(buf *TrackedBuffer) {
	switch node.Type {
	case StrVal:
		sqltypes.MakeTrusted(sqltypes.VarBinary, node.Val).EncodeSQL(buf)
	case IntVal, FloatVal, HexNum:
		buf.Myprintf("%s", []byte(node.Val))
	case HexVal:
		buf.Myprintf("X'%s'", []byte(node.Val))
	case BitVal:
		buf.Myprintf("B'%s'", []byte(node.Val))
	case ValArg:
		buf.WriteArg(string(node.Val))
	default:
		panic("unexpected")
	}
}

// String returns the node as a string, similar to Format.
func (node *SQLVal) String() string {
	buf := NewTrackedBuffer(nil)
	node.Format(buf)
	return buf.String()
}

func (node *SQLVal) replace(from, to Expr) bool {
	return false
}

// HexDecode decodes the hexval into bytes.
func (node *SQLVal) HexDecode() ([]byte, error) {
	dst := make([]byte, hex.DecodedLen(len([]byte(node.Val))))
	_, err := hex.Decode(dst, []byte(node.Val))
	if err != nil {
		return nil, err
	}
	return dst, err
}

// NullVal represents a NULL value.
type NullVal struct{}

// Format formats the node.
func (node *NullVal) Format(buf *TrackedBuffer) {
	buf.Myprintf("null")
}

func (node *NullVal) replace(from, to Expr) bool {
	return false
}

// BoolVal is true or false.
type BoolVal bool

// Format formats the node.
func (node BoolVal) Format(buf *TrackedBuffer) {
	if node {
		buf.Myprintf("true")
	} else {
		buf.Myprintf("false")
	}
}

func (node BoolVal) replace(from, to Expr) bool {
	return false
}

// ColName represents a column name.
type ColName struct {
	// Metadata is not populated by the parser.
	// It's a placeholder for analyzers to store
	// additional data, typically info about which
	// table or column this node references.
	Metadata  interface{}
	Name      ColIdent
	Qualifier TableName
}

// NewColName returns a simple ColName with no table qualifier
func NewColName(name string) *ColName {
	return &ColName{
		Name:      NewColIdent(name),
		Qualifier: TableName{},
	}
}

// Format formats the node.
func (node *ColName) Format(buf *TrackedBuffer) {
	if !node.Qualifier.IsEmpty() {
		buf.Myprintf("%v.", node.Qualifier)
	}
	buf.Myprintf("%v", node.Name)
}

func (node *ColName) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Name,
		node.Qualifier,
	)
}

func (node *ColName) replace(from, to Expr) bool {
	return false
}

// Equal returns true if the column names match.
func (node *ColName) Equal(c *ColName) bool {
	// Failsafe: ColName should not be empty.
	if node == nil || c == nil {
		return false
	}
	return node.Name.Equal(c.Name) && node.Qualifier == c.Qualifier
}

// Equal returns true if the column name matches the string given. Only true for column names with no qualifier.
func (node *ColName) EqualString(s string) bool {
	return node.Qualifier.IsEmpty() && node.Name.EqualString(s)
}

func (node *ColName) String() string {
	if !node.Qualifier.IsEmpty() {
		return fmt.Sprintf("%s.%s", node.Qualifier.String(), node.Name.String())
	}
	return node.Name.String()
}

// ColTuple represents a list of column values.
// It can be ValTuple, Subquery, ListArg.
type ColTuple interface {
	iColTuple()
	Expr
}

func (ValTuple) iColTuple()  {}
func (*Subquery) iColTuple() {}
func (ListArg) iColTuple()   {}

// ValTuple represents a tuple of actual values.
type ValTuple Exprs

// Format formats the node.
func (node ValTuple) Format(buf *TrackedBuffer) {
	buf.Myprintf("(%v)", Exprs(node))
}

func (node ValTuple) walkSubtree(visit Visit) error {
	return Walk(visit, Exprs(node))
}

func (node ValTuple) replace(from, to Expr) bool {
	for i := range node {
		if replaceExprs(from, to, &node[i]) {
			return true
		}
	}
	return false
}

// Subquery represents a subquery.
type Subquery struct {
	Select  SelectStatement
	Columns Columns
}

// Format formats the node.
func (node *Subquery) Format(buf *TrackedBuffer) {
	buf.Myprintf("(%v)", node.Select)
}

func (node *Subquery) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Select,
	)
}

func (node *Subquery) replace(from, to Expr) bool {
	return false
}

// ListArg represents a named list argument.
type ListArg []byte

// Format formats the node.
func (node ListArg) Format(buf *TrackedBuffer) {
	buf.WriteArg(string(node))
}

func (node ListArg) replace(from, to Expr) bool {
	return false
}

// BinaryExpr represents a binary value expression.
type BinaryExpr struct {
	Operator    string
	Left, Right Expr
}

// BinaryExpr.Operator
const (
	BitAndStr     = "&"
	BitOrStr      = "|"
	BitXorStr     = "^"
	PlusStr       = "+"
	MinusStr      = "-"
	MultStr       = "*"
	DivStr        = "/"
	IntDivStr     = "div"
	ModStr        = "%"
	ShiftLeftStr  = "<<"
	ShiftRightStr = ">>"
)

// Format formats the node.
func (node *BinaryExpr) Format(buf *TrackedBuffer) {
	buf.Myprintf("%v %s %v", node.Left, node.Operator, node.Right)
}

func (node *BinaryExpr) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Left,
		node.Right,
	)
}

func (node *BinaryExpr) replace(from, to Expr) bool {
	return replaceExprs(from, to, &node.Left, &node.Right)
}

// UnaryExpr represents a unary value expression.
type UnaryExpr struct {
	Operator string
	Expr     Expr
}

// UnaryExpr.Operator
const (
	UPlusStr    = "+"
	UMinusStr   = "-"
	TildaStr    = "~"
	BangStr     = "!"
	BinaryStr   = "binary "
	Armscii8Str = "_armscii8 "
	AsciiStr    = "_ascii "
	Big5Str     = "_big5 "
	UBinaryStr  = "_binary "
	Cp1250Str   = "_cp1250 "
	Cp1251Str   = "_cp1251 "
	Cp1256Str   = "_cp1256 "
	Cp1257Str   = "_cp1257 "
	Cp850Str    = "_cp850 "
	Cp852Str    = "_cp852 "
	Cp866Str    = "_cp866 "
	Cp932Str    = "_cp932 "
	Dec8Str     = "_dec8 "
	EucjpmsStr  = "_eucjpms "
	EuckrStr    = "_euckr "
	Gb18030Str  = "_gb18030 "
	Gb2312Str   = "_gb2312 "
	GbkStr      = "_gbk "
	Geostd8Str  = "_geostd8 "
	GreekStr    = "_greek "
	HebrewStr   = "_hebrew "
	Hp8Str      = "_hp8 "
	Keybcs2Str  = "_keybcs2 "
	Koi8rStr    = "_koi8r "
	Koi8uStr    = "_koi8u "
	Latin1Str   = "_latin1 "
	Latin2Str   = "_latin2 "
	Latin5Str   = "_latin5 "
	Latin7Str   = "_latin7 "
	MacceStr    = "_macce "
	MacromanStr = "_macroman "
	SjisStr     = "_sjis "
	Swe7Str     = "_swe7 "
	Tis620Str   = "_tis620 "
	Ucs2Str     = "_ucs2 "
	UjisStr     = "_ujis "
	Utf16Str    = "_utf16 "
	Utf16leStr  = "_utf16le "
	Utf32Str    = "_utf32 "
	Utf8mb3Str  = "_utf8mb3 "
	Utf8mb4Str  = "_utf8mb4 "
)

// Format formats the node.
func (node *UnaryExpr) Format(buf *TrackedBuffer) {
	if _, unary := node.Expr.(*UnaryExpr); unary {
		buf.Myprintf("%s %v", node.Operator, node.Expr)
		return
	}
	buf.Myprintf("%s%v", node.Operator, node.Expr)
}

func (node *UnaryExpr) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Expr,
	)
}

func (node *UnaryExpr) replace(from, to Expr) bool {
	return replaceExprs(from, to, &node.Expr)
}

// IntervalExpr represents a date-time INTERVAL expression.
type IntervalExpr struct {
	Expr Expr
	Unit string
}

// Format formats the node.
func (node *IntervalExpr) Format(buf *TrackedBuffer) {
	buf.Myprintf("interval %v %s", node.Expr, node.Unit)
}

func (node *IntervalExpr) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Expr,
	)
}

func (node *IntervalExpr) replace(from, to Expr) bool {
	return replaceExprs(from, to, &node.Expr)
}

// TimestampFuncExpr represents the function and arguments for TIMESTAMP{ADD,DIFF} functions.
type TimestampFuncExpr struct {
	Name  string
	Expr1 Expr
	Expr2 Expr
	Unit  string
}

// Format formats the node.
func (node *TimestampFuncExpr) Format(buf *TrackedBuffer) {
	buf.Myprintf("%s(%s, %v, %v)", node.Name, node.Unit, node.Expr1, node.Expr2)
}

func (node *TimestampFuncExpr) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Expr1,
		node.Expr2,
	)
}

func (node *TimestampFuncExpr) replace(from, to Expr) bool {
	if replaceExprs(from, to, &node.Expr1) {
		return true
	}
	if replaceExprs(from, to, &node.Expr2) {
		return true
	}
	return false
}

// CurTimeFuncExpr represents the function and arguments for CURRENT DATE/TIME functions
// supported functions are documented in the grammar
type CurTimeFuncExpr struct {
	Name ColIdent
	Fsp  Expr // fractional seconds precision, integer from 0 to 6
}

// Format formats the node.
func (node *CurTimeFuncExpr) Format(buf *TrackedBuffer) {
	buf.Myprintf("%s(%v)", node.Name.String(), node.Fsp)
}

func (node *CurTimeFuncExpr) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Fsp,
	)
}

func (node *CurTimeFuncExpr) replace(from, to Expr) bool {
	return replaceExprs(from, to, &node.Fsp)
}

// CollateExpr represents dynamic collate operator.
type CollateExpr struct {
	Expr    Expr
	Charset string
}

// Format formats the node.
func (node *CollateExpr) Format(buf *TrackedBuffer) {
	buf.Myprintf("%v collate %s", node.Expr, node.Charset)
}

func (node *CollateExpr) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Expr,
	)
}

func (node *CollateExpr) replace(from, to Expr) bool {
	return replaceExprs(from, to, &node.Expr)
}

// FuncExpr represents a function call.
type FuncExpr struct {
	Qualifier TableIdent
	Name      ColIdent
	Distinct  bool
	Exprs     SelectExprs
	Over      *Over
}

// Format formats the node.
func (node *FuncExpr) Format(buf *TrackedBuffer) {
	var distinct string
	if node.Distinct {
		distinct = "distinct "
	}
	if !node.Qualifier.IsEmpty() {
		buf.Myprintf("%v.", node.Qualifier)
	}
	// Function names should not be back-quoted even
	// if they match a reserved word. So, print the
	// name as is.
	buf.Myprintf("%s(%s%v)", node.Name.String(), distinct, node.Exprs)

	if node.Over != nil {
		buf.Myprintf(" %v", node.Over)
	}
}

func (node *FuncExpr) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Qualifier,
		node.Name,
		node.Exprs,
		node.Over,
	)
}

func (node *FuncExpr) replace(from, to Expr) bool {
	for _, sel := range node.Exprs {
		aliased, ok := sel.(*AliasedExpr)
		if !ok {
			continue
		}
		if replaceExprs(from, to, &aliased.Expr) {
			return true
		}
	}
	return false
}

// Aggregates is a map of all aggregate functions.
var Aggregates = map[string]bool{
	"avg":            true,
	"bit_and":        true,
	"bit_or":         true,
	"bit_xor":        true,
	"count":          true,
	"group_concat":   true,
	"json_arrayagg":  true,
	"json_objectagg": true,
	"max":            true,
	"min":            true,
	"std":            true,
	"stddev_pop":     true,
	"stddev_samp":    true,
	"stddev":         true,
	"sum":            true,
	"var_pop":        true,
	"var_samp":       true,
	"variance":       true,
}

// IsAggregate returns true if the function is an aggregate.
func (node *FuncExpr) IsAggregate() bool {
	return Aggregates[node.Name.Lowered()]
}

// GroupConcatExpr represents a call to GROUP_CONCAT
type GroupConcatExpr struct {
	Distinct  string
	Exprs     SelectExprs
	OrderBy   OrderBy
	Separator string
}

// Format formats the node
func (node *GroupConcatExpr) Format(buf *TrackedBuffer) {
	sep := node.Separator
	if sep != "" {
		sep = " separator " + "'" + sep + "'"
	}

	buf.Myprintf("group_concat(%s%v%v%s)", node.Distinct, node.Exprs, node.OrderBy, sep)
}

func (node *GroupConcatExpr) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Exprs,
		node.OrderBy,
	)
}

func (node *GroupConcatExpr) replace(from, to Expr) bool {
	for _, sel := range node.Exprs {
		aliased, ok := sel.(*AliasedExpr)
		if !ok {
			continue
		}
		if replaceExprs(from, to, &aliased.Expr) {
			return true
		}
	}
	for _, order := range node.OrderBy {
		if replaceExprs(from, to, &order.Expr) {
			return true
		}
	}
	return false
}

// ValuesFuncExpr represents a function call.
type ValuesFuncExpr struct {
	Name *ColName
}

// Format formats the node.
func (node *ValuesFuncExpr) Format(buf *TrackedBuffer) {
	buf.Myprintf("values(%v)", node.Name)
}

func (node *ValuesFuncExpr) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Name,
	)
}

func (node *ValuesFuncExpr) replace(from, to Expr) bool {
	return false
}

// SubstrExpr represents a call to SubstrExpr(column, value_expression) or SubstrExpr(column, value_expression,value_expression)
// also supported syntax SubstrExpr(column from value_expression for value_expression).
// Additionally to column names, SubstrExpr is also supported for string values, e.g.:
// SubstrExpr('static string value', value_expression, value_expression)
// In this case StrVal will be set instead of Name.
type SubstrExpr struct {
	Name   *ColName
	StrVal *SQLVal
	From   Expr
	To     Expr
}

// Format formats the node.
func (node *SubstrExpr) Format(buf *TrackedBuffer) {
	var val interface{}
	if node.Name != nil {
		val = node.Name
	} else {
		val = node.StrVal
	}

	if node.To == nil {
		buf.Myprintf("substr(%v, %v)", val, node.From)
	} else {
		buf.Myprintf("substr(%v, %v, %v)", val, node.From, node.To)
	}
}

func (node *SubstrExpr) replace(from, to Expr) bool {
	return replaceExprs(from, to, &node.From, &node.To)
}

func (node *SubstrExpr) walkSubtree(visit Visit) error {
	if node == nil || node.Name == nil {
		return nil
	}
	return Walk(
		visit,
		node.Name,
		node.From,
		node.To,
	)
}

// Options for Trim
const (
	Leading  string = "l"
	Trailing string = "r"
	Both     string = "b"
)

type TrimExpr struct {
	Str     Expr
	Pattern Expr
	Dir     string
}

// Format formats the node
func (node *TrimExpr) Format(buf *TrackedBuffer) {
	if node.Dir == Leading {
		buf.Myprintf("trim(leading %v from %v)", node.Pattern, node.Str)
	} else if node.Dir == Trailing {
		buf.Myprintf("trim(trailing %v from %v)", node.Pattern, node.Str)
	} else {
		buf.Myprintf("trim(both %v from %v)", node.Pattern, node.Str)
	}
}

func (node *TrimExpr) replace(from, to Expr) bool {
	if replaceExprs(from, to, &node.Pattern) {
		return true
	}
	if replaceExprs(from, to, &node.Str) {
		return true
	}

	return false
}

func (node *TrimExpr) walkSubtree(visit Visit) error {
	if node == nil || node.Str == nil {
		return nil
	}
	return Walk(visit, node.Str, node.Pattern)
}

// ConvertExpr represents a call to CONVERT(expr, type)
// or its equivalent CAST(expr AS type). Both are rewritten to the former.
type ConvertExpr struct {
	Name string
	Expr Expr
	Type *ConvertType
}

// Format formats the node.
func (node *ConvertExpr) Format(buf *TrackedBuffer) {
	buf.Myprintf("%s(%v, %v)", node.Name, node.Expr, node.Type)
}

func (node *ConvertExpr) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Expr,
		node.Type,
	)
}

func (node *ConvertExpr) replace(from, to Expr) bool {
	return replaceExprs(from, to, &node.Expr)
}

// ConvertUsingExpr represents a call to CONVERT(expr USING charset).
type ConvertUsingExpr struct {
	Expr Expr
	Type string
}

// Format formats the node.
func (node *ConvertUsingExpr) Format(buf *TrackedBuffer) {
	buf.Myprintf("convert(%v using %s)", node.Expr, node.Type)
}

func (node *ConvertUsingExpr) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Expr,
	)
}

func (node *ConvertUsingExpr) replace(from, to Expr) bool {
	return replaceExprs(from, to, &node.Expr)
}

// ConvertType represents the type in call to CONVERT(expr, type)
type ConvertType struct {
	Type     string
	Length   *SQLVal
	Scale    *SQLVal
	Operator string
	Charset  string
}

// this string is "character set" and this comment is required
const (
	CharacterSetStr = " character set"
	CharsetStr      = "charset"
)

// Format formats the node.
func (node *ConvertType) Format(buf *TrackedBuffer) {
	buf.Myprintf("%s", node.Type)
	if node.Length != nil {
		buf.Myprintf("(%v", node.Length)
		if node.Scale != nil {
			buf.Myprintf(", %v", node.Scale)
		}
		buf.Myprintf(")")
	}
	if node.Charset != "" {
		buf.Myprintf("%s %s", node.Operator, node.Charset)
	}
}

// MatchExpr represents a call to the MATCH function
type MatchExpr struct {
	Columns SelectExprs
	Expr    Expr
	Option  string
}

// MatchExpr.Option
const (
	BooleanModeStr                           = " in boolean mode"
	NaturalLanguageModeStr                   = " in natural language mode"
	NaturalLanguageModeWithQueryExpansionStr = " in natural language mode with query expansion"
	QueryExpansionStr                        = " with query expansion"
)

// Format formats the node
func (node *MatchExpr) Format(buf *TrackedBuffer) {
	buf.Myprintf("match(%v) against (%v%s)", node.Columns, node.Expr, node.Option)
}

func (node *MatchExpr) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Columns,
		node.Expr,
	)
}

func (node *MatchExpr) replace(from, to Expr) bool {
	for _, sel := range node.Columns {
		aliased, ok := sel.(*AliasedExpr)
		if !ok {
			continue
		}
		if replaceExprs(from, to, &aliased.Expr) {
			return true
		}
	}
	return replaceExprs(from, to, &node.Expr)
}

// CaseExpr represents a CASE expression.
type CaseExpr struct {
	Expr  Expr
	Whens []*When
	Else  Expr
}

// Format formats the node.
func (node *CaseExpr) Format(buf *TrackedBuffer) {
	buf.Myprintf("case ")
	if node.Expr != nil {
		buf.Myprintf("%v ", node.Expr)
	}
	for _, when := range node.Whens {
		buf.Myprintf("%v ", when)
	}
	if node.Else != nil {
		buf.Myprintf("else %v ", node.Else)
	}
	buf.Myprintf("end")
}

func (node *CaseExpr) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	if err := Walk(visit, node.Expr); err != nil {
		return err
	}
	for _, n := range node.Whens {
		if err := Walk(visit, n); err != nil {
			return err
		}
	}
	return Walk(visit, node.Else)
}

func (node *CaseExpr) replace(from, to Expr) bool {
	for _, when := range node.Whens {
		if replaceExprs(from, to, &when.Cond, &when.Val) {
			return true
		}
	}
	return replaceExprs(from, to, &node.Expr, &node.Else)
}

// Default represents a DEFAULT expression.
type Default struct {
	ColName string
}

// Format formats the node.
func (node *Default) Format(buf *TrackedBuffer) {
	buf.Myprintf("default")
	if node.ColName != "" {
		buf.Myprintf("(%s)", node.ColName)
	}
}

func (node *Default) replace(from, to Expr) bool {
	return false
}

// When represents a WHEN sub-expression.
type When struct {
	Cond Expr
	Val  Expr
}

// Format formats the node.
func (node *When) Format(buf *TrackedBuffer) {
	buf.Myprintf("when %v then %v", node.Cond, node.Val)
}

func (node *When) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Cond,
		node.Val,
	)
}

// GroupBy represents a GROUP BY clause.
type GroupBy []Expr

// Format formats the node.
func (node GroupBy) Format(buf *TrackedBuffer) {
	prefix := " group by "
	for _, n := range node {
		buf.Myprintf("%s%v", prefix, n)
		prefix = ", "
	}
}

func (node GroupBy) walkSubtree(visit Visit) error {
	for _, n := range node {
		if err := Walk(visit, n); err != nil {
			return err
		}
	}
	return nil
}

// OrderBy represents an ORDER By clause.
type OrderBy []*Order

// Format formats the node.
func (node OrderBy) Format(buf *TrackedBuffer) {
	prefix := " order by "
	for _, n := range node {
		buf.Myprintf("%s%v", prefix, n)
		prefix = ", "
	}
}

func (node OrderBy) walkSubtree(visit Visit) error {
	for _, n := range node {
		if err := Walk(visit, n); err != nil {
			return err
		}
	}
	return nil
}

// Order represents an ordering expression.
type Order struct {
	Expr      Expr
	Direction string
}

// Order.Direction
const (
	AscScr  = "asc"
	DescScr = "desc"
)

// Format formats the node.
func (node *Order) Format(buf *TrackedBuffer) {
	if node, ok := node.Expr.(*NullVal); ok {
		buf.Myprintf("%v", node)
		return
	}
	if node, ok := node.Expr.(*FuncExpr); ok {
		if node.Name.Lowered() == "rand" {
			buf.Myprintf("%v", node)
			return
		}
	}

	buf.Myprintf("%v %s", node.Expr, node.Direction)
}

func (node *Order) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Expr,
	)
}

type FrameUnit int

const (
	// RangeUnit matches by value comparison (e.g. 100 units cheaper)
	RangeUnit FrameUnit = iota
	// RowsUnit matches by row position offset (e.g. 1 row before)
	RowsUnit
)

// Frame represents a window Frame clause.
type Frame struct {
	Unit   FrameUnit
	Extent *FrameExtent
}

// Format formats the node.
func (node *Frame) Format(buf *TrackedBuffer) {
	if node == nil {
		return
	}
	switch node.Unit {
	case RangeUnit:
		buf.Myprintf("RANGE %v", node.Extent)
	case RowsUnit:
		buf.Myprintf("ROWS %v", node.Extent)
	}
}

func (node *Frame) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Extent,
	)
}

// FrameExtent defines the start and end bounds for a window frame.
type FrameExtent struct {
	Start, End *FrameBound
}

// Format formats the node.
func (node *FrameExtent) Format(buf *TrackedBuffer) {
	if node == nil {
		return
	}
	if node.End != nil {
		buf.Myprintf("BETWEEN %v AND %v", node.Start, node.End)
	} else {
		buf.Myprintf("%v", node.Start)
	}
}

func (node *FrameExtent) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Start,
		node.End,
	)
}

type BoundType int

const (
	// CurrentRow represents the current row position or value range
	CurrentRow BoundType = iota
	// UnboundedFollowing includes all rows after CURRENT ROW in the active partition
	UnboundedFollowing
	// UnboundedPreceding includes all rows before CURRENT ROW in the active partition
	UnboundedPreceding
	// ExprPreceding matches N rows or a value range after CURRENT ROW
	ExprPreceding
	// ExprFollowing matches N rows or a value range after CURRENT ROW
	ExprFollowing
)

// FrameBound defines one direction of row or range inclusion.
type FrameBound struct {
	Expr Expr
	Type BoundType
}

// Format formats the node.
func (node *FrameBound) Format(buf *TrackedBuffer) {
	if node == nil {
		return
	}

	switch node.Type {
	case CurrentRow:
		buf.Myprintf("CURRENT ROW")
	case UnboundedPreceding:
		buf.Myprintf("UNBOUNDED PRECEDING")
	case UnboundedFollowing:
		buf.Myprintf("UNBOUNDED FOLLOWING")
	case ExprPreceding:
		buf.Myprintf("%v PRECEDING", node.Expr)
	case ExprFollowing:
		buf.Myprintf("%v FOLLOWING", node.Expr)
	}
}

func (node *FrameBound) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Expr,
	)
}

// WindowDef represents a window clause definition
type WindowDef struct {
	// Name is used in WINDOW clauses
	Name ColIdent
	// NameRef is used in OVER clauses
	NameRef     ColIdent
	PartitionBy Exprs
	OrderBy     OrderBy
	Frame       *Frame
}

// Format formats the node.
func (node *WindowDef) Format(buf *TrackedBuffer) {
	var sep string
	if !node.NameRef.IsEmpty() {
		buf.Myprintf("%v", node.NameRef)
		sep = " "
	}
	if len(node.PartitionBy) > 0 {
		buf.Myprintf("%spartition by %v", sep, node.PartitionBy)
		sep = " "
	}
	// OrderBy always adds prefixed whitespace currently
	if len(node.OrderBy) > 0 {
		buf.Myprintf("%v", node.OrderBy)
		sep = " "
	}
	if node.Frame != nil {
		buf.Myprintf("%s%v", sep, node.Frame)
	}
}

func (node *WindowDef) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(visit, node.PartitionBy, node.OrderBy, node.Name)
}

// Window represents a WINDOW clause.
type Window []*WindowDef

// Format formats the node.
func (node Window) Format(buf *TrackedBuffer) {
	if node == nil {
		return
	}
	buf.Myprintf(" window ")
	var sep string
	for _, def := range node {
		buf.Myprintf("%s%v as (%v)", sep, def.Name, def)
		sep = ", "
	}
}

func (node Window) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	var err error
	for _, def := range node {
		err = Walk(visit, def.PartitionBy, def.OrderBy, def.Frame)
		if err != nil {
			return err
		}
	}
	return nil
}

// Limit represents a LIMIT clause.
type Limit struct {
	Offset, Rowcount Expr
}

// Format formats the node.
func (node *Limit) Format(buf *TrackedBuffer) {
	if node == nil {
		return
	}
	buf.Myprintf(" limit ")
	if node.Offset != nil {
		buf.Myprintf("%v, ", node.Offset)
	}
	buf.Myprintf("%v", node.Rowcount)
}

func (node *Limit) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Offset,
		node.Rowcount,
	)
}

// Values represents a VALUES clause.
type Values []ValTuple

// Format formats the node.
func (node Values) Format(buf *TrackedBuffer) {
	prefix := "values "
	for _, n := range node {
		buf.Myprintf("%s%v", prefix, n)
		prefix = ", "
	}
}

func (node Values) walkSubtree(visit Visit) error {
	for _, n := range node {
		if err := Walk(visit, n); err != nil {
			return err
		}
	}
	return nil
}

// AssignmentExprs represents a list of assignment expressions.
type AssignmentExprs []*AssignmentExpr

// Format formats the node.
func (node AssignmentExprs) Format(buf *TrackedBuffer) {
	var prefix string
	for _, n := range node {
		buf.Myprintf("%s%v", prefix, n)
		prefix = ", "
	}
}

func (node AssignmentExprs) walkSubtree(visit Visit) error {
	for _, n := range node {
		if err := Walk(visit, n); err != nil {
			return err
		}
	}
	return nil
}

// AssignmentExpr represents an assignment expression.
type AssignmentExpr struct {
	Name *ColName
	Expr Expr
}

// Format formats the node.
func (node *AssignmentExpr) Format(buf *TrackedBuffer) {
	buf.Myprintf("%s = %v", node.Name.String(), node.Expr)
}

func (node *AssignmentExpr) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Name,
		node.Expr,
	)
}

// SetVarExprs represents a list of set expressions.
type SetVarExprs []*SetVarExpr

// Format formats the node.
func (node SetVarExprs) Format(buf *TrackedBuffer) {
	var prefix string
	for _, n := range node {
		buf.Myprintf("%s%v", prefix, n)
		prefix = ", "
	}
}

func (node SetVarExprs) walkSubtree(visit Visit) error {
	for _, n := range node {
		if err := Walk(visit, n); err != nil {
			return err
		}
	}
	return nil
}

// SetScope represents the scope of the set expression.
type SetScope string

const (
	SetScope_None        SetScope = ""
	SetScope_Global      SetScope = "global"
	SetScope_Persist     SetScope = "persist"
	SetScope_PersistOnly SetScope = "persist_only"
	SetScope_Session     SetScope = "session"
	SetScope_User        SetScope = "user"
)

// VarScopeForColName returns the SetScope of the given ColName, along with a new ColName without the scope information.
func VarScopeForColName(colName *ColName) (*ColName, SetScope, error) {
	if colName.Qualifier.IsEmpty() { // Forms are like `@@x` and `@x`
		if strings.HasPrefix(colName.Name.val, "@") && strings.Index(colName.Name.val, ".") != -1 {
			varName, scope, err := VarScope(strings.Split(colName.Name.val, ".")...)
			if err != nil {
				return nil, SetScope_None, err
			}
			if scope == SetScope_None {
				return colName, scope, nil
			}
			return &ColName{Name: ColIdent{val: varName}}, scope, nil
		} else {
			varName, scope, err := VarScope(colName.Name.val)
			if err != nil {
				return nil, SetScope_None, err
			}
			if scope == SetScope_None {
				return colName, scope, nil
			}
			return &ColName{Name: ColIdent{val: varName}}, scope, nil
		}
	} else if colName.Qualifier.Qualifier.IsEmpty() { // Forms are like `@@GLOBAL.x` and `@@SESSION.x`
		varName, scope, err := VarScope(colName.Qualifier.Name.v, colName.Name.val)
		if err != nil {
			return nil, SetScope_None, err
		}
		if scope == SetScope_None {
			return colName, scope, nil
		}
		return &ColName{Name: ColIdent{val: varName}}, scope, nil
	} else { // Forms are like `@@GLOBAL.validate_password.length`, which is currently unsupported
		_, _, err := VarScope(colName.Qualifier.Qualifier.v, colName.Qualifier.Name.v, colName.Name.val)
		return colName, SetScope_None, err
	}
}

// VarScope returns the SetScope of the given name, broken into parts. For example, `@@GLOBAL.sys_var` would become
// `[]string{"@@GLOBAL", "sys_var"}`. Returns the variable name without any scope specifiers, so the aforementioned
// variable would simply return "sys_var". `[]string{"@@other_var"}` would return "other_var". If the name parts do not
// specify a variable (returns SetScope_None), then it is recommended to use the original non-broken string, as this
// will always only return the last part. `[]string{"my_db", "my_tbl", "my_col"}` will return "my_col" with SetScope_None.
func VarScope(nameParts ...string) (string, SetScope, error) {
	switch len(nameParts) {
	case 0:
		return "", SetScope_None, nil
	case 1:
		// First case covers `@@@`, `@@@@`, etc.
		if strings.HasPrefix(nameParts[0], "@@@") {
			return "", SetScope_None, fmt.Errorf("invalid system variable declaration `%s`", nameParts[0])
		} else if strings.HasPrefix(nameParts[0], "@@") {
			dotIdx := strings.Index(nameParts[0], ".")
			if dotIdx != -1 {
				return VarScope(nameParts[0][:dotIdx], nameParts[0][dotIdx+1:])
			}
			return nameParts[0][2:], SetScope_Session, nil
		} else if strings.HasPrefix(nameParts[0], "@") {
			return nameParts[0][1:], SetScope_User, nil
		} else {
			return nameParts[0], SetScope_None, nil
		}
	case 2:
		// `@user.var` is valid, so we check for it here.
		if len(nameParts[0]) >= 2 && nameParts[0][0] == '@' && nameParts[0][1] != '@' &&
			!strings.HasPrefix(nameParts[1], "@") { // `@user.@var` is invalid though.
			return fmt.Sprintf("%s.%s", nameParts[0][1:], nameParts[1]), SetScope_User, nil
		}
		// We don't support variables such as `@@validate_password.length` right now, only `@@GLOBAL.sys_var`, etc.
		// The `@` symbols are only valid on the first name_part. First case also catches `@@@`, etc.
		if strings.HasPrefix(nameParts[1], "@@") {
			return "", SetScope_None, fmt.Errorf("invalid system variable declaration `%s`", nameParts[1])
		} else if strings.HasPrefix(nameParts[1], "@") {
			return "", SetScope_None, fmt.Errorf("invalid user variable declaration `%s`", nameParts[1])
		}
		switch strings.ToLower(nameParts[0]) {
		case "@@global":
			if strings.HasPrefix(nameParts[1], `"`) || strings.HasPrefix(nameParts[1], `'`) {
				return "", SetScope_None, fmt.Errorf("invalid system variable declaration `%s`", nameParts[1])
			}
			return nameParts[1], SetScope_Global, nil
		case "@@persist":
			if strings.HasPrefix(nameParts[1], `"`) || strings.HasPrefix(nameParts[1], `'`) {
				return "", SetScope_None, fmt.Errorf("invalid system variable declaration `%s`", nameParts[1])
			}
			return nameParts[1], SetScope_Persist, nil
		case "@@persist_only":
			if strings.HasPrefix(nameParts[1], `"`) || strings.HasPrefix(nameParts[1], `'`) {
				return "", SetScope_None, fmt.Errorf("invalid system variable declaration `%s`", nameParts[1])
			}
			return nameParts[1], SetScope_PersistOnly, nil
		case "@@session":
			if strings.HasPrefix(nameParts[1], `"`) || strings.HasPrefix(nameParts[1], `'`) {
				return "", SetScope_None, fmt.Errorf("invalid system variable declaration `%s`", nameParts[1])
			}
			return nameParts[1], SetScope_Session, nil
		case "@@local":
			if strings.HasPrefix(nameParts[1], `"`) || strings.HasPrefix(nameParts[1], `'`) {
				return "", SetScope_None, fmt.Errorf("invalid system variable declaration `%s`", nameParts[1])
			}
			return nameParts[1], SetScope_Session, nil
		default:
			// This catches `@@@GLOBAL.sys_var`. Due to the earlier check, this does not error on `@user.var`.
			if strings.HasPrefix(nameParts[0], "@") {
				// Last value is column name, so we return that in the error
				return "", SetScope_None, fmt.Errorf("invalid system variable declaration `%s`", nameParts[1])
			}
			return nameParts[1], SetScope_None, nil
		}
	default:
		// `@user.var.name` is valid, so we check for it here.
		if len(nameParts[0]) >= 2 && nameParts[0][0] == '@' && nameParts[0][1] != '@' {
			// `@` may only appear in the first name part for user variables
			for i := 1; i < len(nameParts); i++ {
				if strings.HasPrefix(nameParts[i], "@") {
					// Last value is column name, so we return that in the error
					return "", SetScope_None, fmt.Errorf("invalid user variable declaration `%s`", nameParts[len(nameParts)-1])
				}
			}
			return strings.Join(append([]string{nameParts[0][1:]}, nameParts[1:]...), "."), SetScope_User, nil
		}
		// As we don't support `@@GLOBAL.validate_password.length` or anything potentially longer, we error if any part
		// starts with either `@@` or `@`. We can just check for `@` though.
		for _, namePart := range nameParts {
			if strings.HasPrefix(namePart, "@") {
				// Last value is column name, so we return that in the error
				return "", SetScope_None, fmt.Errorf("invalid system variable declaration `%s`", nameParts[len(nameParts)-1])
			}
		}
		return nameParts[len(nameParts)-1], SetScope_None, nil
	}
}

// SetVarExpr represents a set expression.
type SetVarExpr struct {
	Scope SetScope
	Name  *ColName
	Expr  Expr
}

// SetVarExpr.Expr, for SET TRANSACTION ... or START TRANSACTION
const (
	// TransactionStr is the Name for a SET TRANSACTION statement
	TransactionStr = "transaction"

	IsolationLevelReadUncommitted = "isolation level read uncommitted"
	IsolationLevelReadCommitted   = "isolation level read committed"
	IsolationLevelRepeatableRead  = "isolation level repeatable read"
	IsolationLevelSerializable    = "isolation level serializable"

	TxReadOnly  = "read only"
	TxReadWrite = "read write"
)

// Format formats the node.
func (node *SetVarExpr) Format(buf *TrackedBuffer) {
	// We don't have to backtick set variable names.
	if node.Name.EqualString("charset") || node.Name.EqualString("names") {
		buf.Myprintf("%s %v", node.Name.String(), node.Expr)
	} else if node.Name.EqualString(TransactionStr) {
		sqlVal := node.Expr.(*SQLVal)
		buf.Myprintf("%s %s", node.Name.String(), strings.ToLower(string(sqlVal.Val)))
	} else {
		switch node.Scope {
		case SetScope_None:
			buf.Myprintf("%s = %v", node.Name.String(), node.Expr)
		case SetScope_User:
			buf.Myprintf("@%s = %v", node.Name.String(), node.Expr)
		default:
			buf.Myprintf("%s %s = %v", string(node.Scope), node.Name.String(), node.Expr)
		}
	}
}

func (node *SetVarExpr) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(
		visit,
		node.Name,
		node.Expr,
	)
}

// OnDup represents an ON DUPLICATE KEY clause.
type OnDup AssignmentExprs

// Format formats the node.
func (node OnDup) Format(buf *TrackedBuffer) {
	if node == nil {
		return
	}
	buf.Myprintf(" on duplicate key update %v", AssignmentExprs(node))
}

func (node OnDup) walkSubtree(visit Visit) error {
	return Walk(visit, AssignmentExprs(node))
}

// Savepoint represents a SAVEPOINT statement.
type Savepoint struct {
	Identifier string
}

var _ SQLNode = (*Savepoint)(nil)

// Format implements the SQLNode interface.
func (node *Savepoint) Format(buf *TrackedBuffer) {
	buf.Myprintf("savepoint %s", node.Identifier)
}

// RollbackSavepoint represents a ROLLBACK TO statement.
type RollbackSavepoint struct {
	Identifier string
}

var _ SQLNode = (*RollbackSavepoint)(nil)

// Format implements the SQLNode interface.
func (node *RollbackSavepoint) Format(buf *TrackedBuffer) {
	buf.Myprintf("rollback to %s", node.Identifier)
}

// ReleaseSavepoint represents a RELEASE SAVEPOINT statement.
type ReleaseSavepoint struct {
	Identifier string
}

var _ SQLNode = (*ReleaseSavepoint)(nil)

// Format implements the SQLNode interface.
func (node *ReleaseSavepoint) Format(buf *TrackedBuffer) {
	buf.Myprintf("release savepoint %s", node.Identifier)
}

// ColIdent is a case insensitive SQL identifier. It will be escaped with
// backquotes if necessary.
type ColIdent struct {
	// This artifact prevents this struct from being compared
	// with itself. It consumes no space as long as it's not the
	// last field in the struct.
	_            [0]struct{ _ []byte }
	val, lowered string
}

// NewColIdent makes a new ColIdent.
func NewColIdent(str string) ColIdent {
	return ColIdent{
		val: str,
	}
}

// Format formats the node.
func (node ColIdent) Format(buf *TrackedBuffer) {
	formatID(buf, node.val, node.Lowered())
}

// IsEmpty returns true if the name is empty.
func (node ColIdent) IsEmpty() bool {
	return node.val == ""
}

// String returns the unescaped column name. It must
// not be used for SQL generation. Use sqlparser.String
// instead. The Stringer conformance is for usage
// in templates.
func (node ColIdent) String() string {
	return node.val
}

// CompliantName returns a compliant id name
// that can be used for a bind var.
func (node ColIdent) CompliantName() string {
	return compliantName(node.val)
}

// Lowered returns a lower-cased column name.
// This function should generally be used only for optimizing
// comparisons.
func (node ColIdent) Lowered() string {
	if node.val == "" {
		return ""
	}
	if node.lowered == "" {
		node.lowered = strings.ToLower(node.val)
	}
	return node.lowered
}

// Equal performs a case-insensitive compare.
func (node ColIdent) Equal(in ColIdent) bool {
	return node.Lowered() == in.Lowered()
}

// EqualString performs a case-insensitive compare with str.
func (node ColIdent) EqualString(str string) bool {
	return node.Lowered() == strings.ToLower(str)
}

// MarshalJSON marshals into JSON.
func (node ColIdent) MarshalJSON() ([]byte, error) {
	return json.Marshal(node.val)
}

// UnmarshalJSON unmarshals from JSON.
func (node *ColIdent) UnmarshalJSON(b []byte) error {
	var result string
	err := json.Unmarshal(b, &result)
	if err != nil {
		return err
	}
	node.val = result
	return nil
}

type TableFuncExpr struct {
	Name  string
	Exprs SelectExprs
}

// Format formats the node.
func (node TableFuncExpr) Format(buf *TrackedBuffer) {
	buf.Myprintf("%s(%v)", node.Name, node.Exprs)
}

// IsEmpty returns true if TableFuncExpr's name is empty.
func (node TableFuncExpr) IsEmpty() bool {
	return node.Name == ""
}

// String returns the unescaped table function name. It must
// not be used for SQL generation. Use sqlparser.String
// instead. The Stringer conformance is for usage
// in templates.
func (node TableFuncExpr) String() string {
	return node.Name
}

// CompliantName returns a compliant id name
// that can be used for a bind var.
func (node TableFuncExpr) CompliantName() string {
	return compliantName(node.Name)
}

// MarshalJSON marshals into JSON.
func (node TableFuncExpr) MarshalJSON() ([]byte, error) {
	return json.Marshal(node.Name)
}

// UnmarshalJSON unmarshals from JSON.
func (node *TableFuncExpr) UnmarshalJSON(b []byte) error {
	var result string
	err := json.Unmarshal(b, &result)
	if err != nil {
		return err
	}
	node.Name = result
	return nil
}

// TableIdent is a case sensitive SQL identifier. It will be escaped with
// backquotes if necessary.
type TableIdent struct {
	v string
}

// NewTableIdent creates a new TableIdent.
func NewTableIdent(str string) TableIdent {
	return TableIdent{v: str}
}

// Format formats the node.
func (node TableIdent) Format(buf *TrackedBuffer) {
	formatID(buf, node.v, strings.ToLower(node.v))
}

// IsEmpty returns true if TabIdent is empty.
func (node TableIdent) IsEmpty() bool {
	return node.v == ""
}

// String returns the unescaped table name. It must
// not be used for SQL generation. Use sqlparser.String
// instead. The Stringer conformance is for usage
// in templates.
func (node TableIdent) String() string {
	return node.v
}

// CompliantName returns a compliant id name
// that can be used for a bind var.
func (node TableIdent) CompliantName() string {
	return compliantName(node.v)
}

// MarshalJSON marshals into JSON.
func (node TableIdent) MarshalJSON() ([]byte, error) {
	return json.Marshal(node.v)
}

// UnmarshalJSON unmarshals from JSON.
func (node *TableIdent) UnmarshalJSON(b []byte) error {
	var result string
	err := json.Unmarshal(b, &result)
	if err != nil {
		return err
	}
	node.v = result
	return nil
}

func formatID(buf *TrackedBuffer, original, lowered string) {
	isDbSystemVariable := false
	if len(original) > 1 && original[:2] == "@@" {
		isDbSystemVariable = true
	}

	for i, c := range original {
		if !(isLetter(uint16(c)) || c == '@') && (!isDbSystemVariable || !isCarat(uint16(c))) {
			if i == 0 || !isDigit(uint16(c)) {
				goto mustEscape
			}
		}
	}
	if _, ok := keywords[lowered]; ok {
		goto mustEscape
	}
	buf.Myprintf("%s", original)
	return

mustEscape:
	buf.WriteByte('`')
	for _, c := range original {
		buf.WriteRune(c)
		if c == '`' {
			buf.WriteByte('`')
		}
	}
	buf.WriteByte('`')
}

// LockType is an enum for Lock Types
type LockType string

const (
	LockRead             LockType = "read"
	LockWrite            LockType = "write"
	LockReadLocal        LockType = "read local"
	LockLowPriorityWrite LockType = "low_priority write"
)

// TableAndLockType contains table and lock association
type TableAndLockType struct {
	Table TableExpr
	Lock  LockType
	SQLNode
}

func (node *TableAndLockType) Format(buf *TrackedBuffer) {
	buf.Myprintf("%v %s", node.Table, string(node.Lock))
}

func (node *TableAndLockType) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}

	return Walk(
		visit,
		node.Table)
}

type TableAndLockTypes []*TableAndLockType

// LockTables represents the lock statement
type LockTables struct {
	Tables TableAndLockTypes
	SQLNode
}

func (node *LockTables) Format(buf *TrackedBuffer) {
	buf.WriteString("lock tables")
	for i, lt := range node.Tables {
		if i == 0 {
			buf.Myprintf(" %v", lt)
		} else {
			buf.Myprintf(", %v", lt)
		}
	}
}

func (node *LockTables) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}

	for _, t := range node.Tables {
		err := Walk(visit, t)
		if err != nil {
			return err
		}
	}

	return nil
}

// UnlockTables represents the unlock statement
type UnlockTables struct{}

func (node *UnlockTables) Format(buf *TrackedBuffer) {
	buf.WriteString("unlock tables")
}

func (node *UnlockTables) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}

	return nil
}

type Kill struct {
	Connection bool
	ConnID     Expr
}

func (k *Kill) Format(buf *TrackedBuffer) {
	buf.WriteString("kill ")
	if k.Connection {
		buf.WriteString("connection ")
	} else {
		buf.WriteString("query ")
	}
	buf.Myprintf("%v", k.ConnID)
}

func (*Kill) iStatement() {}

func (k *Kill) walkSubtree(visit Visit) error {
	if k == nil {
		return nil
	}
	return Walk(visit, k.ConnID)
}

func compliantName(in string) string {
	var buf strings.Builder
	for i, c := range in {
		if !isLetter(uint16(c)) {
			if i == 0 || !isDigit(uint16(c)) {
				buf.WriteByte('_')
				continue
			}
		}
		buf.WriteRune(c)
	}
	return buf.String()
}

type Analyze struct {
	Tables TableNames
}

func (*Analyze) iStatement() {}

func (node *Analyze) walkSubtree(visit Visit) error {
	if node == nil {
		return nil
	}
	return Walk(visit, node.Tables)
}

func (node *Analyze) Format(buf *TrackedBuffer) {
	buf.Myprintf("analyze table %v", node.Tables)
}

type Prepare struct {
	Name string
	Expr string
}

func (*Prepare) iStatement() {}

func (node *Prepare) walkSubtree(visit Visit) error {
	return nil
}

func (node *Prepare) Format(buf *TrackedBuffer) {
	buf.Myprintf("prepare %s from '%s'", node.Name, node.Expr)
}

type Execute struct {
	Name    string
	VarList []string
}

func (*Execute) iStatement() {}

func (node *Execute) walkSubtree(visit Visit) error {
	return nil
}

func (node *Execute) Format(buf *TrackedBuffer) {
	if len(node.VarList) == 0 {
		buf.Myprintf("execute %s", node.Name)
	} else {
		varList := strings.Join(node.VarList, ", ")
		buf.Myprintf("execute %s using %s", node.Name, varList)
	}
}

type Deallocate struct {
	Name string
}

func (*Deallocate) iStatement() {}

func (node *Deallocate) walkSubtree(visit Visit) error {
	return nil
}

func (node *Deallocate) Format(buf *TrackedBuffer) {
	buf.Myprintf("deallocate prepare %s", node.Name)
}
