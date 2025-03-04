// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package scbuild

import (
	"sort"

	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/security/username"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/colinfo"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/schemaexpr"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/seqexpr"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/tabledesc"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/typedesc"
	"github.com/cockroachdb/cockroach/pkg/sql/parser"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/privilege"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scbuild/internal/scbuildstmt"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scdecomp"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scerrors"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scpb"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/screl"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/catconstants"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/catid"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlerrors"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/errors"
	"github.com/lib/pq/oid"
)

var _ scbuildstmt.BuilderState = (*builderState)(nil)

// QueryByID implements the scbuildstmt.BuilderState interface.
func (b *builderState) QueryByID(id catid.DescID) scbuildstmt.ElementResultSet {
	if id == catid.InvalidDescID {
		return nil
	}
	b.ensureDescriptor(id)
	return b.descCache[id].ers
}

// Ensure implements the scbuildstmt.BuilderState interface.
func (b *builderState) Ensure(e scpb.Element, target scpb.TargetStatus, meta scpb.TargetMetadata) {
	b.ensure(e, scpb.Status_UNKNOWN, scpb.InvalidTarget, target, meta)
}

// ensure is a helper function that ensures the presence of a target.
// The target may be newly defined via Ensure or may be re-used from
// a previous state and imported via newBuilderState.
func (b *builderState) ensure(
	e scpb.Element, current scpb.Status, previous, target scpb.TargetStatus, meta scpb.TargetMetadata,
) {
	if e == nil {
		panic(errors.AssertionFailedf("cannot define target for nil element"))
	}
	id := screl.GetDescID(e)
	b.ensureDescriptor(id)
	c := b.descCache[id]
	key := screl.ElementString(e)
	if i, ok := c.elementIndexMap[key]; ok {
		es := &b.output[i]
		if !screl.EqualElementKeys(es.element, e) {
			panic(errors.AssertionFailedf("element key %v does not match element: %s",
				key, screl.ElementString(es.element)))
		}
		if current != scpb.Status_UNKNOWN {
			es.current = current
		}
		if previous != scpb.InvalidTarget {
			es.previous = previous
		}
		es.target = target
		es.element = e
		es.metadata = meta
	} else {
		if current == scpb.Status_UNKNOWN {
			if target == scpb.ToAbsent {
				panic(errors.AssertionFailedf("element not found: %s", screl.ElementString(e)))
			}
			current = scpb.Status_ABSENT
		}
		c.ers.indexes = append(c.ers.indexes, len(b.output))
		c.elementIndexMap[key] = len(b.output)
		b.output = append(b.output, elementState{
			element:  e,
			previous: previous,
			target:   target,
			current:  current,
			metadata: meta,
		})
	}
}

// LogEventForExistingTarget implements the scbuildstmt.BuilderState interface.
func (b *builderState) LogEventForExistingTarget(e scpb.Element) {
	id := screl.GetDescID(e)
	key := screl.ElementString(e)

	c, ok := b.descCache[id]
	if !ok {
		panic(errors.AssertionFailedf(
			"elements for descriptor ID %d not found in builder state, %s expected", id, key))
	}
	i, ok := c.elementIndexMap[key]
	if !ok {
		panic(errors.AssertionFailedf("element %s expected in builder state but not found", key))
	}
	es := &b.output[i]
	if es.target == scpb.InvalidTarget {
		panic(errors.AssertionFailedf("no target set for element %s in builder state", key))
	}
	es.withLogEvent = true
}

// ForEachElementStatus implements the scpb.ElementStatusIterator interface.
func (b *builderState) ForEachElementStatus(
	fn func(current scpb.Status, target scpb.TargetStatus, e scpb.Element),
) {
	for _, es := range b.output {
		fn(es.current, es.target, es.element)
	}
}

var _ scbuildstmt.PrivilegeChecker = (*builderState)(nil)

// HasOwnership implements the scbuildstmt.PrivilegeChecker interface.
func (b *builderState) HasOwnership(e scpb.Element) bool {
	if b.hasAdmin {
		return true
	}
	id := screl.GetDescID(e)
	b.ensureDescriptor(id)
	return b.descCache[id].hasOwnership
}

func (b *builderState) mustOwn(id catid.DescID) {
	if b.hasAdmin {
		return
	}
	b.ensureDescriptor(id)
	if c := b.descCache[id]; !c.hasOwnership {
		panic(pgerror.Newf(pgcode.InsufficientPrivilege,
			"must be owner of %s %s", c.desc.DescriptorType(), c.desc.GetName()))
	}
}

// CheckPrivilege implements the scbuildstmt.PrivilegeChecker interface.
func (b *builderState) CheckPrivilege(e scpb.Element, privilege privilege.Kind) {
	b.checkPrivilege(screl.GetDescID(e), privilege)
}

func (b *builderState) checkPrivilege(id catid.DescID, priv privilege.Kind) {
	b.ensureDescriptor(id)
	c := b.descCache[id]
	if c.hasOwnership {
		return
	}
	err, found := c.privileges[priv]
	if !found {
		// Validate if this descriptor can be resolved under the current schema.
		if c.desc.DescriptorType() != catalog.Schema &&
			c.desc.DescriptorType() != catalog.Database {
			scpb.ForEachSchemaParent(c.ers, func(current scpb.Status, target scpb.TargetStatus, e *scpb.SchemaParent) {
				if current == scpb.Status_PUBLIC {
					b.checkPrivilege(e.SchemaID, privilege.USAGE)
				}
			})
		}
		err = b.auth.CheckPrivilege(b.ctx, c.desc, priv)
		c.privileges[priv] = err
	}
	if err != nil {
		panic(err)
	}
}

