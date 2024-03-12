package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	sq "github.com/Masterminds/squirrel"

	"github.com/authzed/spicedb/internal/datastore/common"
	"github.com/authzed/spicedb/internal/datastore/revisions"
	"github.com/authzed/spicedb/pkg/datastore"
	"github.com/authzed/spicedb/pkg/datastore/options"
	core "github.com/authzed/spicedb/pkg/proto/core/v1"
)

type txCleanupFunc func() error

type txFactory func(context.Context) (*sql.Tx, txCleanupFunc, error)

type mysqlReader struct {
	*QueryBuilder

	txSource txFactory
	executor common.QueryExecutor
	filterer queryFilterer
}

type queryFilterer func(original sq.SelectBuilder) sq.SelectBuilder

const (
	errUnableToReadConfig     = "unable to read namespace config: %w"
	errUnableToListNamespaces = "unable to list namespaces: %w"
	errUnableToQueryTuples    = "unable to query tuples: %w"
)

var schema = common.NewSchemaInformation(
	colNamespace,
	colObjectID,
	colRelation,
	colUsersetNamespace,
	colUsersetObjectID,
	colUsersetRelation,
	colCaveatName,
	common.ExpandedLogicComparison,
)

func (mr *mysqlReader) QueryRelationships(
	ctx context.Context,
	filter datastore.RelationshipsFilter,
	opts ...options.QueryOptionsOption,
) (iter datastore.RelationshipIterator, err error) {
	qBuilder, err := common.NewSchemaQueryFilterer(schema, mr.filterer(mr.QueryTuplesQuery)).FilterWithRelationshipsFilter(filter)
	if err != nil {
		return nil, err
	}

	return mr.executor.ExecuteQuery(ctx, qBuilder, opts...)
}

func (mr *mysqlReader) ReverseQueryRelationships(
	ctx context.Context,
	subjectsFilter datastore.SubjectsFilter,
	opts ...options.ReverseQueryOptionsOption,
) (iter datastore.RelationshipIterator, err error) {
	qBuilder, err := common.NewSchemaQueryFilterer(schema, mr.filterer(mr.QueryTuplesQuery)).
		FilterWithSubjectsSelectors(subjectsFilter.AsSelector())
	if err != nil {
		return nil, err
	}

	queryOpts := options.NewReverseQueryOptionsWithOptions(opts...)

	if queryOpts.ResRelation != nil {
		qBuilder = qBuilder.
			FilterToResourceType(queryOpts.ResRelation.Namespace).
			FilterToRelation(queryOpts.ResRelation.Relation)
	}

	return mr.executor.ExecuteQuery(
		ctx,
		qBuilder,
		options.WithLimit(queryOpts.LimitForReverse),
		options.WithAfter(queryOpts.AfterForReverse),
		options.WithSort(queryOpts.SortForReverse),
	)
}

func (mr *mysqlReader) ReadNamespaceByName(ctx context.Context, nsName string) (*core.NamespaceDefinition, datastore.Revision, error) {
	tx, txCleanup, err := mr.txSource(ctx)
	if err != nil {
		return nil, datastore.NoRevision, fmt.Errorf(errUnableToReadConfig, err)
	}
	defer common.LogOnError(ctx, txCleanup)

	loaded, version, err := loadNamespace(ctx, nsName, tx, mr.filterer(mr.ReadNamespaceQuery))
	switch {
	case errors.As(err, &datastore.ErrNamespaceNotFound{}):
		return nil, datastore.NoRevision, err
	case err == nil:
		return loaded, version, nil
	default:
		return nil, datastore.NoRevision, fmt.Errorf(errUnableToReadConfig, err)
	}
}

func loadNamespace(ctx context.Context, namespace string, tx *sql.Tx, baseQuery sq.SelectBuilder) (*core.NamespaceDefinition, datastore.Revision, error) {
	ctx, span := tracer.Start(ctx, fmt.Sprintf("loadNamespace %v", namespace))
	defer span.End()

	query, args, err := baseQuery.Where(sq.Eq{colNamespace: namespace}).ToSql()
	if err != nil {
		return nil, datastore.NoRevision, err
	}

	var config []byte
	var txID uint64
	err = tx.QueryRowContext(ctx, query, args...).Scan(&config, &txID)
	span.AddEvent("sql: " + common.InlineSqlArgs(query, args))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = datastore.NewNamespaceNotFoundErr(namespace)
		}
		return nil, datastore.NoRevision, err
	}

	loaded := &core.NamespaceDefinition{}
	if err := loaded.UnmarshalVT(config); err != nil {
		return nil, datastore.NoRevision, err
	}

	return loaded, revisions.NewForTransactionID(txID), nil
}

func (mr *mysqlReader) ListAllNamespaces(ctx context.Context) ([]datastore.RevisionedNamespace, error) {
	tx, txCleanup, err := mr.txSource(ctx)
	if err != nil {
		return nil, err
	}
	defer common.LogOnError(ctx, txCleanup)

	query := mr.filterer(mr.ReadNamespaceQuery)

	nsDefs, err := loadAllNamespaces(ctx, tx, query)
	if err != nil {
		return nil, fmt.Errorf(errUnableToListNamespaces, err)
	}

	return nsDefs, err
}

func (mr *mysqlReader) LookupNamespacesWithNames(ctx context.Context, nsNames []string) ([]datastore.RevisionedNamespace, error) {
	if len(nsNames) == 0 {
		return nil, nil
	}

	tx, txCleanup, err := mr.txSource(ctx)
	if err != nil {
		return nil, err
	}
	defer common.LogOnError(ctx, txCleanup)

	clause := sq.Or{}
	for _, nsName := range nsNames {
		clause = append(clause, sq.Eq{colNamespace: nsName})
	}

	query := mr.filterer(mr.ReadNamespaceQuery.Where(clause))

	nsDefs, err := loadAllNamespaces(ctx, tx, query)
	if err != nil {
		return nil, fmt.Errorf(errUnableToListNamespaces, err)
	}

	return nsDefs, err
}

func loadAllNamespaces(ctx context.Context, tx *sql.Tx, queryBuilder sq.SelectBuilder) ([]datastore.RevisionedNamespace, error) {
	ctx, span := tracer.Start(ctx, "loadAllNamespaces")
	defer span.End()
	query, args, err := queryBuilder.ToSql()
	if err != nil {
		return nil, err
	}

	var nsDefs []datastore.RevisionedNamespace

	rows, err := tx.QueryContext(ctx, query, args...)
	span.AddEvent("sql: " + common.InlineSqlArgs(query, args))
	if err != nil {
		return nil, err
	}
	defer common.LogOnError(ctx, rows.Close)

	for rows.Next() {
		var config []byte
		var txID uint64
		if err := rows.Scan(&config, &txID); err != nil {
			return nil, err
		}

		loaded := &core.NamespaceDefinition{}
		if err := loaded.UnmarshalVT(config); err != nil {
			return nil, fmt.Errorf(errUnableToReadConfig, err)
		}

		nsDefs = append(nsDefs, datastore.RevisionedNamespace{
			Definition:          loaded,
			LastWrittenRevision: revisions.NewForTransactionID(txID),
		})
	}
	if rows.Err() != nil {
		return nil, rows.Err()
	}

	return nsDefs, nil
}

var _ datastore.Reader = &mysqlReader{}
