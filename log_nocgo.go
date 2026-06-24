//go:build !cgo

package main

func pluginLog(hostCallbackID string, level string, message string, fields map[string]any) {}
