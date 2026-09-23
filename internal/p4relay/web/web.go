package web

import "embed"

//go:embed public/index.html public/app.js public/style.css public/icon.png public/logo.png public/fournisseurs
var FS embed.FS
