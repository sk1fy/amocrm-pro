package services

import "testing"

func TestTaskControlCapabilityDoesNotAddServerExecutionJobs(t *testing.T) {
	if !Known(TaskControl) {
		t.Fatal("operator provisioning must accept Task Control")
	}
	for job, service := range JobTypes() {
		if service == TaskControl {
			t.Fatalf("browser-owned Task Control unexpectedly executes job %s", job)
		}
	}
	if Known("task-control-other") {
		t.Fatal("unknown capability accepted")
	}
}
