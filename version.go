package main

// Build-time variables populated via ldflags:
//
//	go build -ldflags "-X main.version=0.1.0 -X main.commit=$(git rev-parse HEAD) -X main.date=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
var (
	Name    = "mrsh"
	version = "dev"
	commit  = ""
	date    = ""
	builtBy = ""
)
