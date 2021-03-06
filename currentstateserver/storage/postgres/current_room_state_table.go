// Copyright 2020 The Matrix.org Foundation C.I.C.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package postgres

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/lib/pq"
	"github.com/matrix-org/dendrite/currentstateserver/storage/tables"
	"github.com/matrix-org/dendrite/internal"
	"github.com/matrix-org/dendrite/internal/sqlutil"
	"github.com/matrix-org/gomatrixserverlib"
)

var currentRoomStateSchema = `
-- Stores the current room state for every room.
CREATE TABLE IF NOT EXISTS currentstate_current_room_state (
    -- The 'room_id' key for the state event.
    room_id TEXT NOT NULL,
    -- The state event ID
    event_id TEXT NOT NULL,
    -- The state event type e.g 'm.room.member'
    type TEXT NOT NULL,
    -- The 'sender' property of the event.
    sender TEXT NOT NULL,
    -- The state_key value for this state event e.g ''
    state_key TEXT NOT NULL,
    -- The JSON for the event. Stored as TEXT because this should be valid UTF-8.
    headered_event_json TEXT NOT NULL,
    -- A piece of extracted content e.g membership for m.room.member events
    content_value TEXT NOT NULL DEFAULT '',
    -- Clobber based on 3-uple of room_id, type and state_key
    CONSTRAINT currentstate_current_room_state_unique UNIQUE (room_id, type, state_key)
);
-- for event deletion
CREATE UNIQUE INDEX IF NOT EXISTS currentstate_event_id_idx ON currentstate_current_room_state(event_id, room_id, type, sender);
-- for querying membership states of users
CREATE INDEX IF NOT EXISTS currentstate_membership_idx ON currentstate_current_room_state(type, state_key, content_value)
WHERE type='m.room.member' AND content_value IS NOT NULL AND content_value != 'leave';
`

const upsertRoomStateSQL = "" +
	"INSERT INTO currentstate_current_room_state (room_id, event_id, type, sender, state_key, headered_event_json, content_value)" +
	" VALUES ($1, $2, $3, $4, $5, $6, $7)" +
	" ON CONFLICT ON CONSTRAINT currentstate_current_room_state_unique" +
	" DO UPDATE SET event_id = $2, sender=$4, headered_event_json = $6, content_value = $7"

const deleteRoomStateByEventIDSQL = "" +
	"DELETE FROM currentstate_current_room_state WHERE event_id = $1"

const selectRoomIDsWithMembershipSQL = "" +
	"SELECT room_id FROM currentstate_current_room_state WHERE type = 'm.room.member' AND state_key = $1 AND content_value = $2"

const selectStateEventSQL = "" +
	"SELECT headered_event_json FROM currentstate_current_room_state WHERE room_id = $1 AND type = $2 AND state_key = $3"

const selectEventsWithEventIDsSQL = "" +
	"SELECT headered_event_json FROM currentstate_current_room_state WHERE event_id = ANY($1)"

const selectBulkStateContentSQL = "" +
	"SELECT room_id, type, state_key, content_value FROM currentstate_current_room_state WHERE room_id = ANY($1) AND type = ANY($2) AND state_key = ANY($3)"

const selectBulkStateContentWildSQL = "" +
	"SELECT room_id, type, state_key, content_value FROM currentstate_current_room_state WHERE room_id = ANY($1) AND type = ANY($2)"

type currentRoomStateStatements struct {
	upsertRoomStateStmt             *sql.Stmt
	deleteRoomStateByEventIDStmt    *sql.Stmt
	selectRoomIDsWithMembershipStmt *sql.Stmt
	selectEventsWithEventIDsStmt    *sql.Stmt
	selectStateEventStmt            *sql.Stmt
	selectBulkStateContentStmt      *sql.Stmt
	selectBulkStateContentWildStmt  *sql.Stmt
}

func NewPostgresCurrentRoomStateTable(db *sql.DB) (tables.CurrentRoomState, error) {
	s := &currentRoomStateStatements{}
	_, err := db.Exec(currentRoomStateSchema)
	if err != nil {
		return nil, err
	}
	if s.upsertRoomStateStmt, err = db.Prepare(upsertRoomStateSQL); err != nil {
		return nil, err
	}
	if s.deleteRoomStateByEventIDStmt, err = db.Prepare(deleteRoomStateByEventIDSQL); err != nil {
		return nil, err
	}
	if s.selectRoomIDsWithMembershipStmt, err = db.Prepare(selectRoomIDsWithMembershipSQL); err != nil {
		return nil, err
	}
	if s.selectEventsWithEventIDsStmt, err = db.Prepare(selectEventsWithEventIDsSQL); err != nil {
		return nil, err
	}
	if s.selectStateEventStmt, err = db.Prepare(selectStateEventSQL); err != nil {
		return nil, err
	}
	if s.selectBulkStateContentStmt, err = db.Prepare(selectBulkStateContentSQL); err != nil {
		return nil, err
	}
	if s.selectBulkStateContentWildStmt, err = db.Prepare(selectBulkStateContentWildSQL); err != nil {
		return nil, err
	}
	return s, nil
}

