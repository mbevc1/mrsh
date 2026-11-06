package config

import _ "embed"

// Example is the annotated reference config printed by `mrsh hosts example`.
//
//go:embed example.yaml
var Example []byte
