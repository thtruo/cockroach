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

// This code was derived from https://github.com/youtube/vitess.
//
// Copyright 2012, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file

package parser

import (
	"bytes"
	"fmt"

	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/types"

	"golang.org/x/text/language"

	"github.com/pkg/errors"
)

// CreateDatabase represents a CREATE DATABASE statement.
type CreateDatabase struct {
	IfNotExists bool
	Name        Name
	Template    string
	Encoding    string
	Collate     string
	CType       string
}

// Format implements the NodeFormatter interface.
func (node *CreateDatabase) Format(buf *bytes.Buffer, f FmtFlags) {
	buf.WriteString("CREATE DATABASE ")
	if node.IfNotExists {
		buf.WriteString("IF NOT EXISTS ")
	}
	FormatNode(buf, f, node.Name)
	if node.Template != "" {
		buf.WriteString(" TEMPLATE = ")
		encodeSQLStringWithFlags(buf, node.Template, f)
	}
	if node.Encoding != "" {
		buf.WriteString(" ENCODING = ")
		encodeSQLStringWithFlags(buf, node.Encoding, f)
	}
	if node.Collate != "" {
		buf.WriteString(" LC_COLLATE = ")
		encodeSQLStringWithFlags(buf, node.Collate, f)
	}
	if node.CType != "" {
		buf.WriteString(" LC_CTYPE = ")
		encodeSQLStringWithFlags(buf, node.CType, f)
	}
}

// IndexElem represents a column with a direction in a CREATE INDEX statement.
type IndexElem struct {
	Column    Name
	Direction Direction
}

// Format implements the NodeFormatter interface.
func (node IndexElem) Format(buf *bytes.Buffer, f FmtFlags) {
	FormatNode(buf, f, node.Column)
	if node.Direction != DefaultDirection {
		buf.WriteByte(' ')
		buf.WriteString(node.Direction.String())
	}
}

// IndexElemList is list of IndexElem.
type IndexElemList []IndexElem

// Format pretty-prints the contained names separated by commas.
// Format implements the NodeFormatter interface.
func (l IndexElemList) Format(buf *bytes.Buffer, f FmtFlags) {
	for i, indexElem := range l {
		if i > 0 {
			buf.WriteString(", ")
		}
		FormatNode(buf, f, indexElem)
	}
}

// CreateIndex represents a CREATE INDEX statement.
type CreateIndex struct {
	Name        Name
	Table       NormalizableTableName
	Unique      bool
	IfNotExists bool
	Columns     IndexElemList
	// Extra columns to be stored together with the indexed ones as an optimization
	// for improved reading performance.
	Storing    NameList
	Interleave *InterleaveDef
}

// Format implements the NodeFormatter interface.
func (node *CreateIndex) Format(buf *bytes.Buffer, f FmtFlags) {
	buf.WriteString("CREATE ")
	if node.Unique {
		buf.WriteString("UNIQUE ")
	}
	buf.WriteString("INDEX ")
	if node.IfNotExists {
		buf.WriteString("IF NOT EXISTS ")
	}
	if node.Name != "" {
		FormatNode(buf, f, node.Name)
		buf.WriteByte(' ')
	}
	buf.WriteString("ON ")
	FormatNode(buf, f, &node.Table)
	buf.WriteString(" (")
	FormatNode(buf, f, node.Columns)
	buf.WriteByte(')')
	if len(node.Storing) > 0 {
		buf.WriteString(" STORING (")
		FormatNode(buf, f, node.Storing)
		buf.WriteByte(')')
	}
	if node.Interleave != nil {
		FormatNode(buf, f, node.Interleave)
	}
}

// TableDef represents a column, index or constraint definition within a CREATE
// TABLE statement.
type TableDef interface {
	NodeFormatter
	// Placeholder function to ensure that only desired types (*TableDef) conform
	// to the TableDef interface.
	tableDef()
	setName(name Name)
}

func (*ColumnTableDef) tableDef() {}
func (*IndexTableDef) tableDef()  {}
func (*FamilyTableDef) tableDef() {}

