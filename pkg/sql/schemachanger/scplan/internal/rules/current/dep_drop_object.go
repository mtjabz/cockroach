// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package current

import (
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/rel"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scpb"
	. "github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scplan/internal/rules"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scplan/internal/scgraph"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/screl"
)

// These rules ensure that:
//   - a descriptor reaches ABSENT in a different transaction than it reaches
//     DROPPED (i.e. it cannot be removed until PostCommit).
//   - a descriptor element reaches the DROPPED state in the txn before
//     its dependent elements (namespace entry, comments, column names, etc)
//     reach the ABSENT state;
//   - or the WRITE_ONLY state for those dependent elements subject to the
//     2-version invariant.
func init() {

	registerDepRule(
		"descriptor dropped in transaction before removal",
		scgraph.PreviousStagePrecedence,
		"dropped", "absent",
		func(from, to NodeVars) rel.Clauses {
			return rel.Clauses{
				from.TypeFilter(IsDescriptor),
				from.El.AttrEqVar(screl.DescID, "_"),
				from.El.AttrEqVar(rel.Self, to.El),
				StatusesToAbsent(from, scpb.Status_DROPPED, to, scpb.Status_ABSENT),
			}
		})

	registerDepRule(
		"descriptor dropped before dependent element removal",
		scgraph.Precedence,
		"descriptor", "dependent",
		func(from, to NodeVars) rel.Clauses {
			return rel.Clauses{
				from.TypeFilter(IsDescriptor),
				to.TypeFilter(IsSimpleDependent),
				JoinOnDescID(from, to, "desc-id"),
				StatusesToAbsent(from, scpb.Status_DROPPED, to, scpb.Status_ABSENT),
			}
		})

	registerDepRule(
		"relation dropped before dependent column",
		scgraph.Precedence,
		"descriptor", "column",
		func(from, to NodeVars) rel.Clauses {
			return rel.Clauses{
				from.Type((*scpb.Table)(nil), (*scpb.View)(nil), (*scpb.Sequence)(nil)),
				to.TypeFilter(IsColumn),
				JoinOnDescID(from, to, "desc-id"),
				StatusesToAbsent(from, scpb.Status_DROPPED, to, scpb.Status_WRITE_ONLY),
			}
		})

	registerDepRule(
		"relation dropped before dependent index",
		scgraph.Precedence,
		"descriptor", "index",
		func(from, to NodeVars) rel.Clauses {
			return rel.Clauses{
				from.Type((*scpb.Table)(nil), (*scpb.View)(nil)),
				to.TypeFilter(IsIndex),
				JoinOnDescID(from, to, "desc-id"),
				StatusesToAbsent(from, scpb.Status_DROPPED, to, scpb.Status_VALIDATED),
			}
		},
	)

	registerDepRule(
		"relation dropped before dependent constraint",
		scgraph.Precedence,
		"descriptor", "constraint",
		func(from, to NodeVars) rel.Clauses {
			return rel.Clauses{
				from.Type((*scpb.Table)(nil)),
				to.TypeFilter(IsSupportedNonIndexBackedConstraint),
				JoinOnDescID(from, to, "desc-id"),
				StatusesToAbsent(from, scpb.Status_DROPPED, to, scpb.Status_WRITE_ONLY),
			}
		},
	)

}

// These rules ensure that cross-referencing simple dependent elements reach
// ABSENT in the same stage right after the referenced descriptor element
// reaches DROPPED.
//
// References from simple dependent elements to other descriptors exist as
// follows:
// - simple dependent elements with a ReferencedDescID attribute,
// - those which embed a TypeT,
// - those which embed an Expression.
func init() {

	registerDepRule(
		"descriptor drop right before removing dependent with attr ref",
		scgraph.SameStagePrecedence,
		"referenced-descriptor", "referencing-via-attr",
		func(from, to NodeVars) rel.Clauses {
			return rel.Clauses{
				from.TypeFilter(IsDescriptor),
				to.TypeFilter(IsSimpleDependent),
				JoinReferencedDescID(to, from, "desc-id"),
				StatusesToAbsent(from, scpb.Status_DROPPED, to, scpb.Status_ABSENT),
			}
		},
	)

	registerDepRule(
		"descriptor drop right before removing dependent with type ref",
		scgraph.SameStagePrecedence,
		"referenced-descriptor", "referencing-via-type",
		func(from, to NodeVars) rel.Clauses {
			fromDescID := rel.Var("fromDescID")
			return rel.Clauses{
				from.TypeFilter(IsTypeDescriptor),
				from.DescIDEq(fromDescID),
				to.ReferencedTypeDescIDsContain(fromDescID),
				to.TypeFilter(IsSimpleDependent, Or(IsWithTypeT, IsWithExpression)),
				StatusesToAbsent(from, scpb.Status_DROPPED, to, scpb.Status_ABSENT),
			}
		},
	)

	registerDepRule(
		"descriptor drop right before removing dependent with expr ref to sequence",
		scgraph.SameStagePrecedence,
		"referenced-descriptor", "referencing-via-expr",
		func(from, to NodeVars) rel.Clauses {
			seqID := rel.Var("seqID")
			return rel.Clauses{
				from.Type((*scpb.Sequence)(nil)),
				from.DescIDEq(seqID),
				to.ReferencedSequenceIDsContains(seqID),
				to.TypeFilter(IsSimpleDependent, IsWithExpression),
				StatusesToAbsent(from, scpb.Status_DROPPED, to, scpb.Status_ABSENT),
			}
		},
	)
}
