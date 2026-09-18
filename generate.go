package main

// Code generation. `go generate ./...` regenerates api/ from the spec;
// CI runs the same command and fails if the result differs from what is
// committed, so a spec change cannot ship without the code that matches.
//
//go:generate go run github.com/ogen-go/ogen/cmd/ogen@latest --config ogen.yml --target api --clean --package api openapi.yml