// TableDefs represents a list of table definitions.
type TableDefs []TableDef

// Format implements the NodeFormatter interface.
func (node TableDefs) Format(buf *bytes.Buffer, f FmtFlags) {
	for i, n := range node {
		if i > 0 {
			buf.WriteString(", ")
		}
		FormatNode(buf, f, n)
	}
}

// Nullability represents either NULL, NOT NULL or an unspecified value (silent
// NULL).
type Nullability int

// The values for NullType.
const (
	NotNull Nullability = iota
	Null
	SilentNull
)

// ColumnTableDef represents a column definition within a CREATE TABLE
// statement.
type ColumnTableDef struct {
	Name     Name
	Type     ColumnType
	Nullable struct {
		Nullability    Nullability
		ConstraintName Name
	}
	PrimaryKey           bool
	Unique               bool
	UniqueConstraintName Name
	DefaultExpr          struct {
		Expr           Expr
		ConstraintName Name
	}
	CheckExprs []ColumnTableDefCheckExpr
	References struct {
		Table          NormalizableTableName
		Col            Name
		ConstraintName Name
		Actions        ReferenceActions
	}
	Family struct {
		Name        Name
		Create      bool
		IfNotExists bool
	}
}

// ColumnTableDefCheckExpr represents a check constraint on a column definition
// within a CREATE TABLE statement.
type ColumnTableDefCheckExpr struct {
	Expr           Expr
	ConstraintName Name
}

func processCollationOnType(name Name, typ ColumnType, c ColumnCollation) (ColumnType, error) {
	locale := string(c)
	switch s := typ.(type) {
	case *StringColType:
		return &CollatedStringColType{s.Name, s.N, locale}, nil
	case *CollatedStringColType:
		return nil, pgerror.NewErrorf(pgerror.CodeSyntaxError,
			"multiple COLLATE declarations for column %q", name)
	case *ArrayColType:
		var err error
		s.ParamType, err = processCollationOnType(name, s.ParamType, c)
		if err != nil {
			return nil, err
		}
		return s, nil
	default:
		return nil, pgerror.NewErrorf(pgerror.CodeDatatypeMismatchError,
			"COLLATE declaration for non-string-typed column %q", name)
	}
}

func newColumnTableDef(
	name Name, typ ColumnType, qualifications []NamedColumnQualification,
) (*ColumnTableDef, error) {
	d := &ColumnTableDef{
		Name: name,
		Type: typ,
	}
	d.Nullable.Nullability = SilentNull
	for _, c := range qualifications {
		switch t := c.Qualification.(type) {
		case ColumnCollation:
			locale := string(t)
			_, err := language.Parse(locale)
			if err != nil {
				return nil, errors.Wrapf(err, "invalid locale %s", locale)
			}
			d.Type, err = processCollationOnType(name, d.Type, t)
			if err != nil {
				return nil, err
			}
		case *ColumnDefault:
			if d.HasDefaultExpr() {
				return nil, pgerror.NewErrorf(pgerror.CodeSyntaxError,
					"multiple default values specified for column %q", name)
			}
			d.DefaultExpr.Expr = t.Expr
			d.DefaultExpr.ConstraintName = c.Name
		case NotNullConstraint:
			if d.Nullable.Nullability == Null {
				return nil, pgerror.NewErrorf(pgerror.CodeSyntaxError,
					"conflicting NULL/NOT NULL declarations for column %q", name)
			}
			d.Nullable.Nullability = NotNull
			d.Nullable.ConstraintName = c.Name
		case NullConstraint:
			if d.Nullable.Nullability == NotNull {
				return nil, pgerror.NewErrorf(pgerror.CodeSyntaxError,
					"conflicting NULL/NOT NULL declarations for column %q", name)
			}
			d.Nullable.Nullability = Null
			d.Nullable.ConstraintName = c.Name
		case PrimaryKeyConstraint:
			d.PrimaryKey = true
			d.UniqueConstraintName = c.Name
		case UniqueConstraint:
			d.Unique = true
			d.UniqueConstraintName = c.Name
		case *ColumnCheckConstraint:
			d.CheckExprs = append(d.CheckExprs, ColumnTableDefCheckExpr{
				Expr:           t.Expr,
				ConstraintName: c.Name,
			})
		case *ColumnFKConstraint:
			if d.HasFKConstraint() {
				return nil, pgerror.NewErrorf(pgerror.CodeInvalidTableDefinitionError,
					"multiple foreign key constraints specified for column %q", name)
			}
			d.References.Table = t.Table
			d.References.Col = t.Col
			d.References.ConstraintName = c.Name
			d.References.Actions = t.Actions
		case *ColumnFamilyConstraint:
			if d.HasColumnFamily() {
				return nil, pgerror.NewErrorf(pgerror.CodeInvalidTableDefinitionError,
					"multiple column families specified for column %q", name)
			}
			d.Family.Name = t.Family
			d.Family.Create = t.Create
			d.Family.IfNotExists = t.IfNotExists
		default:
			panic(fmt.Sprintf("unexpected column qualification: %T", c))
		}
	}
	return d, nil
}