// SelectRoomIDsWithMembership returns the list of room IDs which have the given user in the given membership state.
func (s *currentRoomStateStatements) SelectRoomIDsWithMembership(
	ctx context.Context,
	txn *sql.Tx,
	userID string,
	contentVal string,
) ([]string, error) {
	stmt := sqlutil.TxStmt(txn, s.selectRoomIDsWithMembershipStmt)
	rows, err := stmt.QueryContext(ctx, userID, contentVal)
	if err != nil {
		return nil, err
	}
	defer internal.CloseAndLogIfError(ctx, rows, "selectRoomIDsWithMembership: rows.close() failed")

	var result []string
	for rows.Next() {
		var roomID string
		if err := rows.Scan(&roomID); err != nil {
			return nil, err
		}
		result = append(result, roomID)
	}
	return result, rows.Err()
}

func (s *currentRoomStateStatements) DeleteRoomStateByEventID(
	ctx context.Context, txn *sql.Tx, eventID string,
) error {
	stmt := sqlutil.TxStmt(txn, s.deleteRoomStateByEventIDStmt)
	_, err := stmt.ExecContext(ctx, eventID)
	return err
}

func (s *currentRoomStateStatements) UpsertRoomState(
	ctx context.Context, txn *sql.Tx,
	event gomatrixserverlib.HeaderedEvent, contentVal string,
) error {
	headeredJSON, err := json.Marshal(event)
	if err != nil {
		return err
	}

	// upsert state event
	stmt := sqlutil.TxStmt(txn, s.upsertRoomStateStmt)
	_, err = stmt.ExecContext(
		ctx,
		event.RoomID(),
		event.EventID(),
		event.Type(),
		event.Sender(),
		*event.StateKey(),
		headeredJSON,
		contentVal,
	)
	return err
}

func (s *currentRoomStateStatements) SelectEventsWithEventIDs(
	ctx context.Context, txn *sql.Tx, eventIDs []string,
) ([]gomatrixserverlib.HeaderedEvent, error) {
	stmt := sqlutil.TxStmt(txn, s.selectEventsWithEventIDsStmt)
	rows, err := stmt.QueryContext(ctx, pq.StringArray(eventIDs))
	if err != nil {
		return nil, err
	}
	defer internal.CloseAndLogIfError(ctx, rows, "selectEventsWithEventIDs: rows.close() failed")
	result := []gomatrixserverlib.HeaderedEvent{}
	for rows.Next() {
		var eventBytes []byte
		if err := rows.Scan(&eventBytes); err != nil {
			return nil, err
		}
		var ev gomatrixserverlib.HeaderedEvent
		if err := json.Unmarshal(eventBytes, &ev); err != nil {
			return nil, err
		}
		result = append(result, ev)
	}
	return result, rows.Err()
}

func (s *currentRoomStateStatements) SelectStateEvent(
	ctx context.Context, roomID, evType, stateKey string,
) (*gomatrixserverlib.HeaderedEvent, error) {
	stmt := s.selectStateEventStmt
	var res []byte
	err := stmt.QueryRowContext(ctx, roomID, evType, stateKey).Scan(&res)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ev gomatrixserverlib.HeaderedEvent
	if err = json.Unmarshal(res, &ev); err != nil {
		return nil, err
	}
	return &ev, err
}

func (s *currentRoomStateStatements) SelectBulkStateContent(
	ctx context.Context, roomIDs []string, tuples []gomatrixserverlib.StateKeyTuple, allowWildcards bool,
) ([]tables.StrippedEvent, error) {
	hasWildcards := false
	eventTypeSet := make(map[string]bool)
	stateKeySet := make(map[string]bool)
	var eventTypes []string
	var stateKeys []string
	for _, tuple := range tuples {
		if !eventTypeSet[tuple.EventType] {
			eventTypeSet[tuple.EventType] = true
			eventTypes = append(eventTypes, tuple.EventType)
		}
		if !stateKeySet[tuple.StateKey] {
			stateKeySet[tuple.StateKey] = true
			stateKeys = append(stateKeys, tuple.StateKey)
		}
		if tuple.StateKey == "*" {
			hasWildcards = true
		}
	}
	var rows *sql.Rows
	var err error
	if hasWildcards && allowWildcards {
		rows, err = s.selectBulkStateContentWildStmt.QueryContext(ctx, pq.StringArray(roomIDs), pq.StringArray(eventTypes))
	} else {
		rows, err = s.selectBulkStateContentStmt.QueryContext(
			ctx, pq.StringArray(roomIDs), pq.StringArray(eventTypes), pq.StringArray(stateKeys),
		)
	}
	if err != nil {
		return nil, err
	}
	strippedEvents := []tables.StrippedEvent{}
	defer internal.CloseAndLogIfError(ctx, rows, "SelectBulkStateContent: rows.close() failed")
	for rows.Next() {
		var roomID string
		var eventType string
		var stateKey string
		var contentVal string
		if err = rows.Scan(&roomID, &eventType, &stateKey, &contentVal); err != nil {
			return nil, err
		}
		strippedEvents = append(strippedEvents, tables.StrippedEvent{
			RoomID:       roomID,
			ContentValue: contentVal,
			EventType:    eventType,
			StateKey:     stateKey,
		})
	}
	return strippedEvents, rows.Err()
}
