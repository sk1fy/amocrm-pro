package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const panelUsage = "usage: activity-control panel-list|panel-create|panel-get|panel-patch|panel-rotate|panel-employees INSTALLATION_UUID INTEGRATION_UUID ..."

func runPanel(args []string) error {
	if len(args) == 0 {
		return errors.New(panelUsage)
	}
	base := strings.TrimRight(os.Getenv("API_BASE_URL"), "/")
	token := os.Getenv("ACTIVITY_MANAGEMENT_TOKEN")
	if base == "" || token == "" {
		return fmt.Errorf("API_BASE_URL and ACTIVITY_MANAGEMENT_TOKEN are required")
	}
	command := args[0]
	args = args[1:]
	switch command {
	case "panel-list", "panel-employees":
		if len(args) != 2 {
			return errors.New(panelUsage)
		}
		installation, integration, err := parseScope(args[0], args[1])
		if err != nil {
			return err
		}
		path := "/api/v1/activity/panels"
		if command == "panel-employees" {
			path = "/api/v1/activity/employees"
		}
		return panelRequest(base, token, http.MethodGet, path, installation, integration, "", nil)
	case "panel-create":
		if len(args) < 2 {
			return errors.New("usage: activity-control panel-create INSTALLATION_UUID INTEGRATION_UUID --name NAME --employees 7,9 --from 09:00 --to 18:00")
		}
		installation, integration, err := parseScope(args[0], args[1])
		if err != nil {
			return err
		}
		fs := flag.NewFlagSet("panel-create", flag.ContinueOnError)
		name := fs.String("name", "", "")
		employees := fs.String("employees", "", "")
		from := fs.String("from", "", "")
		to := fs.String("to", "", "")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if fs.NArg() != 0 || *name == "" || *employees == "" || *from == "" || *to == "" {
			return errors.New("usage: activity-control panel-create INSTALLATION_UUID INTEGRATION_UUID --name NAME --employees 7,9 --from 09:00 --to 18:00")
		}
		ids, err := parseEmployeeList(*employees)
		if err != nil {
			return err
		}
		body, _ := json.Marshal(map[string]any{"name": *name, "employee_ids": ids, "display_window": map[string]string{"from": *from, "to": *to}})
		return panelRequest(base, token, http.MethodPost, "/api/v1/activity/panels", installation, integration, uuid.NewString(), body)
	case "panel-get", "panel-rotate":
		if len(args) != 3 {
			return errors.New(panelUsage)
		}
		installation, integration, err := parseScope(args[0], args[1])
		if err != nil {
			return err
		}
		panelID, err := uuid.Parse(args[2])
		if err != nil {
			return fmt.Errorf("a valid UUID is required")
		}
		if command == "panel-get" {
			return panelRequest(base, token, http.MethodGet, "/api/v1/activity/panels/"+panelID.String(), installation, integration, "", nil)
		}
		return panelRequest(base, token, http.MethodPost, "/api/v1/activity/panels/"+panelID.String()+"/share-link", installation, integration, uuid.NewString(), nil)
	case "panel-patch":
		if len(args) < 3 {
			return errors.New("usage: activity-control panel-patch INSTALLATION_UUID INTEGRATION_UUID PANEL_UUID --revision N [--name ...] [--employees ...] [--enabled true|false]")
		}
		installation, integration, err := parseScope(args[0], args[1])
		if err != nil {
			return err
		}
		panelID, err := uuid.Parse(args[2])
		if err != nil {
			return fmt.Errorf("a valid UUID is required")
		}
		fs := flag.NewFlagSet("panel-patch", flag.ContinueOnError)
		revision := fs.Int64("revision", 0, "")
		name := fs.String("name", "", "")
		employees := fs.String("employees", "", "")
		enabled := fs.String("enabled", "", "")
		if err := fs.Parse(args[3:]); err != nil {
			return err
		}
		if fs.NArg() != 0 || *revision < 1 {
			return errors.New("usage: activity-control panel-patch INSTALLATION_UUID INTEGRATION_UUID PANEL_UUID --revision N [--name ...] [--employees ...] [--enabled true|false]")
		}
		payload := map[string]any{"revision": *revision}
		if *name != "" {
			payload["name"] = *name
		}
		if *employees != "" {
			ids, err := parseEmployeeList(*employees)
			if err != nil {
				return err
			}
			payload["employee_ids"] = ids
		}
		if *enabled != "" {
			value, err := strconv.ParseBool(*enabled)
			if err != nil {
				return fmt.Errorf("enabled must be true or false")
			}
			payload["enabled"] = value
		}
		body, _ := json.Marshal(payload)
		return panelRequest(base, token, http.MethodPatch, "/api/v1/activity/panels/"+panelID.String(), installation, integration, "", body)
	default:
		return errors.New("unknown operator command")
	}
}

func parseScope(installation, integration string) (uuid.UUID, uuid.UUID, error) {
	inst, err := uuid.Parse(installation)
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("a valid UUID is required")
	}
	integ, err := uuid.Parse(integration)
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("a valid UUID is required")
	}
	return inst, integ, nil
}

func parseEmployeeList(raw string) ([]int64, error) {
	var ids []int64
	for _, part := range strings.Split(raw, ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("employees must be distinct positive IDs")
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func panelRequest(base, token, method, path string, installation, integration uuid.UUID, idempotency string, body []byte) error {
	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, base+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Activity-Installation-Id", installation.String())
	req.Header.Set("X-Activity-Integration-Id", integration.String())
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if idempotency != "" {
		req.Header.Set("Idempotency-Key", idempotency)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("management API %s", strings.TrimSpace(string(payload)))
	}
	if len(bytes.TrimSpace(payload)) == 0 {
		return nil
	}
	os.Stdout.Write(payload)
	if payload[len(payload)-1] != '\n' {
		fmt.Println()
	}
	return nil
}