func (node *ColumnTableDef) setName(name Name) {
	node.Name = name
}

// HasDefaultExpr returns if the ColumnTableDef has a default expression.
func (node *ColumnTableDef) HasDefaultExpr() bool {
	return node.DefaultExpr.Expr != nil
}

// HasFKConstraint returns if the ColumnTableDef has a foreign key constraint.
func (node *ColumnTableDef) HasFKConstraint() bool {
	return node.References.Table.TableNameReference != nil
}

// HasColumnFamily returns if the ColumnTableDef has a column family.
func (node *ColumnTableDef) HasColumnFamily() bool {
	return node.Family.Name != "" || node.Family.Create
}

// Format implements the NodeFormatter interface.
func (node *ColumnTableDef) Format(buf *bytes.Buffer, f FmtFlags) {
	FormatNode(buf, f, node.Name)
	buf.WriteByte(' ')
	FormatNode(buf, f, node.Type)
	if node.Nullable.Nullability != SilentNull && node.Nullable.ConstraintName != "" {
		buf.WriteString(" CONSTRAINT ")
		FormatNode(buf, f, node.Nullable.ConstraintName)
	}
	switch node.Nullable.Nullability {
	case Null:
		buf.WriteString(" NULL")
	case NotNull:
		buf.WriteString(" NOT NULL")
	}
	if node.PrimaryKey {
		buf.WriteString(" PRIMARY KEY")
	} else if node.Unique {
		buf.WriteString(" UNIQUE")
	}
	if node.HasDefaultExpr() {
		if node.DefaultExpr.ConstraintName != "" {
			buf.WriteString(" CONSTRAINT ")
			FormatNode(buf, f, node.DefaultExpr.ConstraintName)
		}
		buf.WriteString(" DEFAULT ")
		FormatNode(buf, f, node.DefaultExpr.Expr)
	}
	for _, checkExpr := range node.CheckExprs {
		if checkExpr.ConstraintName != "" {
			buf.WriteString(" CONSTRAINT ")
			FormatNode(buf, f, checkExpr.ConstraintName)
		}
		buf.WriteString(" CHECK (")
		FormatNode(buf, f, checkExpr.Expr)
		buf.WriteByte(')')
	}
	if node.HasFKConstraint() {
		if node.References.ConstraintName != "" {
			buf.WriteString(" CONSTRAINT ")
			FormatNode(buf, f, node.References.ConstraintName)
		}
		buf.WriteString(" REFERENCES ")
		FormatNode(buf, f, &node.References.Table)
		if node.References.Col != "" {
			buf.WriteString(" (")
			FormatNode(buf, f, node.References.Col)
			buf.WriteByte(')')
		}
		FormatNode(buf, f, node.References.Actions)
	}
	if node.HasColumnFamily() {
		if node.Family.Create {
			buf.WriteString(" CREATE")
		}
		if node.Family.IfNotExists {
			buf.WriteString(" IF NOT EXISTS")
		}
		buf.WriteString(" FAMILY")
		if len(node.Family.Name) > 0 {
			buf.WriteByte(' ')
			FormatNode(buf, f, node.Family.Name)
		}
	}
}

