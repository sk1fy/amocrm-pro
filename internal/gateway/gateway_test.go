package gateway

import (
	"context"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
	"strings"
	"testing"
)

type directoryPolicy struct {
	serviceapi.Policy
	scope serviceapi.Scope
}

func (p directoryPolicy) Validate(context.Context, serviceapi.Auth, string, string) (serviceapi.Principal, error) {
	return serviceapi.Principal{Scope: p.scope}, nil
}

type directoryAPI struct {
	API
	directory amocrm.AccountDirectory
	calls     int
}

func (a *directoryAPI) GetDirectory(context.Context, uuid.UUID) (amocrm.AccountDirectory, error) {
	a.calls++
	return a.directory, nil
}

func TestOversizedDirectoryIsRejectedBeforeCaching(t *testing.T) {
	large := amocrm.AccountDirectory{Timezone: "Europe/Moscow"}
	for i := 0; i < 1000; i++ {
		large.Users = append(large.Users, amocrm.DirectoryUser{ID: int64(i + 1), Name: strings.Repeat("n", 512), GroupName: strings.Repeat("g", 256)})
	}
	for name, directory := range map[string]amocrm.AccountDirectory{"name": {Users: []amocrm.DirectoryUser{{ID: 1, Name: strings.Repeat("n", 513)}}}, "group": {Users: []amocrm.DirectoryUser{{ID: 1, GroupName: strings.Repeat("g", 257)}}}, "total": large} {
		t.Run(name, func(t *testing.T) {
			api := &directoryAPI{directory: directory}
			scope := serviceapi.Scope{IntegrationID: uuid.New(), InstallationID: uuid.New()}
			service := New(api, directoryPolicy{scope: scope})
			request := serviceapi.UsersRequest{UserIDs: []int64{1}}
			if _, err := service.Users(context.Background(), request); serviceapi.ErrorCode(err) != serviceapi.ResourceExhausted {
				t.Fatalf("oversized directory=%v", err)
			}
			if len(service.directory) != 0 {
				t.Fatal("oversized metadata entered cache")
			}
			api.directory = amocrm.AccountDirectory{Users: []amocrm.DirectoryUser{{ID: 1, Name: "Small"}}}
			result, err := service.Users(context.Background(), request)
			if err != nil || len(result.Users) != 1 || api.calls != 2 {
				t.Fatalf("recovery=%+v calls%d err%v", result, api.calls, err)
			}
		})
	}
}

type scopedDirectoryAPI struct {
	API
	byID map[uuid.UUID]amocrm.AccountDirectory
}

func (a scopedDirectoryAPI) GetDirectory(_ context.Context, id uuid.UUID) (amocrm.AccountDirectory, error) {
	return a.byID[id], nil
}

type scopedDirectoryPolicy struct {
	serviceapi.Policy
	byToken map[string]serviceapi.Scope
}

func (p scopedDirectoryPolicy) Validate(_ context.Context, a serviceapi.Auth, _, _ string) (serviceapi.Principal, error) {
	scope, ok := p.byToken[a.Token]
	if !ok {
		return serviceapi.Principal{}, serviceapi.Fail(serviceapi.PermissionDenied, "denied")
	}
	return serviceapi.Principal{Scope: scope}, nil
}
func TestDirectoryCRMOriginCacheIsScopedToInstallation(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	api := scopedDirectoryAPI{byID: map[uuid.UUID]amocrm.AccountDirectory{a: {CRMBaseURL: "https://alpha.amocrm.ru"}, b: {CRMBaseURL: "https://bravo.kommo.com"}}}
	policy := scopedDirectoryPolicy{byToken: map[string]serviceapi.Scope{"a": {InstallationID: a}, "b": {InstallationID: b}}}
	s := New(api, policy)
	for _, token := range []string{"a", "b", "a", "b"} {
		got, err := s.Users(context.Background(), serviceapi.UsersRequest{Auth: serviceapi.Auth{Token: token}})
		if err != nil || got.CRMBaseURL != api.byID[policy.byToken[token].InstallationID].CRMBaseURL {
			t.Fatalf("scope leak for %s: %+v %v", token, got, err)
		}
	}
	if _, err := s.Users(context.Background(), serviceapi.UsersRequest{Auth: serviceapi.Auth{Token: "revoked"}}); serviceapi.ErrorCode(err) != serviceapi.PermissionDenied {
		t.Fatal("cache bypassed authorization")
	}
}