// CurrentUserHasAdminOrIsMemberOf implements the scbuildstmt.PrivilegeChecker interface.
func (b *builderState) CurrentUserHasAdminOrIsMemberOf(role username.SQLUsername) bool {
	if b.hasAdmin {
		return true
	}
	memberships, err := b.auth.MemberOfWithAdminOption(b.ctx, role)
	if err != nil {
		panic(err)
	}
	_, ok := memberships[b.evalCtx.SessionData().User()]
	return ok
}

var _ scbuildstmt.TableHelpers = (*builderState)(nil)

// NextTableColumnID implements the scbuildstmt.TableHelpers interface.
func (b *builderState) NextTableColumnID(table *scpb.Table) (ret catid.ColumnID) {
	{
		b.ensureDescriptor(table.TableID)
		desc := b.descCache[table.TableID].desc
		tbl, ok := desc.(catalog.TableDescriptor)
		if !ok {
			panic(errors.AssertionFailedf("Expected table descriptor for ID %d, instead got %s",
				desc.GetID(), desc.DescriptorType()))
		}
		ret = tbl.GetNextColumnID()
	}
	scpb.ForEachColumn(b, func(_ scpb.Status, _ scpb.TargetStatus, column *scpb.Column) {
		if column.IsSystemColumn {
			return
		}
		if column.TableID == table.TableID && column.ColumnID >= ret {
			ret = column.ColumnID + 1
		}
	})
	return ret
}

// NextColumnFamilyID implements the scbuildstmt.TableHelpers interface.
func (b *builderState) NextColumnFamilyID(table *scpb.Table) (ret catid.FamilyID) {
	{
		b.ensureDescriptor(table.TableID)
		desc := b.descCache[table.TableID].desc
		tbl, ok := desc.(catalog.TableDescriptor)
		if !ok {
			panic(errors.AssertionFailedf("Expected table descriptor for ID %d, instead got %s",
				desc.GetID(), desc.DescriptorType()))
		}
		ret = tbl.GetNextFamilyID()
	}
	scpb.ForEachColumnFamily(b, func(_ scpb.Status, _ scpb.TargetStatus, cf *scpb.ColumnFamily) {
		if cf.TableID == table.TableID && cf.FamilyID >= ret {
			ret = cf.FamilyID + 1
		}
	})
	return ret
}

// NextTableIndexID implements the scbuildstmt.TableHelpers interface.
func (b *builderState) NextTableIndexID(table *scpb.Table) (ret catid.IndexID) {
	return b.nextIndexID(table.TableID)
}

// NextViewIndexID implements the scbuildstmt.TableHelpers interface.
func (b *builderState) NextViewIndexID(view *scpb.View) (ret catid.IndexID) {
	if !view.IsMaterialized {
		panic(errors.AssertionFailedf("expected materialized view: %s", screl.ElementString(view)))
	}
	return b.nextIndexID(view.ViewID)
}

// NextTableConstraintID implements the scbuildstmt.TableHelpers interface.
func (b *builderState) NextTableConstraintID(id catid.DescID) (ret catid.ConstraintID) {
	{
		b.ensureDescriptor(id)
		desc := b.descCache[id].desc
		tbl, ok := desc.(catalog.TableDescriptor)
		if !ok {
			panic(errors.AssertionFailedf("Expected table descriptor for ID %d, instead got %s",
				desc.GetID(), desc.DescriptorType()))
		}
		ret = tbl.GetNextConstraintID()
		if ret == 0 {
			ret = 1
		}
	}

	b.QueryByID(id).ForEachElementStatus(func(_ scpb.Status, _ scpb.TargetStatus, e scpb.Element) {
		v, _ := screl.Schema.GetAttribute(screl.ConstraintID, e)
		if id, ok := v.(catid.ConstraintID); ok && id >= ret {
			ret = id + 1
		}
	})

	return ret
}

func (b *builderState) IsTableEmpty(table *scpb.Table) bool {
	// Scan the table for any rows, if they exist the lack of a default value
	// should lead to an error.
	elts := b.QueryByID(table.TableID)
	_, _, index := scpb.FindPrimaryIndex(elts)
	return b.tr.IsTableEmpty(b.ctx, table.TableID, index.IndexID)
}

func (b *builderState) nextIndexID(id catid.DescID) (ret catid.IndexID) {
	{
		b.ensureDescriptor(id)
		desc := b.descCache[id].desc
		tbl, ok := desc.(catalog.TableDescriptor)
		if !ok {
			panic(errors.AssertionFailedf("Expected table descriptor for ID %d, instead got %s",
				desc.GetID(), desc.DescriptorType()))
		}
		ret = tbl.GetNextIndexID()
	}
	scpb.ForEachPrimaryIndex(b, func(_ scpb.Status, _ scpb.TargetStatus, index *scpb.PrimaryIndex) {
		if index.TableID == id && index.IndexID >= ret {
			ret = index.IndexID + 1
		}
	})
	scpb.ForEachTemporaryIndex(b, func(_ scpb.Status, _ scpb.TargetStatus, index *scpb.TemporaryIndex) {
		if index.TableID == id && index.IndexID >= ret {
			ret = index.IndexID + 1
		}
	})
	scpb.ForEachSecondaryIndex(b, func(_ scpb.Status, _ scpb.TargetStatus, index *scpb.SecondaryIndex) {
		if index.TableID == id && index.IndexID >= ret {
			ret = index.IndexID + 1
		}
	})
	return ret
}

