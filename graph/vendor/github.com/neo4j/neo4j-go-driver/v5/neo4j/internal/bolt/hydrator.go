/*
 * Copyright (c) "Neo4j"
 * Neo4j Sweden AB [https://neo4j.com]
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package bolt

import (
	"errors"
	"fmt"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j/internal/errorutil"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j/internal/gql"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j/db"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j/dbtype"
	idb "github.com/neo4j/neo4j-go-driver/v5/neo4j/internal/db"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j/internal/packstream"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j/log"
)

const containsSystemUpdatesKey = "contains-system-updates"
const containsUpdatesKey = "contains-updates"

type ignored struct{}
type success struct {
	fields             []string
	tfirst             int64
	qid                int64
	bookmark           string
	connectionId       string
	server             string
	db                 string
	hasMore            bool
	tlast              int64
	qtype              db.StatementType
	counters           map[string]any
	plan               *db.Plan
	profile            *db.ProfiledPlan
	notifications      []db.Notification
	statuses           []db.GqlStatusObject
	routingTable       *idb.RoutingTable
	num                uint32
	configurationHints map[string]any
	patches            []string
}

func (s *success) String() string {
	str := fmt.Sprintf("%#v", s)
	if s.plan != nil {
		str += fmt.Sprintf(" \nplan: %#v", s.plan)
	}
	if s.profile != nil {
		str += fmt.Sprintf(" \nprofile: %#v", s.profile)
	}
	if s.routingTable != nil {
		str += fmt.Sprintf(" \nrouting table: %#v", s.routingTable)
	}
	return str
}

func (s *success) summary() *db.Summary {
	return &db.Summary{
		Bookmark:              s.bookmark,
		StmntType:             s.qtype,
		Counters:              extractIntCounters(s.counters),
		TLast:                 s.tlast,
		Plan:                  s.plan,
		ProfiledPlan:          s.profile,
		Notifications:         s.notifications,
		GqlStatusObjects:      s.statuses,
		Database:              s.db,
		ContainsSystemUpdates: extractBoolPointer(s.counters, containsSystemUpdatesKey),
		ContainsUpdates:       extractBoolPointer(s.counters, containsUpdatesKey),
	}
}

func extractIntCounters(counters map[string]any) map[string]int {
	result := make(map[string]int, len(counters))
	for k, v := range counters {
		if k != containsSystemUpdatesKey && k != containsUpdatesKey {
			result[k] = v.(int)
		}
	}
	return result
}

func extractBoolPointer(counters map[string]any, key string) *bool {
	result, ok := counters[key]
	if !ok {
		return nil
	}
	return result.(*bool)
}

type hydrator struct {
	unpacker      packstream.Unpacker
	unp           *packstream.Unpacker
	err           error
	cachedIgnored ignored
	cachedSuccess success
	boltLogger    log.BoltLogger
	logId         string
	boltMajor     int
	useUtc        bool
}

func (h *hydrator) setErr(err error) {
	if h.err == nil {
		h.err = err
	}
}

func (h *hydrator) getErr() error {
	if h.unp.Err != nil {
		return h.unp.Err
	}
	return h.err
}

func (h *hydrator) assertLength(structType string, expected, actual uint32) {
	if expected != actual {
		h.setErr(&db.ProtocolError{
			MessageType: structType,
			Err: fmt.Sprintf("Invalid length of struct, expected %d but was %d",
				expected, actual),
		})
	}
}

// hydrate hydrates a top-level struct message
func (h *hydrator) hydrate(buf []byte) (x any, err error) {
	h.unp = &h.unpacker
	h.unp.Reset(buf)
	h.unp.Next()

	if h.unp.Curr != packstream.PackedStruct {
		return nil, errors.New("expected struct")
	}

	n := h.unp.Len()
	t := h.unp.StructTag()
	switch t {
	case msgSuccess:
		x = h.success(n)
	case msgIgnored:
		x = h.ignored(n)
	case msgFailure:
		x = h.failure(n, false)
	case msgRecord:
		x = h.record(n)
	default:
		return nil, fmt.Errorf("unexpected tag at top level: %d", t)
	}
	err = h.getErr()
	return
}

func (h *hydrator) ignored(n uint32) *ignored {
	h.assertLength("ignored", 0, n)
	if h.getErr() != nil {
		return nil
	}
	if h.boltLogger != nil {
		h.boltLogger.LogServerMessage(h.logId, "IGNORED")
	}
	return &h.cachedIgnored
}

func (h *hydrator) failure(n uint32, isNestedError bool) *db.Neo4jError {
	h.assertLength("failure", 1, n)
	if h.getErr() != nil {
		return nil
	}
	dberr := db.Neo4jError{}
	// Skip h.unp.Next() for nested errors to avoid reprocessing the unpacker,
	// as it's already handled by the recursive call.
	if !isNestedError {
		h.unp.Next() // Detect map
	}
	for maplen := h.unp.Len(); maplen > 0; maplen-- {
		h.unp.Next()
		key := h.unp.String()
		h.unp.Next()
		switch key {
		case "gql_status":
			dberr.GqlStatus = h.unp.String()
		case "description":
			dberr.GqlStatusDescription = h.unp.String()
		case "neo4j_code":
			fallthrough
		case "code":
			dberr.Code = h.unp.String()
		case "message":
			dberr.Msg = h.unp.String()
		case "diagnostic_record":
			dberr.GqlDiagnosticRecord = h.amap()
		case "cause":
			dberr.GqlCause = h.failure(1, true)
		default:
			// Do not fail on unknown value in map
			h.trash()
		}
	}
	errorutil.PolyfillGqlError(&dberr)
	if h.boltLogger != nil {
		h.boltLogger.LogServerMessage(h.logId, "FAILURE %s", loggableFailure(dberr))
	}
	return &dberr
}

func (h *hydrator) success(n uint32) *success {
	h.assertLength("success", 1, n)
	if h.getErr() != nil {
		return nil
	}
	// Use cached success but clear it first
	h.cachedSuccess = success{}
	h.cachedSuccess.qid = -1
	h.cachedSuccess.tfirst = -1
	h.cachedSuccess.tlast = -1
	succ := &h.cachedSuccess

	h.unp.Next() // Detect map
	n = h.unp.Len()
	succ.num = n
	for ; n > 0; n-- {
		// Key
		h.unp.Next()
		key := h.unp.String()
		// Value
		h.unp.Next()
		switch key {
		case "fields":
			succ.fields = h.strings()
		case "t_first":
			succ.tfirst = h.unp.Int()
		case "qid":
			succ.qid = h.unp.Int()
		case "bookmark":
			succ.bookmark = h.unp.String()
		case "connection_id":
			succ.connectionId = h.unp.String()
		case "server":
			succ.server = h.unp.String()
		case "has_more":
			succ.hasMore = h.unp.Bool()
		case "t_last":
			succ.tlast = h.unp.Int()
		case "type":
			statementType := h.unp.String()
			switch statementType {
			case "r":
				succ.qtype = db.StatementTypeRead
			case "w":
				succ.qtype = db.StatementTypeWrite
			case "rw":
				succ.qtype = db.StatementTypeReadWrite
			case "s":
				succ.qtype = db.StatementTypeSchemaWrite
			default:
				h.setErr(&db.ProtocolError{
					MessageType: "success",
					Field:       "type",
					Err:         fmt.Sprintf("unrecognized success statement type %s", statementType),
				})
			}
		case "db":
			succ.db = h.unp.String()
		case "stats":
			succ.counters = h.successStats()
		case "plan":
			m := h.amap()
			succ.plan = parsePlan(m)
		case "profile":
			m := h.amap()
			succ.profile = parseProfile(m)
		case "notifications":
			l := h.array()
			succ.notifications = parseNotifications(l)
		case "statuses":
			l := h.array()
			succ.statuses = parseGqlStatusObjects(l)
		case "rt":
			succ.routingTable = h.routingTable()
		case "hints":
			hints := h.amap()
			succ.configurationHints = hints
		case "patch_bolt":
			patches := h.strings()
			succ.patches = patches
		default:
			// Unknown key, waste it
			h.trash()
		}
	}
	if h.boltLogger != nil {
		h.boltLogger.LogServerMessage(h.logId, "SUCCESS %s", loggableSuccess(*succ))
	}
	return succ
}

func (h *hydrator) successStats() map[string]any {
	n := h.unp.Len()
	if n == 0 {
		return nil
	}
	counts := make(map[string]any, n)
	for ; n > 0; n-- {
		h.unp.Next()
		key := h.unp.String()
		h.unp.Next()
		val := h.parseStatValue(key)
		counts[key] = val
	}
	return counts
}

func (h *hydrator) parseStatValue(key string) any {
	var val any
	switch key {
	case containsSystemUpdatesKey, containsUpdatesKey:
		boolValue := h.unp.Bool()
		val = &boolValue
	default:
		val = int(h.unp.Int())
	}
	return val
}

// routingTable parses a routing table sent from the server. This is done
// the 'hard' way to reduce number of allocations (would be easier to go via
// a map) since it is called in normal flow (not that frequent...).
func (h *hydrator) routingTable() *idb.RoutingTable {
	rt := idb.RoutingTable{}
	// Length of map
	nkeys := h.unp.Len()
	for ; nkeys > 0; nkeys-- {
		h.unp.Next()
		key := h.unp.String()
		h.unp.Next()
		switch key {
		case "ttl":
			rt.TimeToLive = int(h.unp.Int())
		case "servers":
			nservers := h.unp.Len()
			for ; nservers > 0; nservers-- {
				h.routingTableRole(&rt)
			}
		case "db":
			rt.DatabaseName = h.unp.String()
		default:
			// Unknown key, waste the value
			h.trash()
		}
	}
	return &rt
}

func (h *hydrator) routingTableRole(rt *idb.RoutingTable) {
	h.unp.Next()
	nkeys := h.unp.Len()
	var role string
	var addresses []string
	for ; nkeys > 0; nkeys-- {
		h.unp.Next()
		key := h.unp.String()
		h.unp.Next()
		switch key {
		case "role":
			role = h.unp.String()
		case "addresses":
			addresses = h.strings()
		default:
			// Unknown key, waste the value
			h.trash()
		}
	}
	switch role {
	case "READ":
		rt.Readers = addresses
	case "WRITE":
		rt.Writers = addresses
	case "ROUTE":
		rt.Routers = addresses
	}
}

func (h *hydrator) strings() []string {
	n := h.unp.Len()
	slice := make([]string, n)
	for i := range slice {
		h.unp.Next()
		slice[i] = h.unp.String()
	}
	return slice
}

func (h *hydrator) amap() map[string]any {
	n := h.unp.Len()
	m := make(map[string]any, n)
	for ; n > 0; n-- {
		h.unp.Next()
		key := h.unp.String()
		h.unp.Next()
		m[key] = h.value()
	}
	return m
}

func (h *hydrator) array() []any {
	n := h.unp.Len()
	a := make([]any, n)
	for i := range a {
		h.unp.Next()
		a[i] = h.value()
	}
	return a
}

func (h *hydrator) record(n uint32) *db.Record {
	h.assertLength("record", 1, n)
	if h.getErr() != nil {
		return nil
	}
	rec := db.Record{}
	h.unp.Next() // Detect array
	n = h.unp.Len()
	rec.Values = make([]any, n)
	for i := range rec.Values {
		h.unp.Next()
		rec.Values[i] = h.value()
	}
	if h.boltLogger != nil {
		h.boltLogger.LogServerMessage(h.logId, "RECORD %s", loggableList(rec.Values))
	}
	return &rec
}

func (h *hydrator) value() any {
	valueType := h.unp.Curr
	switch valueType {
	case packstream.PackedInt:
		return h.unp.Int()
	case packstream.PackedFloat:
		return h.unp.Float()
	case packstream.PackedStr:
		return h.unp.String()
	case packstream.PackedStruct:
		t := h.unp.StructTag()
		n := h.unp.Len()
		switch t {
		case 'N':
			if h.boltMajor >= 5 {
				return h.nodeWithElementId(n)
			}
			return h.node(n)
		case 'R':
			if h.boltMajor >= 5 {
				return h.relationshipWithElementId(n)
			}
			return h.relationship(n)
		case 'r':
			if h.boltMajor >= 5 {
				return h.relationnodeWithElementId(n)
			}
			return h.relationnode(n)
		case 'P':
			return h.path(n)
		case 'X':
			return h.point2d(n)
		case 'Y':
			return h.point3d(n)
		case 'F':
			if h.useUtc {
				return h.unknownStructError(t)
			}
			return h.dateTimeOffset(n)
		case 'I':
			if !h.useUtc {
				return h.unknownStructError(t)
			}
			return h.utcDateTimeOffset(n)
		case 'f':
			if h.useUtc {
				return h.unknownStructError(t)
			}
			return h.dateTimeNamedZone(n)
		case 'i':
			if !h.useUtc {
				return h.unknownStructError(t)
			}
			return h.utcDateTimeNamedZone(n)
		case 'd':
			return h.localDateTime(n)
		case 'D':
			return h.date(n)
		case 'T':
			return h.time(n)
		case 't':
			return h.localTime(n)
		case 'E':
			return h.duration(n)
		default:
			return h.unknownStructError(t)
		}
	case packstream.PackedByteArray:
		return h.unp.ByteArray()
	case packstream.PackedArray:
		return h.array()
	case packstream.PackedMap:
		return h.amap()
	case packstream.PackedNil:
		return nil
	case packstream.PackedTrue:
		return true
	case packstream.PackedFalse:
		return false
	default:
		h.setErr(&db.ProtocolError{
			Err: fmt.Sprintf("Received unknown packstream value type: %d", valueType),
		})
		return nil
	}
}

// Trashes current value
func (h *hydrator) trash() {
	// TODO Less consuming implementation
	h.value()
}

func (h *hydrator) node(num uint32) any {
	h.assertLength("node", 3, num)
	if h.getErr() != nil {
		return nil
	}
	n := dbtype.Node{}
	h.unp.Next()
	//lint:ignore SA1019 Id is supported at least until 6.0
	n.Id = h.unp.Int()
	h.unp.Next()
	n.Labels = h.strings()
	h.unp.Next()
	n.Props = h.amap()
	//lint:ignore SA1019 Id is supported at least until 6.0
	n.ElementId = fmt.Sprintf("%d", n.Id)
	return n
}

func (h *hydrator) nodeWithElementId(num uint32) any {
	h.assertLength("node", 4, num)
	if h.getErr() != nil {
		return nil
	}
	n := dbtype.Node{}
	h.unp.Next()
	//lint:ignore SA1019 Id is supported at least until 6.0
	n.Id = h.unp.Int()
	h.unp.Next()
	n.Labels = h.strings()
	h.unp.Next()
	n.Props = h.amap()
	h.unp.Next()
	n.ElementId = h.unp.String()
	return n
}

func (h *hydrator) relationship(n uint32) any {
	h.assertLength("relationship", 5, n)
	if h.getErr() != nil {
		return nil
	}
	r := dbtype.Relationship{}
	h.unp.Next()
	//lint:ignore SA1019 Id is supported at least until 6.0
	r.Id = h.unp.Int()
	h.unp.Next()
	//lint:ignore SA1019 StartId is supported at least until 6.0
	r.StartId = h.unp.Int()
	h.unp.Next()
	//lint:ignore SA1019 EndId is supported at least until 6.0
	r.EndId = h.unp.Int()
	h.unp.Next()
	r.Type = h.unp.String()
	h.unp.Next()
	r.Props = h.amap()
	//lint:ignore SA1019 Id is supported at least until 6.0
	r.ElementId = fmt.Sprintf("%d", r.Id)
	//lint:ignore SA1019 StartId is supported at least until 6.0
	r.StartElementId = fmt.Sprintf("%d", r.StartId)
	//lint:ignore SA1019 EndId is supported at least until 6.0
	r.EndElementId = fmt.Sprintf("%d", r.EndId)
	return r
}

func (h *hydrator) relationshipWithElementId(n uint32) any {
	h.assertLength("relationship", 8, n)
	if h.getErr() != nil {
		return nil
	}
	r := dbtype.Relationship{}
	h.unp.Next()
	//lint:ignore SA1019 Id is supported at least until 6.0
	r.Id = h.unp.Int()
	h.unp.Next()
	//lint:ignore SA1019 StartId is supported at least until 6.0
	r.StartId = h.unp.Int()
	h.unp.Next()
	//lint:ignore SA1019 EndId is supported at least until 6.0
	r.EndId = h.unp.Int()
	h.unp.Next()
	r.Type = h.unp.String()
	h.unp.Next()
	r.Props = h.amap()
	h.unp.Next()
	r.ElementId = h.unp.String()
	h.unp.Next()
	r.StartElementId = h.unp.String()
	h.unp.Next()
	r.EndElementId = h.unp.String()
	return r
}

func (h *hydrator) relationnode(n uint32) any {
	h.assertLength("relationnode", 3, n)
	if h.getErr() != nil {
		return nil
	}
	r := relNode{}
	h.unp.Next()
	r.id = h.unp.Int()
	h.unp.Next()
	r.name = h.unp.String()
	h.unp.Next()
	r.props = h.amap()
	r.elementId = fmt.Sprintf("%d", r.id)
	return &r
}

func (h *hydrator) relationnodeWithElementId(n uint32) any {
	h.assertLength("relationnode", 4, n)
	if h.getErr() != nil {
		return nil
	}
	r := relNode{}
	h.unp.Next()
	r.id = h.unp.Int()
	h.unp.Next()
	r.name = h.unp.String()
	h.unp.Next()
	r.props = h.amap()
	h.unp.Next()
	r.elementId = h.unp.String()
	return &r
}

func (h *hydrator) path(n uint32) any {
	h.assertLength("path", 3, n)
	if h.getErr() != nil {
		return nil
	}
	// Array of nodes
	h.unp.Next()
	num := h.unp.Len()
	nodes := make([]dbtype.Node, num)
	for i := range nodes {
		h.unp.Next()
		node, ok := h.value().(dbtype.Node)
		if !ok {
			h.setErr(&db.ProtocolError{
				MessageType: "path",
				Field:       "nodes",
				Err:         "value could not be cast to Node",
			})
			return nil
		}
		nodes[i] = node
	}
	// Array of relnodes
	h.unp.Next()
	num = h.unp.Len()
	rnodes := make([]*relNode, num)
	for i := range rnodes {
		h.unp.Next()
		rnode, ok := h.value().(*relNode)
		if !ok {
			h.setErr(&db.ProtocolError{
				MessageType: "path",
				Field:       "rnodes",
				Err:         "value could be not cast to *relNode",
			})
			return nil
		}
		rnodes[i] = rnode
	}
	// Array of indexes
	h.unp.Next()
	num = h.unp.Len()
	indexes := make([]int, num)
	for i := range indexes {
		h.unp.Next()
		indexes[i] = int(h.unp.Int())
	}

	if (len(indexes) & 0x01) == 1 {
		h.setErr(&db.ProtocolError{
			MessageType: "path",
			Field:       "indices",
			Err:         fmt.Sprintf("there should be an even number of indices, found %d", len(indexes)),
		})
		return nil
	}

	return buildPath(nodes, rnodes, indexes)
}

func (h *hydrator) point2d(n uint32) any {
	p := dbtype.Point2D{}
	h.unp.Next()
	p.SpatialRefId = uint32(h.unp.Int())
	h.unp.Next()
	p.X = h.unp.Float()
	h.unp.Next()
	p.Y = h.unp.Float()
	return p
}

func (h *hydrator) point3d(n uint32) any {
	p := dbtype.Point3D{}
	h.unp.Next()
	p.SpatialRefId = uint32(h.unp.Int())
	h.unp.Next()
	p.X = h.unp.Float()
	h.unp.Next()
	p.Y = h.unp.Float()
	h.unp.Next()
	p.Z = h.unp.Float()
	return p
}

func (h *hydrator) dateTimeOffset(n uint32) any {
	h.unp.Next()
	seconds := h.unp.Int()
	h.unp.Next()
	nanos := h.unp.Int()
	h.unp.Next()
	offset := h.unp.Int()
	// time.Time in local timezone, e.g. 15th of June 2020, 15:30 in Paris (UTC+2h)
	unixTime := time.Unix(seconds, nanos)
	// time.Time computed in UTC timezone, e.g. 15th of June 2020, 13:30 in UTC
	utcTime := unixTime.UTC()
	// time.Time **copied** as-is in the target timezone, e.g. 15th of June 2020, 13:30 in target tz
	timeZone := time.FixedZone("Offset", int(offset))
	return time.Date(
		utcTime.Year(),
		utcTime.Month(),
		utcTime.Day(),
		utcTime.Hour(),
		utcTime.Minute(),
		utcTime.Second(),
		utcTime.Nanosecond(),
		timeZone,
	)
}

func (h *hydrator) utcDateTimeOffset(n uint32) any {
	h.unp.Next()
	seconds := h.unp.Int()
	h.unp.Next()
	nanos := h.unp.Int()
	h.unp.Next()
	offset := h.unp.Int()
	timeZone := time.FixedZone("Offset", int(offset))
	return time.Unix(seconds, nanos).In(timeZone)
}

func (h *hydrator) dateTimeNamedZone(n uint32) any {
	h.unp.Next()
	seconds := h.unp.Int()
	h.unp.Next()
	nanos := h.unp.Int()
	h.unp.Next()
	zone := h.unp.String()
	// time.Time in local timezone, e.g. 15th of June 2020, 15:30 in Paris (UTC+2h)
	unixTime := time.Unix(seconds, nanos)
	// time.Time computed in UTC timezone, e.g. 15th of June 2020, 13:30 in UTC
	utcTime := unixTime.UTC()
	// time.Time **copied** as-is in the target timezone, e.g. 15th of June 2020, 13:30 in target tz
	l, err := time.LoadLocation(zone)
	if err != nil {
		return &dbtype.InvalidValue{
			Message: "dateTimeNamedZone",
			Err:     err,
		}
	}
	return time.Date(
		utcTime.Year(),
		utcTime.Month(),
		utcTime.Day(),
		utcTime.Hour(),
		utcTime.Minute(),
		utcTime.Second(),
		utcTime.Nanosecond(),
		l,
	)
}

func (h *hydrator) utcDateTimeNamedZone(n uint32) any {
	h.unp.Next()
	secs := h.unp.Int()
	h.unp.Next()
	nans := h.unp.Int()
	h.unp.Next()
	zone := h.unp.String()
	timeZone, err := time.LoadLocation(zone)
	if err != nil {
		return &dbtype.InvalidValue{
			Message: "utcDateTimeNamedZone",
			Err:     err,
		}
	}
	return time.Unix(secs, nans).In(timeZone)
}

func (h *hydrator) localDateTime(n uint32) any {
	h.unp.Next()
	secs := h.unp.Int()
	h.unp.Next()
	nans := h.unp.Int()
	t := time.Unix(secs, nans).UTC()
	t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.Local)
	return dbtype.LocalDateTime(t)
}

func (h *hydrator) date(n uint32) any {
	h.unp.Next()
	days := h.unp.Int()
	secs := days * 86400
	return dbtype.Date(time.Unix(secs, 0).UTC())
}

func (h *hydrator) time(n uint32) any {
	h.unp.Next()
	nans := h.unp.Int()
	h.unp.Next()
	offs := h.unp.Int()
	secs := nans / int64(time.Second)
	nans -= secs * int64(time.Second)
	l := time.FixedZone("Offset", int(offs))
	t := time.Date(0, 0, 0, 0, 0, int(secs), int(nans), l)
	return dbtype.Time(t)
}

func (h *hydrator) localTime(n uint32) any {
	h.unp.Next()
	nans := h.unp.Int()
	secs := nans / int64(time.Second)
	nans -= secs * int64(time.Second)
	t := time.Date(0, 0, 0, 0, 0, int(secs), int(nans), time.Local)
	return dbtype.LocalTime(t)
}

func (h *hydrator) duration(n uint32) any {
	h.unp.Next()
	mon := h.unp.Int()
	h.unp.Next()
	day := h.unp.Int()
	h.unp.Next()
	sec := h.unp.Int()
	h.unp.Next()
	nan := h.unp.Int()
	return dbtype.Duration{Months: mon, Days: day, Seconds: sec, Nanos: int(nan)}
}

func parseNotifications(notificationsx []any) []db.Notification {
	var notifications []db.Notification
	if notificationsx != nil {
		notifications = make([]db.Notification, 0, len(notificationsx))
		for _, x := range notificationsx {
			notificationx, ok := x.(map[string]any)
			if ok {
				notifications = append(notifications, parseNotification(notificationx))
			}
		}
	}
	return notifications
}

func parseGqlStatusObjects(statuses []any) []db.GqlStatusObject {
	var gqlStatusObjects []db.GqlStatusObject
	if statuses != nil {
		gqlStatusObjects = make([]db.GqlStatusObject, 0, len(statuses))
		for _, x := range statuses {
			status, ok := x.(map[string]any)
			if ok {
				gqlStatusObjects = append(gqlStatusObjects, parseGqlStatusObject(status))
			}
		}
	}
	return gqlStatusObjects
}

func parsePlanOpIdArgsChildren(planx map[string]any) (string, []string, map[string]any, []any) {
	operator, _ := planx["operatorType"].(string)
	identifiersx, _ := planx["identifiers"].([]any)
	arguments, _ := planx["args"].(map[string]any)

	identifiers := make([]string, len(identifiersx))
	for i, id := range identifiersx {
		identifiers[i], _ = id.(string)
	}

	childrenx, _ := planx["children"].([]any)

	return operator, identifiers, arguments, childrenx
}

func parsePlan(planx map[string]any) *db.Plan {
	op, ids, args, childrenx := parsePlanOpIdArgsChildren(planx)
	plan := &db.Plan{
		Operator:    op,
		Arguments:   args,
		Identifiers: ids,
	}

	plan.Children = make([]db.Plan, 0, len(childrenx))
	for _, c := range childrenx {
		childPlanx, _ := c.(map[string]any)
		if len(childPlanx) > 0 {
			childPlan := parsePlan(childPlanx)
			if childPlan != nil {
				plan.Children = append(plan.Children, *childPlan)
			}
		}
	}

	return plan
}

func parseProfile(profilex map[string]any) *db.ProfiledPlan {
	op, ids, args, childrenx := parsePlanOpIdArgsChildren(profilex)
	plan := &db.ProfiledPlan{
		Operator:    op,
		Arguments:   args,
		Identifiers: ids,
	}

	plan.DbHits, _ = profilex["dbHits"].(int64)
	plan.Records, _ = profilex["rows"].(int64)

	plan.Children = make([]db.ProfiledPlan, 0, len(childrenx))
	for _, c := range childrenx {
		childPlanx, _ := c.(map[string]any)
		if len(childPlanx) > 0 {
			childPlan := parseProfile(childPlanx)
			if childPlan != nil {
				if pageCacheMisses, ok := childPlanx["pageCacheMisses"]; ok {
					childPlan.PageCacheMisses = pageCacheMisses.(int64)
				}
				if pageCacheHits, ok := childPlanx["pageCacheHits"]; ok {
					childPlan.PageCacheHits = pageCacheHits.(int64)
				}
				if pageCacheHitRatio, ok := childPlanx["pageCacheHitRatio"]; ok {
					childPlan.PageCacheHitRatio = pageCacheHitRatio.(float64)
				}
				if planTime, ok := childPlanx["time"]; ok {
					childPlan.Time = planTime.(int64)
				}
				plan.Children = append(plan.Children, *childPlan)
			}
		}
	}

	return plan
}

func parseInputPosition(m map[string]any) *db.InputPosition {
	if m == nil {
		return nil
	}
	pos := &db.InputPosition{}
	if i, ok := m["column"].(int64); ok {
		pos.Column = int(i)
	}
	if i, ok := m["line"].(int64); ok {
		pos.Line = int(i)
	}
	if i, ok := m["offset"].(int64); ok {
		pos.Offset = int(i)
	}
	return pos
}

func parseNotification(m map[string]any) db.Notification {
	n := db.Notification{}
	n.Code, _ = m["code"].(string)
	if description, ok := m["description"].(string); ok {
		n.Description = description
	}
	n.Severity, _ = m["severity"].(string)
	n.Category, _ = m["category"].(string)
	n.Title, _ = m["title"].(string)
	if pos, ok := m["position"].(map[string]any); ok {
		n.Position = parseInputPosition(pos)
	}
	return n
}

func parseGqlStatusObject(m map[string]any) db.GqlStatusObject {
	g := db.GqlStatusObject{}

	if status, ok := m["gql_status"].(string); ok {
		g.GqlStatus = status
	}

	if statusDescription, ok := m["status_description"].(string); ok {
		g.StatusDescription = statusDescription
	}

	// Backward compatibility support for older Notification API.
	if code, ok := m["neo4j_code"].(string); ok {
		//lint:ignore SA1019 Code is supported at least until 6.0
		g.Code = code
		g.IsNotification = true
	}

	// Backward compatibility support for older Notification API.
	if title, ok := m["title"].(string); ok {
		//lint:ignore SA1019 Title is supported at least until 6.0
		g.Title = title
	}

	// Backward compatibility support for older Notification API.
	if description, ok := m["description"].(string); ok {
		//lint:ignore SA1019 Description is supported at least until 6.0
		g.Description = description
	}

	// Initialize the default diagnostic record
	diagnosticRecord := gql.NewDefaultDiagnosticRecord()

	// Merge the diagnostic record from the map m
	if dr, ok := m["diagnostic_record"].(map[string]any); ok {
		for key, value := range dr {
			diagnosticRecord[key] = value
		}
		if pos, ok := dr["_position"].(map[string]any); ok {
			g.Position = parseInputPosition(pos)
		}
		if classification, ok := dr["_classification"].(string); ok {
			g.Classification = classification
		}
		if severity, ok := dr["_severity"].(string); ok {
			g.Severity = severity
		}
	}

	g.DiagnosticRecord = diagnosticRecord
	return g
}

func (h *hydrator) unknownStructError(t byte) any {
	h.setErr(&db.ProtocolError{
		Err: fmt.Sprintf("Received unknown struct tag: %d", t),
	})
	return nil
}