// NamedColumnQualification wraps a NamedColumnQualification with a name.
type NamedColumnQualification struct {
	Name          Name
	Qualification ColumnQualification
}

// ColumnQualification represents a constraint on a column.
type ColumnQualification interface {
	columnQualification()
}

func (ColumnCollation) columnQualification()         {}
func (*ColumnDefault) columnQualification()          {}
func (NotNullConstraint) columnQualification()       {}
func (NullConstraint) columnQualification()          {}
func (PrimaryKeyConstraint) columnQualification()    {}
func (UniqueConstraint) columnQualification()        {}
func (*ColumnCheckConstraint) columnQualification()  {}
func (*ColumnFKConstraint) columnQualification()     {}
func (*ColumnFamilyConstraint) columnQualification() {}

// ColumnCollation represents a COLLATE clause for a column.
type ColumnCollation string

// ColumnDefault represents a DEFAULT clause for a column.
type ColumnDefault struct {
	Expr Expr
}

// NotNullConstraint represents NOT NULL on a column.
type NotNullConstraint struct{}

// NullConstraint represents NULL on a column.
type NullConstraint struct{}

// PrimaryKeyConstraint represents NULL on a column.
type PrimaryKeyConstraint struct{}

// UniqueConstraint represents UNIQUE on a column.
type UniqueConstraint struct{}

// ColumnCheckConstraint represents either a check on a column.
type ColumnCheckConstraint struct {
	Expr Expr
}

// ColumnFKConstraint represents a FK-constaint on a column.
type ColumnFKConstraint struct {
	Table   NormalizableTableName
	Col     Name // empty-string means use PK
	Actions ReferenceActions
}

// ColumnFamilyConstraint represents FAMILY on a column.
type ColumnFamilyConstraint struct {
	Family      Name
	Create      bool
	IfNotExists bool
}

// IndexTableDef represents an index definition within a CREATE TABLE
// statement.
type IndexTableDef struct {
	Name       Name
	Columns    IndexElemList
	Storing    NameList
	Interleave *InterleaveDef
}

func (node *IndexTableDef) setName(name Name) {
	node.Name = name
}

// Format implements the NodeFormatter interface.
func (node *IndexTableDef) Format(buf *bytes.Buffer, f FmtFlags) {
	buf.WriteString("INDEX ")
	if node.Name != "" {
		FormatNode(buf, f, node.Name)
		buf.WriteByte(' ')
	}
	buf.WriteByte('(')
	FormatNode(buf, f, node.Columns)
	buf.WriteByte(')')
	if node.Storing != nil {
		buf.WriteString(" STORING (")
		FormatNode(buf, f, node.Storing)
		buf.WriteByte(')')
	}
	if node.Interleave != nil {
		FormatNode(buf, f, node.Interleave)
	}
}

// ConstraintTableDef represents a constraint definition within a CREATE TABLE
// statement.
type ConstraintTableDef interface {
	TableDef
	// Placeholder function to ensure that only desired types
	// (*ConstraintTableDef) conform to the ConstraintTableDef interface.
	constraintTableDef()
}

func (*UniqueConstraintTableDef) constraintTableDef() {}

// UniqueConstraintTableDef represents a unique constraint within a CREATE
// TABLE statement.
type UniqueConstraintTableDef struct {
	IndexTableDef
	PrimaryKey bool
}

// Format implements the NodeFormatter interface.
func (node *UniqueConstraintTableDef) Format(buf *bytes.Buffer, f FmtFlags) {
	if node.Name != "" {
		buf.WriteString("CONSTRAINT ")
		FormatNode(buf, f, node.Name)
		buf.WriteByte(' ')
	}
	if node.PrimaryKey {
		buf.WriteString("PRIMARY KEY ")
	} else {
		buf.WriteString("UNIQUE ")
	}
	buf.WriteByte('(')
	FormatNode(buf, f, node.Columns)
	buf.WriteByte(')')
	if node.Storing != nil {
		buf.WriteString(" STORING (")
		FormatNode(buf, f, node.Storing)
		buf.WriteByte(')')
	}
	if node.Interleave != nil {
		FormatNode(buf, f, node.Interleave)
	}
}