// IndexPartitioningDescriptor implements the scbuildstmt.TableHelpers
// interface.
func (b *builderState) IndexPartitioningDescriptor(
	indexName string, index *scpb.Index, columns []*scpb.IndexColumn, partBy *tree.PartitionBy,
) catpb.PartitioningDescriptor {
	b.ensureDescriptor(index.TableID)
	bd := b.descCache[index.TableID]
	desc := bd.desc
	tbl, ok := desc.(catalog.TableDescriptor)
	if !ok {
		panic(errors.AssertionFailedf("Expected table descriptor for ID %d, instead got %s",
			desc.GetID(), desc.DescriptorType()))
	}
	var oldNumImplicitColumns int

	scpb.ForEachIndexPartitioning(bd.ers, func(_ scpb.Status, _ scpb.TargetStatus, p *scpb.IndexPartitioning) {
		if p.TableID != index.TableID || p.IndexID != index.IndexID {
			return
		}
		oldNumImplicitColumns = int(p.PartitioningDescriptor.NumImplicitColumns)
	})

	keyColumns := make([]*scpb.IndexColumn, 0, len(columns))
	for _, column := range columns {
		if column.Kind != scpb.IndexColumn_KEY {
			continue
		}
		keyColumns = append(keyColumns, column)
	}
	sort.Slice(keyColumns, func(i, j int) bool {
		return keyColumns[i].OrdinalInKind < keyColumns[j].OrdinalInKind
	})
	oldKeyColumnNames := make([]string, len(keyColumns))
	for i, ic := range keyColumns {
		scpb.ForEachColumnName(b, func(_ scpb.Status, _ scpb.TargetStatus, cn *scpb.ColumnName) {
			if cn.TableID != index.TableID || cn.ColumnID != ic.ColumnID {
				return
			}
			oldKeyColumnNames[i] = cn.Name
		})
	}
	var allowedNewColumnNames []tree.Name
	scpb.ForEachColumnName(b, func(current scpb.Status, target scpb.TargetStatus, cn *scpb.ColumnName) {
		if cn.TableID != index.TableID && current != scpb.Status_PUBLIC && target == scpb.ToPublic {
			allowedNewColumnNames = append(allowedNewColumnNames, tree.Name(cn.Name))
		}
	})
	allowImplicitPartitioning := b.evalCtx.SessionData().ImplicitColumnPartitioningEnabled ||
		tbl.IsLocalityRegionalByRow()
	_, ret, err := b.createPartCCL(
		b.ctx,
		b.clusterSettings,
		b.evalCtx,
		func(name tree.Name) (catalog.Column, error) {
			return catalog.MustFindColumnByTreeName(tbl, name)
		},
		oldNumImplicitColumns,
		oldKeyColumnNames,
		partBy,
		allowedNewColumnNames,
		allowImplicitPartitioning, /* allowImplicitPartitioning */
	)
	if err != nil {
		panic(err)
	}
	// Validate the index partitioning descriptor.
	partitionNames := make(map[string]struct{})
	for _, rangePart := range ret.Range {
		if _, ok := partitionNames[rangePart.Name]; ok {
			panic(pgerror.Newf(pgcode.InvalidObjectDefinition, "PARTITION %s: name must be unique (used twice in index %q)",
				rangePart.Name, indexName))
		}
		partitionNames[rangePart.Name] = struct{}{}
	}
	for _, listPart := range ret.List {
		if _, ok := partitionNames[listPart.Name]; ok {
			panic(pgerror.Newf(pgcode.InvalidObjectDefinition, "PARTITION %s: name must be unique (used twice in index %q)",
				listPart.Name, indexName))
		}
		partitionNames[listPart.Name] = struct{}{}
	}
	return ret
}

// ResolveTypeRef implements the scbuildstmt.TableHelpers interface.
func (b *builderState) ResolveTypeRef(ref tree.ResolvableTypeReference) scpb.TypeT {
	toType, err := tree.ResolveType(b.ctx, ref, b.cr)
	if err != nil {
		panic(err)
	}
	return newTypeT(toType)
}

func newTypeT(t *types.T) scpb.TypeT {
	return scpb.TypeT{Type: t, ClosedTypeIDs: typedesc.GetTypeDescriptorClosure(t).Ordered()}
}

