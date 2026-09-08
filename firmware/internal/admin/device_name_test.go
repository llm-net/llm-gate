package admin_test

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

func TestDeviceNameReadAndUpdate(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	read := func() string {
		t.Helper()
		var snapshot struct {
			DeviceName string `json:"device_name"`
		}
		response := e.do("GET", "/admin/v1/endpoints", root, "")
		wantStatus(t, response, http.StatusOK)
		decodeInto(t, response, &snapshot)
		return snapshot.DeviceName
	}
	if initial := read(); !regexp.MustCompile(`^[a-z0-9]{6}$`).MatchString(initial) {
		t.Fatalf("initial name = %q", initial)
	}
	const name = "书房网关"
	response := e.do("PUT", "/admin/v1/system/device-name", root, `{"name":"  `+name+`  "}`)
	wantStatus(t, response, http.StatusOK)
	var saved struct {
		Name string `json:"name"`
	}
	decodeInto(t, response, &saved)
	if saved.Name != name || read() != name {
		t.Fatal("saved name and endpoint snapshot differ")
	}
	for _, body := range []string{`{}`, `{"name":null}`, `{"name":""}`, `{"name":" \t "}`, `{"name":"one\ntwo"}`, `{"name":"` + strings.Repeat("名", 33) + `"}`} {
		response := e.do("PUT", "/admin/v1/system/device-name", root, body)
		wantStatus(t, response, http.StatusBadRequest)
		if code := errCode(t, response); code != "invalid_device_name" {
			t.Errorf("error code = %q", code)
		}
	}
	if read() != name {
		t.Fatal("invalid update changed device name")
	}
	response = e.do("GET", "/admin/v1/version", "", "")
	wantStatus(t, response, http.StatusOK)
	var public map[string]json.RawMessage
	decodeInto(t, response, &public)
	if _, exists := public["device_name"]; exists {
		t.Fatal("public version exposes private display name")
	}
	if strings.Contains(e.buf.String(), name) {
		t.Fatal("device name written to logs")
	}
}

func TestDeviceNameRequiresSessionAndCSRF(t *testing.T) {
	e := newEnv(t)
	wantStatus(t, e.do("PUT", "/admin/v1/system/device-name", "", `{"name":"room01"}`), http.StatusUnauthorized)
	root := e.rootSession()
	request := e.req("PUT", "/admin/v1/system/device-name", root, `{"name":"room01"}`)
	request.Header.Del("X-LlmGate-CSRF")
	wantStatus(t, e.send(request), http.StatusForbidden)
}
