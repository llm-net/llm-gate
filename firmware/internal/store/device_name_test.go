package store

import (
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
)

func TestDeviceNameInitializedAndPersistent(t *testing.T) {
	s, dir := mustOpen(t)
	initial, err := s.GetSetting(t.Context(), DeviceNameSetting)
	if err != nil || !regexp.MustCompile(`^[a-z0-9]{6}$`).MatchString(initial) {
		t.Fatalf("startup name = %q, error = %v", initial, err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got, err := reopened.DeviceName(t.Context()); err != nil || got != initial {
		t.Fatalf("reopened name = %q, error = %v", got, err)
	}
	name, err := s.SetDeviceName(t.Context(), "  书房网关  ")
	if err != nil || name != "书房网关" {
		t.Fatalf("saved name = %q, error = %v", name, err)
	}
	if got, err := reopened.DeviceName(t.Context()); err != nil || got != name {
		t.Fatalf("other reader name = %q, error = %v", got, err)
	}
}

func TestDeviceNameRepairsEmptyAndConcurrentReadersAgree(t *testing.T) {
	s, _ := mustOpen(t)
	if err := s.SetSetting(t.Context(), DeviceNameSetting, " \t\u3000 "); err != nil {
		t.Fatal(err)
	}
	const readers = 12
	names := make([]string, readers)
	errs := make([]error, readers)
	var wg sync.WaitGroup
	for i := range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			names[i], errs[i] = s.DeviceName(t.Context())
		}()
	}
	wg.Wait()
	for i := range readers {
		if errs[i] != nil || names[i] != names[0] || !regexp.MustCompile(`^[a-z0-9]{6}$`).MatchString(names[i]) {
			t.Fatalf("reader %d name = %q, error = %v", i, names[i], errs[i])
		}
	}
}

func TestDeviceNameValidation(t *testing.T) {
	s, _ := mustOpen(t)
	before, err := s.DeviceName(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{"", " \t\u3000", strings.Repeat("名", 33), "one\ntwo", "one\x00two", "\u200b", "one\u2028two"} {
		if _, err := s.SetDeviceName(t.Context(), input); !errors.Is(err, ErrInvalidDeviceName) {
			t.Errorf("input %q error = %v", input, err)
		}
	}
	if got, err := s.DeviceName(t.Context()); err != nil || got != before {
		t.Fatalf("invalid update changed name to %q: %v", got, err)
	}
	if got, err := s.SetDeviceName(t.Context(), strings.Repeat("名", 32)); err != nil || got != strings.Repeat("名", 32) {
		t.Fatalf("32 Unicode characters rejected: %v", err)
	}
}
