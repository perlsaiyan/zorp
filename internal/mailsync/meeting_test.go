package mailsync

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/perlsaiyan/zorp/internal/graph"
	"github.com/perlsaiyan/zorp/internal/store"
)

func TestMeetingFieldsFromGraph(t *testing.T) {
	// As delta returns them, with the eventMessage fields cast-selected.
	raw := `[
	 {"@odata.type":"#microsoft.graph.eventMessageRequest","id":"r","subject":"Check-in","meetingMessageType":"meetingRequest",
	  "type":"seriesMaster","isAllDay":false,"isOutOfDate":false,"responseRequested":true,
	  "startDateTime":{"dateTime":"2026-10-26T21:00:00.0000000","timeZone":"UTC"},
	  "endDateTime":{"dateTime":"2026-10-26T21:30:00.0000000","timeZone":"UTC"},
	  "location":{"displayName":"Room 4"}},
	 {"@odata.type":"#microsoft.graph.eventMessage","id":"c","meetingMessageType":"meetingCancelled",
	  "startDateTime":{"dateTime":"2026-10-29T13:00:00.0000000","timeZone":"UTC"}},
	 {"@odata.type":"#microsoft.graph.eventMessageResponse","id":"a","meetingMessageType":"meetingTenativelyAccepted"},
	 {"id":"p","subject":"Lunch?"}
	]`
	var msgs []graph.Message
	if err := json.Unmarshal([]byte(raw), &msgs); err != nil {
		t.Fatal(err)
	}
	r := toStore(msgs[0]).Meeting
	want := store.Meeting{Kind: store.MeetingRequest, Location: "Room 4", ResponseRequested: true, Recurring: true,
		Start: time.Date(2026, 10, 26, 21, 0, 0, 0, time.UTC), End: time.Date(2026, 10, 26, 21, 30, 0, 0, time.UTC)}
	if r == nil || *r != want {
		t.Errorf("request = %+v", r)
	}
	if c := toStore(msgs[1]).Meeting; c == nil || c.Kind != store.MeetingCancelled || !c.Start.Equal(time.Date(2026, 10, 29, 13, 0, 0, 0, time.UTC)) {
		t.Errorf("cancellation = %+v", c)
	}
	if a := toStore(msgs[2]).Meeting; a == nil || a.Kind != store.MeetingTentative {
		t.Errorf("answer = %+v", a)
	}
	if p := toStore(msgs[3]).Meeting; p != nil {
		t.Errorf("plain mail has a meeting: %+v", p)
	}
}