// ReferenceAction is the method used to maintain referential integrity through
// foreign keys.
type ReferenceAction int

// The values for ReferenceAction.
const (
	NoAction ReferenceAction = iota
	Restrict
	SetNull
	SetDefault
	Cascade
)

var referenceActionName = [...]string{
	NoAction:   "NO ACTION",
	Restrict:   "RESTRICT",
	SetNull:    "SET NULL",
	SetDefault: "SET DEFAULT",
	Cascade:    "CASCADE",
}

func (ra ReferenceAction) String() string {
	return referenceActionName[ra]
}

// ReferenceActions contains the actions specified to maintain referential
// integrity through foreign keys for different operations.
type ReferenceActions struct {
	Delete ReferenceAction
	Update ReferenceAction
}

// Format implements the NodeFormatter interface.
func (node ReferenceActions) Format(buf *bytes.Buffer, f FmtFlags) {
	if node.Delete != NoAction {
		buf.WriteString(" ON DELETE ")
		buf.WriteString(node.Delete.String())
	}
	if node.Update != NoAction {
		buf.WriteString(" ON UPDATE ")
		buf.WriteString(node.Update.String())
	}
}

// ForeignKeyConstraintTableDef represents a FOREIGN KEY constraint in the AST.
type ForeignKeyConstraintTableDef struct {
	Name     Name
	Table    NormalizableTableName
	FromCols NameList
	ToCols   NameList
	Actions  ReferenceActions
}

// Format implements the NodeFormatter interface.
func (node *ForeignKeyConstraintTableDef) Format(buf *bytes.Buffer, f FmtFlags) {
	if node.Name != "" {
		buf.WriteString("CONSTRAINT ")
		FormatNode(buf, f, node.Name)
		buf.WriteByte(' ')
	}
	buf.WriteString("FOREIGN KEY (")
	FormatNode(buf, f, node.FromCols)
	buf.WriteString(") REFERENCES ")
	FormatNode(buf, f, &node.Table)

	if len(node.ToCols) > 0 {
		buf.WriteByte(' ')
		buf.WriteByte('(')
		FormatNode(buf, f, node.ToCols)
		buf.WriteByte(')')
	}

	FormatNode(buf, f, node.Actions)
}

func (node *ForeignKeyConstraintTableDef) setName(name Name) {
	node.Name = name
}

func (*ForeignKeyConstraintTableDef) tableDef()           {}
func (*ForeignKeyConstraintTableDef) constraintTableDef() {}

func (*CheckConstraintTableDef) tableDef()           {}
func (*CheckConstraintTableDef) constraintTableDef() {}

// CheckConstraintTableDef represents a check constraint within a CREATE
// TABLE statement.
type CheckConstraintTableDef struct {
	Name Name
	Expr Expr
}

func (node *CheckConstraintTableDef) setName(name Name) {
	node.Name = name
}

// Format implements the NodeFormatter interface.
func (node *CheckConstraintTableDef) Format(buf *bytes.Buffer, f FmtFlags) {
	if node.Name != "" {
		buf.WriteString("CONSTRAINT ")
		FormatNode(buf, f, node.Name)
		buf.WriteByte(' ')
	}
	buf.WriteString("CHECK (")
	FormatNode(buf, f, node.Expr)
	buf.WriteByte(')')
}

// FamilyTableDef represents a family definition within a CREATE TABLE
// statement.
type FamilyTableDef struct {
	Name    Name
	Columns NameList
}

func (node *FamilyTableDef) setName(name Name) {
	node.Name = name
}

// Format implements the NodeFormatter interface.
func (node *FamilyTableDef) Format(buf *bytes.Buffer, f FmtFlags) {
	buf.WriteString("FAMILY ")
	if node.Name != "" {
		FormatNode(buf, f, node.Name)
		buf.WriteByte(' ')
	}
	buf.WriteByte('(')
	FormatNode(buf, f, node.Columns)
	buf.WriteByte(')')
}

