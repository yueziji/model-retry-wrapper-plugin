//go:build cgo

package main

import "github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"

func pluginLog(hostCallbackID string, level string, message string, fields map[string]any) {
	defer func() {
		_ = recover()
	}()
	if fields == nil {
		fields = map[string]any{}
	}
	fields["plugin"] = pluginIdentifier
	_, _ = callHost(pluginabi.MethodHostLog, rpcHostLogRequest{
		HostCallbackID: hostCallbackID,
		Level:          level,
		Message:        message,
		Fields:         fields,
	})
}
