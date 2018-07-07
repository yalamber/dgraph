/*
 * Copyright 2017-2018 Dgraph Labs, Inc.
 *
 * This file is available under the Apache License, Version 2.0,
 * with the Commons Clause restriction.
 */

package query

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
	geom "github.com/twpayne/go-geom"
	"github.com/twpayne/go-geom/encoding/geojson"

	"github.com/dgraph-io/dgo/protos/api"
	"github.com/dgraph-io/dgraph/gql"
	"github.com/dgraph-io/dgraph/posting"
	"github.com/dgraph-io/dgraph/protos/intern"
	"github.com/dgraph-io/dgraph/schema"
	"github.com/dgraph-io/dgraph/types"
	"github.com/dgraph-io/dgraph/types/facets"
	"github.com/dgraph-io/dgraph/worker"
	"github.com/dgraph-io/dgraph/x"
)

func childAttrs(sg *SubGraph) []string {
	var out []string
	for _, c := range sg.Children {
		out = append(out, c.Attr)
	}
	return out
}

func taskValues(t *testing.T, v []*intern.ValueList) []string {
	out := make([]string, len(v))
	for i, tv := range v {
		out[i] = string(tv.Values[0].Val)
	}
	return out
}

var index uint64

func addEdge(t *testing.T, attr string, src uint64, edge *intern.DirectedEdge) {
	// Mutations don't go through normal flow, so default schema for predicate won't be present.
	// Lets add it.
	if _, ok := schema.State().Get(attr); !ok {
		schema.State().Set(attr, intern.SchemaUpdate{
			Predicate: attr,
			ValueType: edge.ValueType,
		})
	}
	l, err := posting.Get(x.DataKey(attr, src))
	require.NoError(t, err)
	startTs := timestamp()
	txn := posting.Oracle().RegisterStartTs(startTs)
	require.NoError(t,
		l.AddMutationWithIndex(context.Background(), edge, txn))

	commit := timestamp()
	require.NoError(t, txn.CommitMutations(context.Background(), commit))
	delta := &intern.OracleDelta{}
	delta.Commits = make(map[uint64]uint64)
	delta.Commits[startTs] = commit
	delta.MaxAssigned = commit
	posting.Oracle().ProcessDelta(delta)
}

func makeFacets(facetKVs map[string]string) (fs []*api.Facet, err error) {
	if len(facetKVs) == 0 {
		return nil, nil
	}
	allKeys := make([]string, 0, len(facetKVs))
	for k := range facetKVs {
		allKeys = append(allKeys, k)
	}
	sort.Strings(allKeys)

	for _, k := range allKeys {
		f, err := facets.FacetFor(k, facetKVs[k])
		if err != nil {
			return nil, err
		}
		fs = append(fs, f)
	}
	return fs, nil
}

func addPredicateEdge(t *testing.T, attr string, src uint64) {
	if worker.Config.ExpandEdge {
		edge := &intern.DirectedEdge{
			Value: []byte(attr),
			Attr:  "_predicate_",
			Op:    intern.DirectedEdge_SET,
		}
		addEdge(t, "_predicate_", src, edge)
	}
}

func addEdgeToValue(t *testing.T, attr string, src uint64,
	value string, facetKVs map[string]string) {
	addEdgeToLangValue(t, attr, src, value, "", facetKVs)
	addPredicateEdge(t, attr, src)
}

func addEdgeToLangValue(t *testing.T, attr string, src uint64,
	value, lang string, facetKVs map[string]string) {
	fs, err := makeFacets(facetKVs)
	require.NoError(t, err)
	edge := &intern.DirectedEdge{
		Value:  []byte(value),
		Lang:   lang,
		Label:  "testing",
		Attr:   attr,
		Entity: src,
		Op:     intern.DirectedEdge_SET,
		Facets: fs,
	}
	addEdge(t, attr, src, edge)
	addPredicateEdge(t, attr, src)
}

