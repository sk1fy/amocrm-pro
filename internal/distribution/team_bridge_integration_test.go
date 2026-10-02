package distribution

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/sk1fy/amocrm-pro/internal/integration/amocrm"
	"github.com/sk1fy/amocrm-pro/internal/jobs"
)

// This opt-in test process supplies the actual Core private API/store/worker to
// TeamOS's PostgreSQL bridge acceptance test. Only amoCRM is controlled here;
// TeamOS's production signed callback and decision registry provide permissions.
// Run both test processes in the same Docker network namespace, against separate
// disposable databases; the files are test coordination, never runtime routes.
func TestDistributionTeamBridgeServer(t *testing.T) {
	dir := os.Getenv("DISTRIBUTION_BRIDGE_DIR")
	if dir == "" {
		t.Skip("opt-in cross-repository bridge; set DISTRIBUTION_BRIDGE_DIR")
	}
	ctx := context.Background()
	f := assignmentFixture(t)
	secret := strings.Repeat("bridge-test-only-", 3)
	var employee2 uuid.UUID
	if err := f.Pool.QueryRow(ctx, `SELECT employee_id FROM distribution_actor_mappings WHERE binding_id=$1 AND user_id=2`, f.Assignment.Scope.BindingID).Scan(&employee2); err != nil {
		t.Fatal(err)
	}
	employee3 := uuid.New()
	if _, err := f.Pool.Exec(ctx, `INSERT INTO distribution_actor_mappings(binding_id,employee_id,user_id) VALUES($1,$2,3)`, f.Assignment.Scope.BindingID, employee3); err != nil {
		t.Fatal(err)
	}
	f.CRM.users[3] = amocrm.DistributionUser{ID: 3, Name: "Bridge C", Rights: amocrm.DistributionRights{IsActive: ptr(true), Leads: map[string]string{"view": "A"}}}
	f.CRM.pipelines = []amocrm.Pipeline{{ID: 20, Name: "Bridge pipeline", Statuses: []amocrm.PipelineStatus{{ID: 30, Name: "Distribution"}, {ID: 31, Name: "Exit"}}}}
	crm := &teamBridgeCRM{assignmentFakeCRM: f.CRM, leads: map[int64]amocrm.LeadState{10: f.CRM.Lead}}
	h := &Handler{Store: f.Store, CRM: crm, TeamOSKeyID: f.Scope.KeyID, Keys: map[string]string{f.Scope.KeyID: secret}}
	router := chi.NewRouter()
	h.RegisterService(router, Auth{Keys: h.Keys, Store: f.Store})
	server := httptest.NewUnstartedServer(router)
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = server.Listener.Close()
	server.Listener = listener
	server.StartTLS()
	defer server.Close()
	url := fmt.Sprintf("https://127.0.0.1:%d", listener.Addr().(*net.TCPAddr).Port)
	cert := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
	if err := os.WriteFile(filepath.Join(dir, "core-ca.pem"), []byte(cert), 0600); err != nil {
		t.Fatal(err)
	}
	writeBridgeFile(t, dir, "core.json", map[string]any{"url": url, "certificatePEM": cert, "scope": f.Assignment.Scope, "employeeId": employee2, "employee3Id": employee3, "secret": secret, "keyId": f.Scope.KeyID})
	deadline := time.Now().Add(10 * time.Minute)
	for {
		raw, err := os.ReadFile(filepath.Join(dir, "team.json"))
		if err == nil {
			var team struct {
				URL         string `json:"url"`
				Certificate string `json:"certificatePEM"`
			}
			if json.Unmarshal(raw, &team) != nil || team.URL == "" {
				t.Fatal("invalid Team bridge readiness")
			}
			roots := x509.NewCertPool()
			if !roots.AppendCertsFromPEM([]byte(team.Certificate)) {
				t.Fatal("invalid Team test TLS certificate")
			}
			h.TeamOSURL = team.URL
			h.HTTP = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}, Timeout: 3 * time.Second}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Team bridge did not become ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	writeBridgeFile(t, dir, "connected.json", map[string]any{"ready": true})
	worker := &AssignmentWorker{Store: f.Store, CRM: crm, Policy: h}
	for sequence := 1; ; sequence++ {
		var command struct {
			Action            string    `json:"action"`
			LeadID            int64     `json:"leadId"`
			PipelineID        int64     `json:"pipelineId"`
			StatusID          int64     `json:"statusId"`
			ResponsibleUserID int64     `json:"responsibleUserId"`
			UpdatedAt         int64     `json:"updatedAt"`
			Mode              string    `json:"mode"`
			UserID            int64     `json:"userId"`
			Active            bool      `json:"active"`
			OperationID       uuid.UUID `json:"operationId"`
			Milliseconds      int       `json:"milliseconds"`
		}
		for {
			raw, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("ctl-%d.json", sequence)))
			if err == nil {
				if json.Unmarshal(raw, &command) != nil {
					t.Fatal("invalid bridge control")
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("Team bridge test timed out")
			}
			time.Sleep(20 * time.Millisecond)
		}
		result := map[string]any{}
		switch command.Action {
		case "stop":
			writeBridgeFile(t, dir, fmt.Sprintf("reply-%d.json", sequence), map[string]any{"stopped": true})
			return
		case "crm":
			crm.mu.Lock()
			crm.leads[command.LeadID] = amocrm.LeadState{ID: command.LeadID, PipelineID: command.PipelineID, StatusID: command.StatusID, ResponsibleUserID: command.ResponsibleUserID, UpdatedAt: command.UpdatedAt}
			crm.Mode = command.Mode
			crm.observationUnavailable = false
			crm.mu.Unlock()
		case "recipient":
			crm.mu.Lock()
			user := crm.users[command.UserID]
			user.Rights.IsActive = ptr(command.Active)
			crm.users[command.UserID] = user
			crm.mu.Unlock()
		case "guard_hold":
			if command.Milliseconds < 1 || command.Milliseconds > 6000 {
				t.Fatal("invalid bridge lock duration")
			}
			tx, err := f.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `SELECT operation_id FROM distribution_lead_guards WHERE operation_id=$1 FOR UPDATE`, command.OperationID); err != nil {
				_ = tx.Rollback(ctx)
				t.Fatal(err)
			}
			go func() {
				time.Sleep(time.Duration(command.Milliseconds) * time.Millisecond)
				_ = tx.Rollback(context.Background())
			}()
		case "job_ready":
			if _, err := f.Pool.Exec(ctx, `UPDATE jobs SET run_after=clock_timestamp() WHERE status='retry' AND type=$1`, AssignmentJobType); err != nil {
				t.Fatal(err)
			}
		case "tick":
			claimed, err := f.Jobs.Claim(ctx, "team-bridge", 1, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if len(claimed) > 0 {
				job := claimed[0]
				raw, err := worker.Handler(ctx, job)
				if err != nil {
					result["error"] = err.Error()
					if _, failErr := f.Jobs.FailWithObserver(ctx, job, "team-bridge", jobs.Classify(err, job.Attempts), 0, f.Store.AssignmentFailure); failErr != nil {
						t.Fatal(failErr)
					}
				} else {
					var operation Operation
					if err := json.Unmarshal(raw, &operation); err != nil {
						t.Fatal(err)
					}
					result["operation"] = operation
					if err := f.Jobs.Complete(ctx, job, "team-bridge", raw, 0); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				result["noJob"] = true
			}
		default:
			t.Fatal("unknown bridge control", command.Action)
		}
		crm.mu.Lock()
		result["patchCalls"] = crm.Calls
		crm.mu.Unlock()
		writeBridgeFile(t, dir, fmt.Sprintf("reply-%d.json", sequence), result)
	}
}