// WrapExpression implements the scbuildstmt.TableHelpers interface.
func (b *builderState) WrapExpression(tableID catid.DescID, expr tree.Expr) *scpb.Expression {
	// We will serialize and reparse the expression, so that type information
	// annotations are directly embedded inside, otherwise while parsing the
	// expression table record implicit types will not be correctly detected
	// by the TypeCollectorVisitor.
	expr, err := parser.ParseExpr(tree.Serialize(expr))
	if err != nil {
		panic(err)
	}
	if expr == nil {
		return nil
	}
	// Collect type IDs.
	var typeIDs catalog.DescriptorIDSet
	{
		visitor := &tree.TypeCollectorVisitor{OIDs: make(map[oid.Oid]struct{})}
		tree.WalkExpr(visitor, expr)
		for oid := range visitor.OIDs {
			if !types.IsOIDUserDefinedType(oid) {
				continue
			}
			id := typedesc.UserDefinedTypeOIDToID(oid)
			b.ensureDescriptor(id)
			desc := b.descCache[id].desc
			// Implicit record types will lead to table references, which will be
			// disallowed.
			if desc.DescriptorType() == catalog.Table {
				panic(pgerror.Newf(pgcode.DependentObjectsStillExist,
					"cannot modify table record type %q", desc.GetName()))
			}
			// Validate that no cross DB type references will exist here.
			// Determine the parent database ID, since cross database references are
			// disallowed.
			_, _, parentNamespace := scpb.FindNamespace(b.QueryByID(tableID))
			if desc.GetParentID() != parentNamespace.DatabaseID {
				typeName := tree.MakeTypeNameWithPrefix(b.descCache[id].prefix, desc.GetName())
				panic(pgerror.Newf(
					pgcode.FeatureNotSupported,
					"cross database type references are not supported: %s",
					typeName.String()))
			}
			typ, err := catalog.AsTypeDescriptor(desc)
			if err != nil {
				panic(err)
			}
			typ.GetIDClosure().ForEach(typeIDs.Add)
		}
	}
	// Collect sequence IDs.
	var seqIDs catalog.DescriptorIDSet
	{
		seqIdentifiers, err := seqexpr.GetUsedSequences(expr)
		if err != nil {
			panic(err)
		}
		seqNameToID := make(map[string]descpb.ID)
		for _, seqIdentifier := range seqIdentifiers {
			if seqIdentifier.IsByID() {
				seqIDs.Add(catid.DescID(seqIdentifier.SeqID))
				continue
			}
			uqName, err := parser.ParseTableName(seqIdentifier.SeqName)
			if err != nil {
				panic(err)
			}
			elts := b.ResolveSequence(uqName, scbuildstmt.ResolveParams{
				IsExistenceOptional: false,
				RequiredPrivilege:   privilege.SELECT,
			})
			_, _, seq := scpb.FindSequence(elts)
			seqNameToID[seqIdentifier.SeqName] = seq.SequenceID
			seqIDs.Add(seq.SequenceID)
		}
		if len(seqNameToID) > 0 {
			expr, err = seqexpr.ReplaceSequenceNamesWithIDs(expr, seqNameToID)
			if err != nil {
				panic(err)
			}
		}
	}
	ids, err := scbuildstmt.ExtractColumnIDsInExpr(b, tableID, expr)
	if err != nil {
		panic(err)
	}
	ret := &scpb.Expression{
		Expr:                catpb.Expression(tree.Serialize(expr)),
		UsesSequenceIDs:     seqIDs.Ordered(),
		UsesTypeIDs:         typeIDs.Ordered(),
		ReferencedColumnIDs: ids.Ordered(),
	}
	return ret
}

// ComputedColumnExpression implements the scbuildstmt.TableHelpers interface.
func (b *builderState) ComputedColumnExpression(tbl *scpb.Table, d *tree.ColumnTableDef) tree.Expr {
	_, _, ns := scpb.FindNamespace(b.QueryByID(tbl.TableID))
	tn := tree.MakeTableNameFromPrefix(b.NamePrefix(tbl), tree.Name(ns.Name))
	b.ensureDescriptor(tbl.TableID)
	// TODO(postamar): this doesn't work when referencing newly added columns.
	expr, _, err := schemaexpr.ValidateComputedColumnExpression(
		b.ctx,
		b.descCache[tbl.TableID].desc.(catalog.TableDescriptor),
		d,
		&tn,
		"computed column",
		b.semaCtx,
	)
	if err != nil {
		// This may be referencing newly added columns, so cheat and return
		// a not implemented error.
		panic(errors.Wrapf(errors.WithSecondaryError(scerrors.NotImplementedError(d), err),
			"computed column validation error"))
	}
	parsedExpr, err := parser.ParseExpr(expr)
	if err != nil {
		panic(err)
	}
	return parsedExpr
}

// PartialIndexPredicateExpression implements the scbuildstmt.TableHelpers interface.
func (b *builderState) PartialIndexPredicateExpression(
	tableID catid.DescID, expr tree.Expr,
) *scpb.Expression {
	// Ensure that an namespace entry exists for the table.
	_, _, ns := scpb.FindNamespace(b.QueryByID(tableID))
	if ns == nil {
		panic(errors.AssertionFailedf("unable to find namespace for %d.", tableID))
	}
	tn := tree.MakeTableNameFromPrefix(b.NamePrefix(ns), tree.Name(ns.Name))
	// Confirm that we have a table descriptor.
	if _, ok := b.descCache[tableID].desc.(catalog.TableDescriptor); !ok {
		panic(errors.AssertionFailedf("descriptor %d is not a table.", tableID))
	}
	// TODO(fqazi): this doesn't work when referencing newly added columns, this
	// is not problematic today since declarative schema changer is only enabled
	// for single statement transactions.
	_, err := schemaexpr.ValidatePartialIndexPredicate(
		b.ctx,
		b.descCache[tableID].desc.(catalog.TableDescriptor),
		expr,
		&tn,
		b.semaCtx)
	if err != nil {
		panic(err)
	}
	return b.WrapExpression(tableID, expr)
}

var _ scbuildstmt.ElementReferences = (*builderState)(nil)

// ForwardReferences implements the scbuildstmt.ElementReferences interface.
func (b *builderState) ForwardReferences(e scpb.Element) scbuildstmt.ElementResultSet {
	ids := screl.AllDescIDs(e)
	var c int
	ids.ForEach(func(id descpb.ID) {
		b.ensureDescriptor(id)
		c = c + len(b.descCache[id].ers.indexes)
	})
	ret := &elementResultSet{b: b, indexes: make([]int, 0, c)}
	ids.ForEach(func(id descpb.ID) {
		ret.indexes = append(ret.indexes, b.descCache[id].ers.indexes...)
	})
	return ret
}

