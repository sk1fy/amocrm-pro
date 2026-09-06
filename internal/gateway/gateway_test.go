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
