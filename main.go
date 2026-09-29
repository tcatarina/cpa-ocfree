// Command cpa-plugin-ocfree is a CLIProxyAPI plugin that satisfies the
// OpenCode Zen free-tier request contract, so the keyless models there can be
// served through an ordinary openai-compatibility provider.
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"encoding/json"
	"net/http"
	"os"
	"sync/atomic"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	pluginName    = "ocfree"
	resourcePath  = "/ocfree"
	defaultUserAg = defaultUserAgent
)

var (
	pluginVersion = "dev"
	hostPtr       atomic.Pointer[C.cliproxy_host_api]
	traceOn       = os.Getenv("OCFREE_TRACE") != ""
	state         = &configState{cfg: defaultConfig()}
)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

type hostEnvelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type hostError struct {
	msg    string
	status int
	code   string
}

func (e *hostError) Error() string { return e.msg }

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	RequestInterceptor  bool `json:"request_interceptor"`
	ResponseInterceptor bool `json:"response_interceptor"`
	ManagementAPI       bool `json:"management_api"`
}

type rpcManagementRequest struct {
	pluginapi.ManagementRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcPluginLifecycle struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	hostPtr.Store(host)
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, err := handleMethod(C.GoString(method), requestBytes)
	if err != nil {
		writeResponse(response, errorEnvelope("plugin_error", err.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	buf := C.CBytes(raw)
	response.ptr = buf
	response.len = C.size_t(len(raw))
}

func handleMethod(method string, request []byte) ([]byte, error) {
	if traceOn {
		_, _ = callHost(pluginabi.MethodHostLog, map[string]any{
			"level":   "debug",
			"message": "ocfree: dispatch " + method + " payload=" + itoa(len(request)),
		})
	}
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if err := applyConfig(request); err != nil {
			return nil, err
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodRequestInterceptBefore, pluginabi.MethodRequestInterceptAfter:
		return handleInterceptBefore(request)
	case pluginabi.MethodResponseInterceptAfter:
		return handleInterceptAfter(request)
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRouteSetResponse())
	case pluginabi.MethodManagementHandle:
		return handleManagement(request)
	default:
		return errorEnvelope("unsupported_method", "method not implemented: "+method), nil
	}
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           "tcatarina",
			GitHubRepository: "https://github.com/tcatarina/cpa-ocfree",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Inject the free-tier request contract."},
				{Name: "user_agent", Type: pluginapi.ConfigFieldTypeString, Description: "User-Agent sent upstream. Must be opencode/1.17 or newer."},
				{Name: "session_salt", Type: pluginapi.ConfigFieldTypeString, Description: "Salt for the deterministic per-conversation session id."},
			},
		},
		Capabilities: registrationCapability{
			RequestInterceptor:  true,
			ResponseInterceptor: true,
			ManagementAPI:       true,
		},
	}
}

func callHost(method string, payload any) (json.RawMessage, error) {
	host := hostPtr.Load()
	if host == nil {
		return nil, &hostError{msg: "host API unavailable", code: method}
	}
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var requestPtr *C.uint8_t
	if len(rawPayload) > 0 {
		requestPtr = (*C.uint8_t)(unsafe.Pointer(&rawPayload[0]))
	}
	var out C.cliproxy_buffer
	rc := C.call_host_api(cMethod, requestPtr, C.size_t(len(rawPayload)), &out)
	if rc != 0 {
		return nil, &hostError{msg: method + " failed", code: method}
	}
	if out.ptr == nil || out.len == 0 {
		return nil, nil
	}
	defer C.free_host_buffer(out.ptr, out.len)
	return C.GoBytes(out.ptr, C.int(out.len)), nil
}

func okEnvelope(result any) ([]byte, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return errorEnvelope("marshal_failed", err.Error()), nil
	}
	out, err := json.Marshal(envelope{OK: true, Result: raw})
	if err != nil {
		return errorEnvelope("marshal_failed", err.Error()), nil
	}
	return out, nil
}

func errorEnvelope(code, message string) []byte {
	out, err := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	if err != nil {
		return []byte(`{"ok":false,"error":{"code":"internal_error","message":"envelope marshal failed"}}`)
	}
	return out
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func logInfo(message string) {
	if !traceOn {
		return
	}
	_, _ = callHost(pluginabi.MethodHostLog, map[string]any{
		"level": "info", "message": "ocfree: " + message,
	})
}

func logWarn(message string) {
	_, _ = callHost(pluginabi.MethodHostLog, map[string]any{
		"level": "warn", "message": "ocfree: " + message,
	})
}

var _ = http.Header{}