// BackReferences implements the scbuildstmt.ElementReferences interface.
func (b *builderState) BackReferences(id catid.DescID) scbuildstmt.ElementResultSet {
	if id == catid.InvalidDescID {
		return nil
	}
	var ids catalog.DescriptorIDSet
	{
		b.ensureDescriptor(id)
		c := b.descCache[id]
		c.backrefs.ForEach(ids.Add)
		c.backrefs.ForEach(b.ensureDescriptor)
		for i := range b.output {
			es := &b.output[i]
			if es.current == scpb.Status_PUBLIC || es.target != scpb.ToPublic {
				continue
			}
			descID := screl.GetDescID(es.element)
			if ids.Contains(descID) || descID == id {
				continue
			}
			if !screl.ContainsDescID(es.element, id) {
				continue
			}
			ids.Add(descID)
		}
		ids.ForEach(b.ensureDescriptor)
	}
	ret := &elementResultSet{b: b}
	ids.ForEach(func(id descpb.ID) {
		ret.indexes = append(ret.indexes, b.descCache[id].ers.indexes...)
	})
	return ret
}

var _ scbuildstmt.NameResolver = (*builderState)(nil)

// NamePrefix implements the scbuildstmt.NameResolver interface.
func (b *builderState) NamePrefix(e scpb.Element) tree.ObjectNamePrefix {
	id := screl.GetDescID(e)
	b.ensureDescriptor(id)
	return b.descCache[id].prefix
}

// ResolveDatabase implements the scbuildstmt.NameResolver interface.
func (b *builderState) ResolveDatabase(
	name tree.Name, p scbuildstmt.ResolveParams,
) scbuildstmt.ElementResultSet {
	db := b.cr.MayResolveDatabase(b.ctx, name)
	if db == nil {
		if p.IsExistenceOptional {
			return nil
		}
		if string(name) == "" {
			panic(pgerror.New(pgcode.Syntax, "empty database name"))
		}
		panic(sqlerrors.NewUndefinedDatabaseError(name.String()))
	}
	b.ensureDescriptor(db.GetID())
	b.checkPrivilege(db.GetID(), p.RequiredPrivilege)
	return b.descCache[db.GetID()].ers
}

// ResolveSchema implements the scbuildstmt.NameResolver interface.
func (b *builderState) ResolveSchema(
	name tree.ObjectNamePrefix, p scbuildstmt.ResolveParams,
) scbuildstmt.ElementResultSet {
	_, sc := b.cr.MayResolveSchema(b.ctx, name)
	if sc == nil {
		if p.IsExistenceOptional {
			return nil
		}
		panic(sqlerrors.NewUndefinedSchemaError(name.Schema()))
	}
	switch sc.SchemaKind() {
	case catalog.SchemaPublic, catalog.SchemaVirtual, catalog.SchemaTemporary:
		panic(pgerror.Newf(pgcode.InsufficientPrivilege,
			"%s permission denied for schema %q", p.RequiredPrivilege.String(), name))
	case catalog.SchemaUserDefined:
		b.ensureDescriptor(sc.GetID())
		b.mustOwn(sc.GetID())
	default:
		panic(errors.AssertionFailedf("unknown schema kind %d", sc.SchemaKind()))
	}
	return b.descCache[sc.GetID()].ers
}

// ResolveUserDefinedTypeType implements the scbuildstmt.NameResolver interface.
func (b *builderState) ResolveUserDefinedTypeType(
	name *tree.UnresolvedObjectName, p scbuildstmt.ResolveParams,
) scbuildstmt.ElementResultSet {
	prefix, typ := b.cr.MayResolveType(b.ctx, *name)
	if typ == nil {
		if p.IsExistenceOptional {
			return nil
		}
		panic(sqlerrors.NewUndefinedTypeError(name))
	}
	switch typ.GetKind() {
	case descpb.TypeDescriptor_ALIAS:
		// The implicit array types are not directly modifiable.
		panic(pgerror.Newf(pgcode.DependentObjectsStillExist,
			"%q is an implicit array type and cannot be modified", typ.GetName()))
	case descpb.TypeDescriptor_MULTIREGION_ENUM:
		typeName := tree.MakeTypeNameWithPrefix(prefix.NamePrefix(), typ.GetName())
		// Multi-region enums are not directly modifiable.
		panic(errors.WithHintf(pgerror.Newf(pgcode.DependentObjectsStillExist,
			"%q is a multi-region enum and cannot be modified directly", typeName.FQString()),
			"try ALTER DATABASE %s DROP REGION %s", prefix.Database.GetName(), typ.GetName()))
	case descpb.TypeDescriptor_ENUM:
		b.ensureDescriptor(typ.GetID())
		b.mustOwn(typ.GetID())
	case descpb.TypeDescriptor_COMPOSITE:
		b.ensureDescriptor(typ.GetID())
		b.mustOwn(typ.GetID())
	case descpb.TypeDescriptor_TABLE_IMPLICIT_RECORD_TYPE:
		// Implicit record types are not directly modifiable.
		panic(pgerror.Newf(pgcode.DependentObjectsStillExist,
			"cannot modify table record type %q", typ.GetName()))
	default:
		panic(errors.AssertionFailedf("unknown type kind %s", typ.GetKind()))
	}
	return b.descCache[typ.GetID()].ers
}

