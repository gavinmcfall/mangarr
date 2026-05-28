.PHONY: build test run tidy
build:
	CGO_ENABLED=0 go build -o bin/mangarr .
test:
	go test ./...
run:
	go run .
tidy:
	go mod tidy
