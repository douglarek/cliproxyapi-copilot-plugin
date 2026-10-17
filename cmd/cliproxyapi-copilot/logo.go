package main

import (
	_ "embed"
	"encoding/base64"
)

//go:embed logo.svg
var logoSVG []byte

// pluginLogo is base64-encoded because the management API HTML-escapes plugin
// metadata strings, which would corrupt the markup of a raw SVG data URI.
var pluginLogo = "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString(logoSVG)
