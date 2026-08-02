package main

import "testing"

func TestUpdateStatus(t *testing.T) {
	vib := func(v float64) *float64 { return &v }

	cases := []struct {
		name       string
		temp, load float64
		vibration  *float64
		wantStatus MachineStatus
	}{
		{"high temperature -> error", 96, 50, vib(2), StatusError},
		{"high vibration -> error", 60, 50, vib(7.1), StatusError},
		{"error takes priority over warning-level load", 96, 90, vib(2), StatusError},
		{"elevated temperature -> warning", 85, 50, vib(2), StatusWarning},
		{"elevated load -> warning", 60, 90, vib(2), StatusWarning},
		{"elevated vibration -> warning", 60, 50, vib(5), StatusWarning},
		{"low load -> idle", 60, 4, vib(0.1), StatusIdle},
		{"nominal -> running", 60, 50, vib(2), StatusRunning},
		{"nil vibration treated as zero", 60, 50, nil, StatusRunning},
		{"boundary temp 80 is not yet a warning", 80, 50, vib(2), StatusRunning},
		{"boundary temp 80.1 is a warning", 80.1, 50, vib(2), StatusWarning},
	}

	s := &Store{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := &Machine{
				Metrics: MachineMetrics{
					Temperature: c.temp,
					Load:        c.load,
					Vibration:   c.vibration,
				},
			}
			s.updateStatus(m)
			if m.Status != c.wantStatus {
				t.Errorf("updateStatus() = %v, want %v", m.Status, c.wantStatus)
			}
		})
	}
}
