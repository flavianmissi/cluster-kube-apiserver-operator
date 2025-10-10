package kmsplugin

import (
	"embed"
)

//go:embed assets/*
var f embed.FS

// asset reads and returns the content of the named file.
func asset(name string) ([]byte, error) {
	return f.ReadFile(name)
}
