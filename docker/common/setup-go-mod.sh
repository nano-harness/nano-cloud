#!/bin/sh
set -eu
go mod edit -replace github.com/nano-harness/nano-agent=/src/nano-agent
go mod download