// InterleaveDef represents an interleave definition within a CREATE TABLE
// or CREATE INDEX statement.
type InterleaveDef struct {
	Parent       *NormalizableTableName
	Fields       NameList
	DropBehavior DropBehavior
}

// Format implements the NodeFormatter interface.
func (node *InterleaveDef) Format(buf *bytes.Buffer, f FmtFlags) {
	buf.WriteString(" INTERLEAVE IN PARENT ")
	FormatNode(buf, f, node.Parent)
	buf.WriteString(" (")
	for i, field := range node.Fields {
		if i > 0 {
			buf.WriteString(", ")
		}
		FormatNode(buf, f, field)
	}
	buf.WriteString(")")
	if node.DropBehavior != DropDefault {
		buf.WriteString(" ")
		buf.WriteString(node.DropBehavior.String())
	}
}

// PartitionByType is an enum of each type of partitioning (LIST/RANGE).
type PartitionByType string

const (
	// PartitionByList indicates a PARTITION BY LIST clause.
	PartitionByList PartitionByType = "LIST"
	// PartitionByRange indicates a PARTITION BY LIST clause.
	PartitionByRange PartitionByType = "RANGE"
)

// PartitionBy represents an PARTITION BY definition within a CREATE/ALTER
// TABLE/INDEX statement.
type PartitionBy struct {
	Fields NameList
	// Exactly one of List or Range is required to be non-empty.
	List  []ListPartition
	Range []RangePartition
}

// Format implements the NodeFormatter interface.
func (node *PartitionBy) Format(buf *bytes.Buffer, f FmtFlags) {
	if len(node.List) > 0 {
		buf.WriteString(` PARTITION BY LIST (`)
	} else if len(node.Range) > 0 {
		buf.WriteString(` PARTITION BY RANGE (`)
	}
	FormatNode(buf, f, node.Fields)
	buf.WriteString(`) (`)
	for i, p := range node.List {
		if i > 0 {
			buf.WriteString(", ")
		}
		FormatNode(buf, f, p)
	}
	for i, p := range node.Range {
		if i > 0 {
			buf.WriteString(", ")
		}
		FormatNode(buf, f, p)
	}
	buf.WriteString(`)`)
}

// ListPartition represents a PARTITION definition within a PARTITION BY LIST.
type ListPartition struct {
	Name         UnrestrictedName
	Tuples       []*Tuple
	Subpartition *PartitionBy
}

// Format implements the NodeFormatter interface.
func (node ListPartition) Format(buf *bytes.Buffer, f FmtFlags) {
	buf.WriteString(`PARTITION `)
	FormatNode(buf, f, node.Name)
	buf.WriteString(` VALUES `)
	for i, n := range node.Tuples {
		if i > 0 {
			buf.WriteString(", ")
		}
		FormatNode(buf, f, n)
	}
	if node.Subpartition != nil {
		FormatNode(buf, f, node.Subpartition)
	}
}

// RangePartition represents a PARTITION definition within a PARTITION BY LIST.
type RangePartition struct {
	Name         UnrestrictedName
	Tuple        *Tuple
	Subpartition *PartitionBy
}

// Format implements the NodeFormatter interface.
func (node RangePartition) Format(buf *bytes.Buffer, f FmtFlags) {
	buf.WriteString(`PARTITION `)
	FormatNode(buf, f, node.Name)
	buf.WriteString(` VALUES LESS THAN `)
	FormatNode(buf, f, node.Tuple)
	if node.Subpartition != nil {
		FormatNode(buf, f, node.Subpartition)
	}
}

// PartitionDefault represents the DEFAULT expression in a PARTITION BY clause.
type PartitionDefault struct{}

// Format implements the NodeFormatter interface.
func (node PartitionDefault) Format(buf *bytes.Buffer, f FmtFlags) {
	buf.WriteString("DEFAULT")
}

// ResolvedType implements the TypedExpr interface.
func (PartitionDefault) ResolvedType() types.T { return TypeAny }