func addEdgeToTypedValue(t *testing.T, attr string, src uint64,
	typ types.TypeID, value []byte, facetKVs map[string]string) {
	fs, err := makeFacets(facetKVs)
	require.NoError(t, err)
	edge := &intern.DirectedEdge{
		Value:     value,
		ValueType: intern.Posting_ValType(typ),
		Label:     "testing",
		Attr:      attr,
		Entity:    src,
		Op:        intern.DirectedEdge_SET,
		Facets:    fs,
	}
	addEdge(t, attr, src, edge)
	addPredicateEdge(t, attr, src)
}

func addEdgeToUID(t *testing.T, attr string, src uint64,
	dst uint64, facetKVs map[string]string) {
	fs, err := makeFacets(facetKVs)
	require.NoError(t, err)
	edge := &intern.DirectedEdge{
		ValueId: dst,
		// This is used to set uid schema type for pred for the purpose of tests. Actual mutation
		// won't set ValueType to types.UidID.
		ValueType: intern.Posting_ValType(types.UidID),
		Label:     "testing",
		Attr:      attr,
		Entity:    src,
		Op:        intern.DirectedEdge_SET,
		Facets:    fs,
	}
	addEdge(t, attr, src, edge)
	addPredicateEdge(t, attr, src)
}

func delEdgeToUID(t *testing.T, attr string, src uint64, dst uint64) {
	edge := &intern.DirectedEdge{
		ValueType: intern.Posting_ValType(types.UidID),
		ValueId:   dst,
		Label:     "testing",
		Attr:      attr,
		Entity:    src,
		Op:        intern.DirectedEdge_DEL,
	}
	addEdge(t, attr, src, edge)
}

func delEdgeToLangValue(t *testing.T, attr string, src uint64, value, lang string) {
	edge := &intern.DirectedEdge{
		Value:  []byte(value),
		Lang:   lang,
		Label:  "testing",
		Attr:   attr,
		Entity: src,
		Op:     intern.DirectedEdge_DEL,
	}
	addEdge(t, attr, src, edge)
}

func addGeoData(t *testing.T, uid uint64, p geom.T, name string) {
	value := types.ValueForType(types.BinaryID)
	src := types.ValueForType(types.GeoID)
	src.Value = p
	err := types.Marshal(src, &value)
	require.NoError(t, err)
	addEdgeToTypedValue(t, "geometry", uid, types.GeoID, value.Value.([]byte), nil)
	addEdgeToTypedValue(t, "name", uid, types.StringID, []byte(name), nil)
}

func defaultContext() context.Context {
	return context.Background()
}

func processToFastJson(t *testing.T, query string) (string, error) {
	return processToFastJsonCtxVars(t, query, defaultContext(), nil)
}

func processToFastJsonCtxVars(t *testing.T, query string, ctx context.Context,
	vars map[string]string) (string, error) {
	res, err := gql.Parse(gql.Request{Str: query, Variables: vars})
	if err != nil {
		return "", err
	}

	startTs := timestamp()
	maxPendingCh <- startTs
	queryRequest := QueryRequest{Latency: &Latency{}, GqlQuery: &res, ReadTs: startTs}
	err = queryRequest.ProcessQuery(ctx)
	if err != nil {
		return "", err
	}

	out, err := ToJson(queryRequest.Latency, queryRequest.Subgraphs)
	if err != nil {
		return "", err
	}
	response := map[string]interface{}{}
	response["data"] = json.RawMessage(string(out))
	resp, err := json.Marshal(response)
	require.NoError(t, err)
	return string(resp), err
}

func processToFastJsonNoErr(t *testing.T, query string) string {
	res, err := processToFastJson(t, query)
	require.NoError(t, err)
	return res
}

func processSchemaQuery(t *testing.T, q string) []*api.SchemaNode {
	res, err := gql.Parse(gql.Request{Str: q})
	require.NoError(t, err)

	ctx := context.Background()
	schema, err := worker.GetSchemaOverNetwork(ctx, res.Schema)
	require.NoError(t, err)
	return schema
}

func loadPolygon(name string) (geom.T, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var b bytes.Buffer
	_, err = io.Copy(&b, f)
	if err != nil {
		return nil, err
	}

	var g geojson.Geometry
	g.Type = "MultiPolygon"
	m := json.RawMessage(b.Bytes())
	g.Coordinates = &m
	return g.Decode()
}