// ResolveRelation implements the scbuildstmt.NameResolver interface.
func (b *builderState) ResolveRelation(
	name *tree.UnresolvedObjectName, p scbuildstmt.ResolveParams,
) scbuildstmt.ElementResultSet {
	c := b.resolveRelation(name, p)
	if c == nil {
		return nil
	}
	return c.ers
}

func (b *builderState) resolveRelation(
	name *tree.UnresolvedObjectName, p scbuildstmt.ResolveParams,
) *cachedDesc {
	prefix, rel := b.cr.MayResolveTable(b.ctx, *name)
	if rel == nil {
		if p.IsExistenceOptional {
			return nil
		}
		panic(sqlerrors.NewUndefinedRelationError(name))
	}
	if rel.IsVirtualTable() {
		if prefix.Schema.GetName() == catconstants.PgCatalogName {
			panic(pgerror.Newf(pgcode.InsufficientPrivilege,
				"%s is a system catalog", tree.ErrNameString(rel.GetName())))
		}
		panic(pgerror.Newf(pgcode.WrongObjectType,
			"%s is a virtual object and cannot be modified", tree.ErrNameString(rel.GetName())))
	}
	if rel.IsTemporary() {
		panic(scerrors.NotImplementedErrorf(nil /* n */, "dropping a temporary table"))
	}
	// If we own the schema then we can manipulate the underlying relation.
	b.ensureDescriptor(rel.GetID())
	c := b.descCache[rel.GetID()]
	b.ensureDescriptor(rel.GetParentSchemaID())
	if b.descCache[rel.GetParentSchemaID()].hasOwnership {
		c.hasOwnership = true
		return c
	}
	err, found := c.privileges[p.RequiredPrivilege]
	if !found {
		// Validate if this descriptor can be resolved under the current schema.
		b.checkPrivilege(rel.GetParentSchemaID(), privilege.USAGE)
		err = b.auth.CheckPrivilege(b.ctx, rel, p.RequiredPrivilege)
		c.privileges[p.RequiredPrivilege] = err
	}
	if err == nil {
		return c
	}
	if p.RequiredPrivilege != privilege.CREATE {
		panic(err)
	}
	relationType := "table"
	if rel.IsView() {
		relationType = "view"
	} else if rel.IsSequence() {
		relationType = "sequence"
	}
	panic(pgerror.Newf(pgcode.InsufficientPrivilege,
		"must be owner of %s %s or have %s privilege on %s %s",
		relationType,
		tree.Name(rel.GetName()),
		p.RequiredPrivilege,
		relationType,
		tree.Name(rel.GetName())))
}

// ResolveTable implements the scbuildstmt.NameResolver interface.
func (b *builderState) ResolveTable(
	name *tree.UnresolvedObjectName, p scbuildstmt.ResolveParams,
) scbuildstmt.ElementResultSet {
	c := b.resolveRelation(name, p)
	if c == nil {
		return nil
	}
	if rel := c.desc.(catalog.TableDescriptor); !rel.IsTable() {
		panic(pgerror.Newf(pgcode.WrongObjectType, "%q is not a table", rel.GetName()))
	}
	return c.ers
}

// ResolveSequence implements the scbuildstmt.NameResolver interface.
func (b *builderState) ResolveSequence(
	name *tree.UnresolvedObjectName, p scbuildstmt.ResolveParams,
) scbuildstmt.ElementResultSet {
	c := b.resolveRelation(name, p)
	if c == nil {
		return nil
	}
	if rel := c.desc.(catalog.TableDescriptor); !rel.IsSequence() {
		panic(pgerror.Newf(pgcode.WrongObjectType, "%q is not a sequence", rel.GetName()))
	}
	return c.ers
}

// ResolveView implements the scbuildstmt.NameResolver interface.
func (b *builderState) ResolveView(
	name *tree.UnresolvedObjectName, p scbuildstmt.ResolveParams,
) scbuildstmt.ElementResultSet {
	c := b.resolveRelation(name, p)
	if c == nil {
		return nil
	}
	if rel := c.desc.(catalog.TableDescriptor); !rel.IsView() {
		panic(pgerror.Newf(pgcode.WrongObjectType, "%q is not a view", rel.GetName()))
	}
	return c.ers
}

// ResolveIndex implements the scbuildstmt.NameResolver interface.
func (b *builderState) ResolveIndex(
	relationID catid.DescID, indexName tree.Name, p scbuildstmt.ResolveParams,
) scbuildstmt.ElementResultSet {
	b.ensureDescriptor(relationID)
	c := b.descCache[relationID]
	rel := c.desc.(catalog.TableDescriptor)
	if !rel.IsPhysicalTable() || rel.IsSequence() {
		panic(pgerror.Newf(pgcode.WrongObjectType,
			"%q is not an indexable table or a materialized view", rel.GetName()))
	}
	b.checkPrivilege(rel.GetID(), p.RequiredPrivilege)
	var indexID catid.IndexID
	scpb.ForEachIndexName(c.ers, func(_ scpb.Status, _ scpb.TargetStatus, e *scpb.IndexName) {
		if e.TableID == relationID && tree.Name(e.Name) == indexName {
			indexID = e.IndexID
		}
	})
	if indexID == 0 && (indexName == "" || indexName == tabledesc.LegacyPrimaryKeyIndexName) {
		indexID = rel.GetPrimaryIndexID()
	}
	if indexID == 0 {
		if p.IsExistenceOptional {
			return nil
		}
		panic(pgerror.Newf(pgcode.UndefinedObject,
			"index %q not found in relation %q", indexName, rel.GetName()))
	}
	return c.ers.Filter(func(_ scpb.Status, _ scpb.TargetStatus, element scpb.Element) bool {
		idI, _ := screl.Schema.GetAttribute(screl.IndexID, element)
		return idI != nil && idI.(catid.IndexID) == indexID
	})
}

