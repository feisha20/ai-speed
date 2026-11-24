package web

import "embed"

// Assets embeds the single-page web UI static files directly into the binary.
//
//go:embed index.html style.css app.js theme-init.js
var Assets embed.FS
