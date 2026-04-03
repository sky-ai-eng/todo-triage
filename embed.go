package main

import (
	"embed"
	"io/fs"
)

//go:embed frontend/dist/*
var frontendFS embed.FS

func frontendDist() (fs.FS, error) {
	return fs.Sub(frontendFS, "frontend/dist")
}
