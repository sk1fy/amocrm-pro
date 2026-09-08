package servicerpc

import (
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/servicerpc/pb"
	"google.golang.org/protobuf/proto"
	"testing"
)

func TestInvalidEnrichmentIDsSurviveWire(t *testing.T) {
	notes := toNotePage(serviceapi.NotePage{Notes: []serviceapi.Note{{ID: 1}}, InvalidIDs: []int64{2}})
	tasks := toTaskPage(serviceapi.TaskPage{Tasks: []serviceapi.Task{{ID: 1}}, InvalidIDs: []int64{2}})
	entities := toEntityCatalog(serviceapi.EntityCatalog{Entities: []serviceapi.EntityName{{ID: 1}}, InvalidIDs: []int64{2}})
	round := func(src, dst proto.Message) {
		t.Helper()
		b, err := proto.Marshal(src)
		if err != nil {
			t.Fatal(err)
		}
		if err = proto.Unmarshal(b, dst); err != nil {
			t.Fatal(err)
		}
	}
	n := new(pb.NotePage)
	round(notes, n)
	nn := fromNotePage(n)
	a := new(pb.TaskPage)
	round(tasks, a)
	aa := fromTaskPage(a)
	e := new(pb.EntityCatalog)
	round(entities, e)
	ee := fromEntityCatalog(e)
	for _, ids := range [][]int64{nn.InvalidIDs, aa.InvalidIDs, ee.InvalidIDs} {
		if len(ids) != 1 || ids[0] != 2 {
			t.Fatalf("invalid IDs lost: %v", ids)
		}
	}
	if len(nn.Notes) != 1 || nn.Notes[0].ID != 1 || len(aa.Tasks) != 1 || aa.Tasks[0].ID != 1 || len(ee.Entities) != 1 || ee.Entities[0].ID != 1 {
		t.Fatal("healthy items lost")
	}
}
