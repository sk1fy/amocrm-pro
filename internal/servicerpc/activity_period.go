package servicerpc

import (
	"context"
	"encoding/json"

	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"github.com/sk1fy/amocrm-pro/internal/servicerpc/pb"
)

// period-v1 conversions are hand-written (generate-adapters.py skips them):
// requests are typed, views travel as their public JSON document.

func toActivityPeriodRequest(src serviceapi.ActivityPeriodRequest) *pb.ActivityPeriodRequest {
	return &pb.ActivityPeriodRequest{Auth: toAuth(src.Auth), Kind: src.Kind, Anchor: src.Anchor, StartMinutes: boundedInt32(src.StartMinutes), EndMinutes: boundedInt32(src.EndMinutes), CutoffUnix: src.CutoffUnix, EmployeeIds: append([]int64(nil), src.EmployeeIDs...)}
}

func fromActivityPeriodRequest(src *pb.ActivityPeriodRequest) serviceapi.ActivityPeriodRequest {
	if src == nil {
		return serviceapi.ActivityPeriodRequest{}
	}
	return serviceapi.ActivityPeriodRequest{Auth: fromAuth(src.Auth), Kind: src.Kind, Anchor: src.Anchor, StartMinutes: int(src.StartMinutes), EndMinutes: int(src.EndMinutes), CutoffUnix: src.CutoffUnix, EmployeeIDs: append([]int64(nil), src.EmployeeIds...)}
}

func toActivityPeriodDaysRequest(src serviceapi.ActivityPeriodDaysRequest) *pb.ActivityPeriodDaysRequest {
	return &pb.ActivityPeriodDaysRequest{Auth: toAuth(src.Auth), EmployeeId: src.EmployeeID, FromDay: src.FromDay, ToDay: src.ToDay, StartMinutes: boundedInt32(src.StartMinutes), EndMinutes: boundedInt32(src.EndMinutes), CutoffUnix: src.CutoffUnix}
}

func fromActivityPeriodDaysRequest(src *pb.ActivityPeriodDaysRequest) serviceapi.ActivityPeriodDaysRequest {
	if src == nil {
		return serviceapi.ActivityPeriodDaysRequest{}
	}
	return serviceapi.ActivityPeriodDaysRequest{Auth: fromAuth(src.Auth), EmployeeID: src.EmployeeId, FromDay: src.FromDay, ToDay: src.ToDay, StartMinutes: int(src.StartMinutes), EndMinutes: int(src.EndMinutes), CutoffUnix: src.CutoffUnix}
}

func decodePeriodView[T any](raw []byte) (T, error) {
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, serviceapi.Fail(serviceapi.Unavailable, "invalid Activity period response")
	}
	return out, nil
}

func (s *activityServer) ViewActivityPeriod(ctx context.Context, r *pb.ActivityPeriodRequest) (*pb.ActivityPeriodView, error) {
	reader, ok := s.impl.(serviceapi.ActivityPeriodReader)
	if !ok {
		return nil, serviceapi.Fail(serviceapi.Unavailable, "Activity period service unavailable")
	}
	v, err := reader.ViewActivityPeriod(ctx, fromActivityPeriodRequest(r))
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, serviceapi.Fail(serviceapi.Internal, "invalid Activity period response")
	}
	return &pb.ActivityPeriodView{Json: raw}, nil
}

func (c activityClient) ViewActivityPeriod(ctx context.Context, r serviceapi.ActivityPeriodRequest) (serviceapi.ActivityPeriod, error) {
	v, err := c.remote.ViewActivityPeriod(ctx, toActivityPeriodRequest(r))
	if err != nil {
		return serviceapi.ActivityPeriod{}, err
	}
	return decodePeriodView[serviceapi.ActivityPeriod](v.GetJson())
}

func (s *activityServer) ViewActivityPeriodDays(ctx context.Context, r *pb.ActivityPeriodDaysRequest) (*pb.ActivityPeriodDaysView, error) {
	reader, ok := s.impl.(serviceapi.ActivityPeriodReader)
	if !ok {
		return nil, serviceapi.Fail(serviceapi.Unavailable, "Activity period service unavailable")
	}
	v, err := reader.ViewActivityPeriodDays(ctx, fromActivityPeriodDaysRequest(r))
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, serviceapi.Fail(serviceapi.Internal, "invalid Activity period response")
	}
	return &pb.ActivityPeriodDaysView{Json: raw}, nil
}

func (c activityClient) ViewActivityPeriodDays(ctx context.Context, r serviceapi.ActivityPeriodDaysRequest) (serviceapi.ActivityPeriodDays, error) {
	v, err := c.remote.ViewActivityPeriodDays(ctx, toActivityPeriodDaysRequest(r))
	if err != nil {
		return serviceapi.ActivityPeriodDays{}, err
	}
	return decodePeriodView[serviceapi.ActivityPeriodDays](v.GetJson())
}