// ResolveIndexByName implements the scbuildstmt.NameResolver interface.
func (b *builderState) ResolveIndexByName(
	tableIndexName *tree.TableIndexName, p scbuildstmt.ResolveParams,
) scbuildstmt.ElementResultSet {
	// If a table name is specified, confirm its not a systme table first.
	if tableIndexName.Table.ObjectName != "" {
		desc := b.resolveRelation(tableIndexName.Table.ToUnresolvedObjectName(), p)
		if desc == nil {
			return nil
		}
		tableDesc := desc.desc.(catalog.TableDescriptor)
		b.ensureDescriptor(tableDesc.GetParentSchemaID())
		if catalog.IsSystemDescriptor(tableDesc) {
			schemaDesc := b.descCache[tableDesc.GetParentSchemaID()].desc
			panic(catalog.NewMutableAccessToVirtualSchemaError(schemaDesc.(catalog.SchemaDescriptor)))
		}
	}

	found, prefix, tbl, idx := b.cr.MayResolveIndex(b.ctx, *tableIndexName)
	if !found {
		if !p.IsExistenceOptional {
			panic(pgerror.Newf(pgcode.UndefinedObject, "index %q does not exist", tableIndexName.Index))
		}
		return nil
	}
	tableIndexName.Table.CatalogName = tree.Name(prefix.Database.GetName())
	tableIndexName.Table.SchemaName = tree.Name(prefix.Schema.GetName())
	tableIndexName.Table.ObjectName = tree.Name(tbl.GetName())
	return b.ResolveIndex(tbl.GetID(), tree.Name(idx.GetName()), p)
}

// ResolveColumn implements the scbuildstmt.NameResolver interface.
func (b *builderState) ResolveColumn(
	relationID catid.DescID, columnName tree.Name, p scbuildstmt.ResolveParams,
) scbuildstmt.ElementResultSet {
	b.ensureDescriptor(relationID)
	c := b.descCache[relationID]
	rel := c.desc.(catalog.TableDescriptor)
	b.checkPrivilege(rel.GetID(), p.RequiredPrivilege)
	var columnID catid.ColumnID
	scpb.ForEachColumnName(c.ers, func(status scpb.Status, _ scpb.TargetStatus, e *scpb.ColumnName) {
		if e.TableID == relationID && tree.Name(e.Name) == columnName {
			columnID = e.ColumnID
		}
	})
	if columnID == 0 {
		if p.IsExistenceOptional {
			return nil
		}
		panic(colinfo.NewUndefinedColumnError(string(columnName)))
	}
	return c.ers.Filter(func(_ scpb.Status, _ scpb.TargetStatus, e scpb.Element) bool {
		idI, _ := screl.Schema.GetAttribute(screl.ColumnID, e)
		return idI != nil && idI.(catid.ColumnID) == columnID
	})
}

// ResolveConstraint implements the scbuildstmt.NameResolver interface.
func (b *builderState) ResolveConstraint(
	relationID catid.DescID, constraintName tree.Name, p scbuildstmt.ResolveParams,
) scbuildstmt.ElementResultSet {
	b.ensureDescriptor(relationID)
	c := b.descCache[relationID]
	rel := c.desc.(catalog.TableDescriptor)
	var constraintID catid.ConstraintID
	scpb.ForEachConstraintWithoutIndexName(c.ers, func(status scpb.Status, _ scpb.TargetStatus, e *scpb.ConstraintWithoutIndexName) {
		if e.TableID == relationID && tree.Name(e.Name) == constraintName {
			constraintID = e.ConstraintID
		}
	})

	if constraintID == 0 {
		var indexID catid.IndexID
		scpb.ForEachIndexName(c.ers, func(_ scpb.Status, _ scpb.TargetStatus, e *scpb.IndexName) {
			if e.TableID == relationID && tree.Name(e.Name) == constraintName {
				indexID = e.IndexID
			}
		})
		if indexID != 0 {
			scpb.ForEachPrimaryIndex(c.ers, func(_ scpb.Status, _ scpb.TargetStatus, e *scpb.PrimaryIndex) {
				if e.TableID == relationID && e.IndexID == indexID {
					constraintID = e.ConstraintID
				}
			})
			scpb.ForEachSecondaryIndex(c.ers, func(_ scpb.Status, _ scpb.TargetStatus, e *scpb.SecondaryIndex) {
				if e.TableID == relationID && e.IndexID == indexID && e.IsUnique {
					constraintID = e.ConstraintID
				}
			})
		}
	}

	if constraintID == 0 {
		if p.IsExistenceOptional {
			return nil
		}
		panic(pgerror.Newf(pgcode.UndefinedObject,
			"constraint %q of relation %q does not exist", constraintName, rel.GetName()))
	}

	return c.ers.Filter(func(_ scpb.Status, _ scpb.TargetStatus, e scpb.Element) bool {
		idI, _ := screl.Schema.GetAttribute(screl.ConstraintID, e)
		return idI != nil && idI.(catid.ConstraintID) == constraintID
	})
}

