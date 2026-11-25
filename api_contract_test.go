package kvm

import "testing"

// An MCP server drives a device through POST /api/rpc and GET /screenshot.jpg.
// These are the methods and routes it names; a rebase onto upstream must not
// lose one silently.
func TestHTTPAPIKeepsWhatTheMCPServerCalls(t *testing.T) {
	for _, method := range []string{
		"getDeviceID", "getVideoState", "reboot",
		"keyboardReport", "absMouseReport", "wheelReport",
		"getKeyboardMacros",
		"getJigglerState", "setJigglerState", "getJigglerConfig", "setJigglerConfig",
	} {
		if _, ok := rpcHandlers[method]; !ok {
			t.Errorf("JSON-RPC method %q is not in the handler table", method)
		}
	}
	config = &Config{LocalAuthMode: "noPassword"}
	routes := map[string]bool{}
	for _, r := range setupRouter().Routes() {
		routes[r.Method+" "+r.Path] = true
	}
	for _, route := range []string{"POST /api/rpc", "GET /screenshot.jpg", "POST /auth/login-local"} {
		if !routes[route] {
			t.Errorf("route %q is not registered", route)
		}
	}
}
