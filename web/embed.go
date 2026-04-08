package web

import "embed"

// FS holds static assets under web/pikvm/ (CSS, JS, etc.).
//
//go:embed pikvm
var FS embed.FS