// PartitionMaxValue represents the MAXVALUE expression expression in a
// PARTITION BY clause.
type PartitionMaxValue struct{}

// Format implements the NodeFormatter interface.
func (node PartitionMaxValue) Format(buf *bytes.Buffer, f FmtFlags) {
	buf.WriteString("MAXVALUE")
}

// ResolvedType implements the TypedExpr interface.
func (PartitionMaxValue) ResolvedType() types.T { return TypeAny }

// CreateTable represents a CREATE TABLE statement.
type CreateTable struct {
	IfNotExists   bool
	Table         NormalizableTableName
	Interleave    *InterleaveDef
	PartitionBy   *PartitionBy
	Defs          TableDefs
	AsSource      *Select
	AsColumnNames NameList // Only to be used in conjunction with AsSource
}

// As returns true if this table represents a CREATE TABLE ... AS statement,
// false otherwise.
func (node *CreateTable) As() bool {
	return node.AsSource != nil
}

// Format implements the NodeFormatter interface.
func (node *CreateTable) Format(buf *bytes.Buffer, f FmtFlags) {
	buf.WriteString("CREATE TABLE ")
	if node.IfNotExists {
		buf.WriteString("IF NOT EXISTS ")
	}
	FormatNode(buf, f, &node.Table)
	if node.As() {
		if len(node.AsColumnNames) > 0 {
			buf.WriteString(" (")
			FormatNode(buf, f, node.AsColumnNames)
			buf.WriteByte(')')
		}
		buf.WriteString(" AS ")
		FormatNode(buf, f, node.AsSource)
	} else {
		buf.WriteString(" (")
		FormatNode(buf, f, node.Defs)
		buf.WriteByte(')')
		if node.Interleave != nil {
			FormatNode(buf, f, node.Interleave)
		}
		if node.PartitionBy != nil {
			FormatNode(buf, f, node.PartitionBy)
		}
	}
}

// CreateUser represents a CREATE USER statement.
type CreateUser struct {
	Name        Expr
	Password    Expr // nil if no password specified
	IfNotExists bool
}

// HasPassword returns if the CreateUser has a password.
func (node *CreateUser) HasPassword() bool {
	return node.Password != nil
}

// Format implements the NodeFormatter interface.
func (node *CreateUser) Format(buf *bytes.Buffer, f FmtFlags) {
	buf.WriteString("CREATE USER ")
	if node.IfNotExists {
		buf.WriteString("IF NOT EXISTS ")
	}
	FormatNode(buf, f, node.Name)
	if node.HasPassword() {
		buf.WriteString(" WITH PASSWORD ")
		if f.showPasswords {
			FormatNode(buf, f, node.Password)
		} else {
			buf.WriteString("*****")
		}
	}
}

// AlterUserSetPassword represents an ALTER USER ... WITH PASSWORD statement.
type AlterUserSetPassword struct {
	Name     Expr
	Password Expr
	IfExists bool
}

// Format implements the NodeFormatter interface.
func (node *AlterUserSetPassword) Format(buf *bytes.Buffer, f FmtFlags) {
	buf.WriteString("ALTER USER ")
	if node.IfExists {
		buf.WriteString("IF EXISTS ")
	}
	FormatNode(buf, f, node.Name)
	buf.WriteString(" WITH PASSWORD ")
	if f.showPasswords {
		FormatNode(buf, f, node.Password)
	} else {
		buf.WriteString("*****")
	}
}

// CreateView represents a CREATE VIEW statement.
type CreateView struct {
	Name        NormalizableTableName
	ColumnNames NameList
	AsSource    *Select
}

// Format implements the NodeFormatter interface.
func (node *CreateView) Format(buf *bytes.Buffer, f FmtFlags) {
	buf.WriteString("CREATE VIEW ")
	FormatNode(buf, f, &node.Name)

	if len(node.ColumnNames) > 0 {
		buf.WriteByte(' ')
		buf.WriteByte('(')
		FormatNode(buf, f, node.ColumnNames)
		buf.WriteByte(')')
	}

	buf.WriteString(" AS ")
	FormatNode(buf, f, node.AsSource)
}