func writeBridgeFile(t *testing.T, dir, name string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path+".tmp", raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		t.Fatal(err)
	}
}

type teamBridgeCRM struct {
	*assignmentFakeCRM
	leads                  map[int64]amocrm.LeadState
	observationUnavailable bool
}

func (c *teamBridgeCRM) DistributionUser(_ context.Context, _ uuid.UUID, id int64) (amocrm.DistributionUser, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.users[id], nil
}

func (c *teamBridgeCRM) DistributionUsers(context.Context, uuid.UUID) ([]amocrm.DistributionUser, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	users := make([]amocrm.DistributionUser, 0, len(c.users))
	for _, user := range c.users {
		users = append(users, user)
	}
	return users, nil
}

func (c *teamBridgeCRM) GetLeadSnapshot(_ context.Context, _ uuid.UUID, id int64) (amocrm.LeadState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.observationUnavailable {
		return amocrm.LeadState{}, ErrUnavailable
	}
	lead, ok := c.leads[id]
	if !ok {
		return amocrm.LeadState{}, amocrm.ErrLeadAbsent
	}
	return lead, nil
}
func (c *teamBridgeCRM) PrepareLeadResponsible(context.Context, uuid.UUID) (amocrm.LeadResponsibleMutation, error) {
	return c, nil
}
func (c *teamBridgeCRM) Assign(_ context.Context, id, target int64) (amocrm.LeadResponsibleResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Calls++
	if c.Mode == "timeout_unapplied" {
		return amocrm.LeadResponsibleResult{}, &amocrm.ResponsibleDispatchError{Dispatched: true, Cause: context.DeadlineExceeded}
	}
	lead := c.leads[id]
	lead.ResponsibleUserID = target
	lead.UpdatedAt++
	c.leads[id] = lead
	if c.Mode == "observe_failure" {
		c.observationUnavailable = true
	}
	if c.Mode == "timeout_applied" {
		return amocrm.LeadResponsibleResult{}, &amocrm.ResponsibleDispatchError{Dispatched: true, Cause: context.DeadlineExceeded}
	}
	return amocrm.LeadResponsibleResult{HTTPStatus: 200, Accepted: true, UpdatedAt: lead.UpdatedAt}, nil
}
