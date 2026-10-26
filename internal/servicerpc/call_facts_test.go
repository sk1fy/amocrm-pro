package servicerpc

import (
	"reflect"
	"testing"

	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/servicerpc/pb"
	"google.golang.org/protobuf/proto"
)

func TestCallFactsProtobufPreservesUnknownAndZero(t *testing.T) {
	zero, seconds := int64(0), int64(120)
	phone, result := "+70000000001", "4"
	for _, facts := range []*serviceapi.CallFacts{
		nil,
		{Direction: "incoming"},
		{Duration: &zero, Phone: &phone, Direction: "incoming", Source: serviceapi.SourceEventPayload},
		{Duration: &seconds, Phone: &phone, Result: &result, Direction: "outgoing", Source: serviceapi.SourceNotesAPI, Current: true},
	} {
		want := serviceapi.Event{ID: "synthetic-call", Type: "incoming_call", CallFacts: facts}
		wire, err := proto.Marshal(toEvent(want))
		if err != nil {
			t.Fatal(err)
		}
		var decoded pb.Event
		if err := proto.Unmarshal(wire, &decoded); err != nil {
			t.Fatal(err)
		}
		if got := fromEvent(&decoded); !reflect.DeepEqual(got, want) {
			t.Fatalf("call facts changed over protobuf: got %+v want %+v", got.CallFacts, want.CallFacts)
		}
	}
}