func (b *builderState) ensureDescriptor(id catid.DescID) {
	if _, found := b.descCache[id]; found {
		return
	}
	c := &cachedDesc{
		desc:            b.readDescriptor(id),
		privileges:      make(map[privilege.Kind]error),
		hasOwnership:    b.hasAdmin,
		ers:             &elementResultSet{b: b},
		elementIndexMap: map[string]int{},
	}
	// Collect privileges
	if !c.hasOwnership {
		var err error
		c.hasOwnership, err = b.auth.HasOwnership(b.ctx, c.desc)
		if err != nil {
			panic(err)
		}
	}
	// Collect backrefs and elements.
	b.descCache[id] = c
	crossRefLookupFn := func(id catid.DescID) catalog.Descriptor {
		return b.readDescriptor(id)
	}
	visitorFn := func(status scpb.Status, e scpb.Element) {
		c.ers.indexes = append(c.ers.indexes, len(b.output))
		key := screl.ElementString(e)
		c.elementIndexMap[key] = len(b.output)
		b.output = append(b.output, elementState{
			element: e,
			target:  scpb.AsTargetStatus(status),
			current: status,
		})
	}

	c.backrefs = scdecomp.WalkDescriptor(b.ctx, c.desc, crossRefLookupFn, visitorFn, b.commentGetter, b.zoneConfigReader)
	// Name prefix and namespace lookups.
	switch d := c.desc.(type) {
	case catalog.DatabaseDescriptor:
		if !d.HasPublicSchemaWithDescriptor() {
			panic(scerrors.NotImplementedErrorf(nil, /* n */
				"database %q (%d) with a descriptorless public schema",
				d.GetName(), d.GetID()))
		}
		// Handle special case of database children, which may include temporary
		// schemas, which aren't explicitly referenced in the database's schemas
		// map.
		childSchemas := b.cr.GetAllSchemasInDatabase(b.ctx, d)
		_ = childSchemas.ForEachDescriptor(func(desc catalog.Descriptor) error {
			sc, err := catalog.AsSchemaDescriptor(desc)
			if err != nil {
				panic(err)
			}
			switch sc.SchemaKind() {
			case catalog.SchemaVirtual:
				return nil
			case catalog.SchemaTemporary:
				b.tempSchemas[sc.GetID()] = sc
			}
			c.backrefs.Add(sc.GetID())
			return nil
		})
	case catalog.SchemaDescriptor:
		b.ensureDescriptor(c.desc.GetParentID())
		db := b.descCache[c.desc.GetParentID()].desc
		c.prefix.CatalogName = tree.Name(db.GetName())
		c.prefix.ExplicitCatalog = true
		// Handle special case of schema children, which have to be added to
		// the back-referenced ID set but which aren't explicitly referenced in
		// the schema descriptor itself.
		objects := b.cr.GetAllObjectsInSchema(b.ctx, db.(catalog.DatabaseDescriptor), d)
		_ = objects.ForEachDescriptor(func(desc catalog.Descriptor) error {
			c.backrefs.Add(desc.GetID())
			return nil
		})
	default:
		b.ensureDescriptor(c.desc.GetParentID())
		db := b.descCache[c.desc.GetParentID()].desc
		c.prefix.CatalogName = tree.Name(db.GetName())
		c.prefix.ExplicitCatalog = true
		b.ensureDescriptor(c.desc.GetParentSchemaID())
		sc := b.descCache[c.desc.GetParentSchemaID()].desc
		c.prefix.SchemaName = tree.Name(sc.GetName())
		c.prefix.ExplicitSchema = true
	}
}

func (b *builderState) readDescriptor(id catid.DescID) catalog.Descriptor {
	if id == catid.InvalidDescID {
		panic(errors.AssertionFailedf("invalid descriptor ID %d", id))
	}
	if id == keys.SystemPublicSchemaID || id == keys.PublicSchemaIDForBackup || id == keys.PublicSchemaID {
		panic(scerrors.NotImplementedErrorf(nil /* n */, "descriptorless public schema %d", id))
	}
	if tempSchema := b.tempSchemas[id]; tempSchema != nil {
		return tempSchema
	}
	return b.cr.MustReadDescriptor(b.ctx, id)
}

type elementResultSet struct {
	b       *builderState
	indexes []int
}

var _ scbuildstmt.ElementResultSet = &elementResultSet{}

// ForEachElementStatus implements the scpb.ElementStatusIterator interface.
func (ers *elementResultSet) ForEachElementStatus(
	fn func(current scpb.Status, target scpb.TargetStatus, element scpb.Element),
) {
	if ers.IsEmpty() {
		return
	}
	for _, i := range ers.indexes {
		es := &ers.b.output[i]
		fn(es.current, es.target, es.element)
	}
}

// IsEmpty implements the scbuildstmt.ElementResultSet interface.
func (ers *elementResultSet) IsEmpty() bool {
	return ers == nil || len(ers.indexes) == 0
}

// Filter implements the scbuildstmt.ElementResultSet interface.
func (ers *elementResultSet) Filter(
	predicate func(current scpb.Status, target scpb.TargetStatus, e scpb.Element) bool,
) scbuildstmt.ElementResultSet {
	return ers.filter(predicate)
}

func (ers *elementResultSet) filter(
	predicate func(current scpb.Status, target scpb.TargetStatus, e scpb.Element) bool,
) *elementResultSet {
	if ers.IsEmpty() {
		return nil
	}
	ret := elementResultSet{b: ers.b}
	for _, i := range ers.indexes {
		es := &ers.b.output[i]
		if predicate(es.current, es.target, es.element) {
			ret.indexes = append(ret.indexes, i)
		}
	}
	if len(ret.indexes) == 0 {
		return nil
	}
	return &ret
}
