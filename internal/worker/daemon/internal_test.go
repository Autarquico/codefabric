package daemon

import (
	"errors"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
)

func TestBackoffGrowsFromATenthOfTheIntervalToTheInterval(t *testing.T) {
	d := &daemon{interval: 100 * time.Millisecond}
	var got []time.Duration
	for failures := 1; failures <= 6; failures++ {
		got = append(got, d.backoff(failures))
	}
	want := []time.Duration{10, 20, 40, 80, 100, 100}
	for i := range want {
		if got[i] != want[i]*time.Millisecond {
			t.Errorf("after %d failures: %v, want %v", i+1, got[i], want[i]*time.Millisecond)
		}
	}
}

func TestOnlyAnUnreachableOrFailingControllerIsRetried(t *testing.T) {
	for code, want := range map[domain.Code]bool{
		domain.CodeControllerUnreachable: true, domain.CodeInternal: true,
		domain.CodeUnauthenticated: false, domain.CodePolicyDenied: false, domain.CodeNotFound: false, domain.CodeInvalidRequest: false,
	} {
		if got := retryable(domain.NewError(code, "x", nil)); got != want {
			t.Errorf("retryable(%s) = %t, want %t", code, got, want)
		}
	}
	if retryable(errors.New("not coded")) {
		t.Error("an error without a code was retried")
	}
}

func TestParseSizeMB(t *testing.T) {
	for text, want := range map[string]int{"": 0, "512MB": 512, "16GB": 16384, "16gb": 16384, "2TB": 2 << 20} {
		if got, err := parseSizeMB("--x", text); err != nil || got != want {
			t.Errorf("parseSizeMB(%q) = %d, %v; want %d", text, got, err, want)
		}
	}
	for _, text := range []string{"16", "GB", "0GB", "-1GB", "1.5GB", "16 GB", "99999999TB", "16KB"} {
		if _, err := parseSizeMB("--x", text); err == nil {
			t.Errorf("parseSizeMB(%q) was accepted", text)
		}
	}
}

func TestWorkerName(t *testing.T) {
	for hostname, want := range map[string]string{
		"MacBook-Air.local": "macbook-air", "proxmox": "proxmox", "Alex's Mac": "alex-s-mac",
		"": "worker", "...": "worker", "_x_": "x", "build_01.corp.example": "build_01",
	} {
		if got := workerName(hostname); got != want {
			t.Errorf("workerName(%q) = %q, want %q", hostname, got, want)
		}
	}
}
