//go:build tools

// Package main pins the ent code generator so `go run entgo.io/ent/cmd/ent
// generate ./internal/ent/schema` works without pulling it into the binary.
package main

import _ "entgo.io/ent/cmd/ent"
